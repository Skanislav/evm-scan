/**
 * Ingest, against a store in memory.
 *
 * The interesting cases are the ugly ones: an ingest that ran twice, an ingest
 * killed between writing rows and marking the epoch done, a document that does not
 * match the root it claims to be. Each of them, done wrong, produces a mirror that
 * fails verification for a reason the reader would misdiagnose as a dishonest
 * publisher.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

import { ingestSnapshot, previousEpoch } from "../src/ingest.js";
import { normalizeHex, type Hex } from "../src/merkle.js";
import { parseSnapshot, rootsOf, type Leaf } from "../src/snapshot.js";
import { MemoryStore } from "../src/store.js";
import { resolveAsOf } from "../src/state.js";
import { verifyAsOf, type Commitment } from "../src/verify.js";

const testdata = (name: string): string =>
  readFileSync(fileURLToPath(new URL(`../testdata/${name}`, import.meta.url)), "utf8");

const CHAIN = 8453n;
const account = (n: number): Hex => `0x${n.toString(16).padStart(40, "0")}`;
const asset = (n: number): Hex => `0x${n.toString(16).padStart(40, "0")}`;

/** Builds a snapshot document the way the Go writer does. */
function document(chainId: bigint, coverage: { asset: Hex; from: bigint; to: bigint }[], leaves: Leaf[]): string {
  const roots = rootsOf(
    chainId,
    coverage.map((c) => ({ asset: c.asset, fromBlock: c.from, toBlock: c.to })),
    leaves,
  );
  const header = {
    format: "evmscan-snapshot/1",
    chain_id: Number(chainId),
    from_block: 1,
    to_block: 900,
    root: roots.root,
    coverage_root: roots.coverageRoot,
    leaf_count: leaves.length,
    asset_count: coverage.length,
  };
  const lines = [JSON.stringify(header)];
  for (const c of coverage) {
    lines.push(
      JSON.stringify({
        t: "coverage",
        asset: c.asset,
        from_block: Number(c.from),
        to_block: Number(c.to),
      }),
    );
  }
  for (const l of leaves) {
    lines.push(JSON.stringify({ t: "leaf", account: l.account, assets: l.assets }));
  }
  return lines.join("\n");
}

function commitmentFor(doc: string, epochId: bigint): Commitment {
  const snapshot = parseSnapshot(doc);
  return {
    epochId,
    chainId: snapshot.header.chainId,
    root: snapshot.root,
    coverageRoot: snapshot.coverageRoot,
  };
}

const COVERAGE = [
  { asset: asset(0xa1), from: 100n, to: 900n },
  { asset: asset(0xb2), from: 150n, to: 900n },
];

const epoch4 = document(CHAIN, COVERAGE, [
  { account: account(1), assets: [asset(0xa1)] },
  { account: account(2), assets: [asset(0xa1)] },
]);
const epoch7 = document(CHAIN, COVERAGE, [
  { account: account(1), assets: [asset(0xa1)] },
  { account: account(2), assets: [asset(0xa1), asset(0xb2)] },
  { account: account(3), assets: [asset(0xb2)] },
]);

describe("ingesting an epoch", () => {
  it("writes rows and leaves the mirror verifiable", async () => {
    const store = new MemoryStore();
    const commitment = commitmentFor(epoch4, 4n);
    const result = await ingestSnapshot(store, epoch4, commitment);

    expect(result.wrote).toBe(true);
    expect(result.leaves).toBe(2);
    expect(result.changed).toBe(2);
    expect(result.tombstoned).toBe(0);

    const verified = verifyAsOf(await store.rows(), await store.coverage(4n), commitment);
    expect(verified.ok, verified.reason).toBe(true);
  });

  it("writes only the delta on the next epoch", async () => {
    const store = new MemoryStore();
    await ingestSnapshot(store, epoch4, commitmentFor(epoch4, 4n));
    const second = await ingestSnapshot(store, epoch7, commitmentFor(epoch7, 7n));

    // Epoch 7 has three accounts, but account(1) did not change.
    expect(second.leaves).toBe(3);
    expect(second.rows).toHaveLength(2);
    expect(second.rows.map((r) => r.account)).toEqual([account(2), account(3)]);

    const rows = await store.rows();
    expect(rows).toHaveLength(4); // 2 from epoch 4, 2 from epoch 7
  });

  it("still verifies the older epoch after the newer one is in", async () => {
    const store = new MemoryStore();
    const first = commitmentFor(epoch4, 4n);
    await ingestSnapshot(store, epoch4, first);
    await ingestSnapshot(store, epoch7, commitmentFor(epoch7, 7n));

    const rows = await store.rows();
    expect(verifyAsOf(rows, await store.coverage(4n), first).ok).toBe(true);
    expect(verifyAsOf(rows, await store.coverage(7n), commitmentFor(epoch7, 7n)).ok).toBe(true);
  });

  it("is a no-op the second time it is asked for the same epoch", async () => {
    const store = new MemoryStore();
    const commitment = commitmentFor(epoch4, 4n);
    await ingestSnapshot(store, epoch4, commitment);
    const again = await ingestSnapshot(store, epoch4, commitment);

    expect(again.wrote).toBe(false);
    expect(again.rows).toEqual([]);
    expect(await store.rows()).toHaveLength(2);
  });

  it("recovers from an ingest killed before the epoch was marked done", async () => {
    // The crash window: rows are written, markIngested never ran. A rerun must
    // reproduce exactly the same rows rather than a second version of each.
    const store = new MemoryStore();
    const commitment = commitmentFor(epoch4, 4n);
    const first = await ingestSnapshot(store, epoch4, commitment);
    await store.putRows(first.rows); // as if the retry wrote them again
    expect(await store.rows()).toHaveLength(2);

    const verified = verifyAsOf(await store.rows(), await store.coverage(4n), commitment);
    expect(verified.ok, verified.reason).toBe(true);
  });

  it("refuses a document that does not match the committed root", async () => {
    const store = new MemoryStore();
    const lying = commitmentFor(epoch4, 4n);
    await expect(
      ingestSnapshot(store, epoch7, { ...lying, epochId: 7n }),
    ).rejects.toThrow(/does not match epoch 7/);
    expect(await store.rows()).toHaveLength(0);
  });

  it("refuses a document about another chain", async () => {
    const store = new MemoryStore();
    const commitment = { ...commitmentFor(epoch4, 4n), chainId: 1n };
    await expect(ingestSnapshot(store, epoch4, commitment)).rejects.toThrow(
      /snapshot is about chain 8453, commitment about 1/,
    );
  });

  it("writes nothing when the coverage root disagrees", async () => {
    const store = new MemoryStore();
    const commitment = commitmentFor(epoch4, 4n);
    await expect(
      ingestSnapshot(store, epoch4, { ...commitment, coverageRoot: `0x${"11".repeat(32)}` }),
    ).rejects.toThrow(/rebuilt coverage root/);
    expect(await store.rows()).toHaveLength(0);
  });
});

describe("ordering", () => {
  it("diffs against the newest epoch below the one being ingested", () => {
    expect(previousEpoch([], 4n)).toBe(0n);
    expect(previousEpoch([4n], 7n)).toBe(4n);
    expect(previousEpoch([4n, 7n], 9n)).toBe(7n);
  });

  it("refuses to ingest an epoch older than one already stored", async () => {
    const store = new MemoryStore();
    await ingestSnapshot(store, epoch7, commitmentFor(epoch7, 7n));
    await expect(ingestSnapshot(store, epoch4, commitmentFor(epoch4, 4n))).rejects.toThrow(
      /must be ingested in order/,
    );
  });
});

describe("a real fixture through the whole path", () => {
  it("ingests, resolves and verifies the odd-deep snapshot", async () => {
    const doc = testdata("odd-deep.ndjson");
    const store = new MemoryStore();
    const commitment = commitmentFor(doc, 1n);

    const result = await ingestSnapshot(store, doc, commitment);
    expect(result.leaves).toBe(5);

    const rows = await store.rows();
    const leaves = resolveAsOf(rows, 1n);
    expect(leaves).toHaveLength(5);
    // The order a mirror resolves to is the publisher's leaf order, which is what
    // makes the root reproducible at all.
    expect(leaves.map((l) => l.account)).toEqual(
      parseSnapshot(doc).leaves.map((l) => normalizeHex(l.account, 20)),
    );
    expect(verifyAsOf(rows, await store.coverage(1n), commitment).ok).toBe(true);
  });
});
