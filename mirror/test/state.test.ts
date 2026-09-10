/**
 * The mirror's storage model, checked end to end without a relay or a chain.
 *
 * The property under test is the one the whole design rests on: a client holding
 * only append-only version rows can rebuild the exact root any past epoch
 * committed, in any row order, having synced the epochs in any order.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

import { assetsHash, type Hex } from "../src/merkle.js";
import { parseSnapshot, rootsOf, type Coverage, type Leaf } from "../src/snapshot.js";
import { diff, epochsPresent, resolveAsOf, rowKey, sameAssets, type VersionRow } from "../src/state.js";
import { verifyAsOf } from "../src/verify.js";

const CHAIN = 8453n;

const account = (n: number): Hex => `0x${n.toString(16).padStart(40, "0")}`;
const asset = (n: number): Hex => `0x${n.toString(16).padStart(40, "0")}`;

const A1 = asset(0xa1);
const B2 = asset(0xb2);
const C3 = asset(0xc3);

/** A plausible history: accounts appear, gain assets, and one goes away. */
const epochs: { id: bigint; leaves: Leaf[] }[] = [
  {
    id: 4n,
    leaves: [
      { account: account(1), assets: [A1] },
      { account: account(2), assets: [A1] },
      { account: account(3), assets: [B2] },
    ],
  },
  {
    id: 7n, // on-chain ids are not contiguous: an epoch can be challenged away
    leaves: [
      { account: account(1), assets: [A1] }, // unchanged: no row written
      { account: account(2), assets: [A1, B2] }, // gained one
      { account: account(3), assets: [B2] },
      { account: account(4), assets: [C3] }, // new
    ],
  },
  {
    id: 9n,
    leaves: [
      { account: account(1), assets: [A1, B2, C3] },
      { account: account(2), assets: [A1, B2] },
      { account: account(4), assets: [C3] },
      // account(3) is gone: a tombstone
    ],
  },
];

const coverage: Coverage[] = [
  { asset: A1, fromBlock: 100n, toBlock: 900n },
  { asset: B2, fromBlock: 150n, toBlock: 900n },
  { asset: C3, fromBlock: 200n, toBlock: 900n },
];

/** Every version row the publisher's ingest would write for that history. */
function ingest(): VersionRow[] {
  const rows: VersionRow[] = [];
  let prev: Leaf[] = [];
  for (const epoch of epochs) {
    rows.push(...diff(prev, epoch.leaves, epoch.id));
    prev = epoch.leaves;
  }
  return rows;
}

describe("version rows", () => {
  it("writes a row only for accounts whose asset set changed", () => {
    const rows = ingest();
    // Epoch 4 introduces three accounts; epoch 7 changes one and adds one; epoch 9
    // changes one and tombstones one.
    expect(rows.filter((r) => r.sinceEpoch === 4n)).toHaveLength(3);
    expect(rows.filter((r) => r.sinceEpoch === 7n)).toHaveLength(2);
    expect(rows.filter((r) => r.sinceEpoch === 9n)).toHaveLength(2);
    // 7 rows for a history whose final state has 3 accounts and whose epochs list
    // 11 account-rows between them. That gap is the entire point.
    expect(rows).toHaveLength(7);
  });

  it("leaves an unchanged account alone across an epoch", () => {
    const rows = ingest().filter((r) => r.account === account(1));
    expect(rows.map((r) => r.sinceEpoch)).toEqual([4n, 9n]); // nothing at 7
  });

  it("records a departure as an empty asset set", () => {
    const tombstone = ingest().find((r) => r.account === account(3) && r.sinceEpoch === 9n);
    expect(tombstone?.assets).toEqual([]);
  });

  it("gives a row a key that makes a repeated ingest a no-op", () => {
    const rows = ingest();
    const keys = rows.map((r) => rowKey(r.account, r.sinceEpoch));
    expect(new Set(keys).size).toBe(keys.length);
    // The same key whatever the case of the address it came in as.
    expect(rowKey("0x00000000000000000000000000000000000000A1", 4n)).toBe(rowKey(A1, 4n));
  });
});

describe("resolving an epoch", () => {
  it.each(epochs)("rebuilds the root epoch $id committed", ({ id, leaves }) => {
    const expected = rootsOf(CHAIN, coverage, leaves);
    const resolved = resolveAsOf(ingest(), id);
    expect(rootsOf(CHAIN, coverage, resolved).root).toBe(expected.root);
  });

  it("verifies a past epoch while rows from a later one are already present", () => {
    // The realistic case: the client synced through epoch 9 and is asked about 7.
    const result = verifyAsOf(ingest(), coverage, {
      epochId: 7n,
      chainId: CHAIN,
      root: rootsOf(CHAIN, coverage, epochs[1]!.leaves).root,
      coverageRoot: rootsOf(CHAIN, coverage, epochs[1]!.leaves).coverageRoot,
    });
    expect(result.ok).toBe(true);
    expect(result.leaves).toHaveLength(4);
    expect(result.reason).toMatch(/verified: these 4 accounts/);
  });

  it("does not resurrect a tombstoned account", () => {
    const at9 = resolveAsOf(ingest(), 9n);
    expect(at9.map((l) => l.account)).not.toContain(account(3));
    const at7 = resolveAsOf(ingest(), 7n);
    expect(at7.map((l) => l.account)).toContain(account(3));
  });

  it("ignores rows from epochs above the one being checked", () => {
    const at4 = resolveAsOf(ingest(), 4n);
    expect(at4).toEqual(epochs[0]!.leaves);
  });

  it("is independent of the order rows arrive in", () => {
    const rows = ingest();
    const shuffles = [
      [...rows].reverse(),
      [...rows].sort((a, b) => (a.sinceEpoch > b.sinceEpoch ? -1 : 1)),
      [rows[3]!, rows[0]!, rows[6]!, rows[1]!, rows[5]!, rows[2]!, rows[4]!],
    ];
    for (const shuffled of shuffles) {
      for (const { id, leaves } of epochs) {
        expect(rootsOf(CHAIN, coverage, resolveAsOf(shuffled, id)).root).toBe(
          rootsOf(CHAIN, coverage, leaves).root,
        );
      }
    }
  });

  it("reports an incomplete sync as unverified, not as a lie", () => {
    const partial = ingest().filter((r) => r.account !== account(4));
    const result = verifyAsOf(partial, coverage, {
      epochId: 9n,
      chainId: CHAIN,
      root: rootsOf(CHAIN, coverage, epochs[2]!.leaves).root,
      coverageRoot: rootsOf(CHAIN, coverage, epochs[2]!.leaves).coverageRoot,
    });
    expect(result.ok).toBe(false);
    expect(result.mismatches).toEqual(["root"]);
    expect(result.reason).toMatch(/has not finished syncing, or it is serving rows/);
  });

  it("catches a coverage set that does not match the commitment", () => {
    const truth = rootsOf(CHAIN, coverage, epochs[2]!.leaves);
    const result = verifyAsOf(ingest(), coverage.slice(0, 2), {
      epochId: 9n,
      chainId: CHAIN,
      root: truth.root,
      coverageRoot: truth.coverageRoot,
    });
    expect(result.mismatches).toEqual(["coverage-root"]);
  });

  it("refuses two different asset sets for one account at one epoch", () => {
    const rows = [
      ...ingest(),
      { account: account(2), sinceEpoch: 7n, assets: [A1, B2, C3] },
    ];
    expect(() => resolveAsOf(rows, 9n)).toThrow(/conflicting versions/);
  });

  it("refuses a conflict that is not the winning version, in any row order", () => {
    // The conflicting pair sits at epoch 4 while a newer version of the same
    // account exists at epoch 7. `store.rows()` promises no order, so detection
    // must not depend on which of the three arrives first: otherwise the same
    // poisoned row fails verification on one load and passes on the next.
    const poison: VersionRow[] = [
      { account: account(1), sinceEpoch: 4n, assets: [A1] },
      { account: account(1), sinceEpoch: 4n, assets: [B2] },
      { account: account(1), sinceEpoch: 7n, assets: [A1, B2] },
    ];
    for (const order of [
      [0, 1, 2],
      [2, 0, 1],
      [1, 2, 0],
      [2, 1, 0],
    ]) {
      const rows = order.map((i) => poison[i]!);
      expect(() => resolveAsOf(rows, 9n), `order ${order.join("")}`).toThrow(
        /conflicting versions/,
      );
    }
  });

  it("accepts a duplicate row that says the same thing", () => {
    const rows = ingest();
    const duplicated = [...rows, { ...rows[0]! }];
    expect(() => resolveAsOf(duplicated, 9n)).not.toThrow();
    expect(resolveAsOf(duplicated, 9n)).toEqual(resolveAsOf(rows, 9n));
  });

  it("treats a reordered, duplicated asset list as the same set", () => {
    expect(sameAssets([A1, B2], [B2, A1, A1])).toBe(true);
    expect(sameAssets([A1], [A1, B2])).toBe(false);
    expect(diff([{ account: account(1), assets: [A1, B2] }],
                [{ account: account(1), assets: [B2, A1] }], 9n)).toEqual([]);
  });

  it("lists the epochs a row set covers", () => {
    expect(epochsPresent(ingest())).toEqual([4n, 7n, 9n]);
  });
});

describe("against the Go fixtures", () => {
  const testdata = (name: string): string =>
    readFileSync(fileURLToPath(new URL(`../testdata/${name}`, import.meta.url)), "utf8");

  it("carries a real snapshot through diff and back to its committed root", () => {
    // The path an ingest actually takes: a snapshot document in, version rows out,
    // and a root rebuilt from those rows that equals the one in the header.
    for (const name of ["single", "even", "odd", "odd-deep", "messy-assets"]) {
      const snapshot = parseSnapshot(testdata(`${name}.ndjson`));
      const rows = diff([], snapshot.leaves, 1n);
      const result = verifyAsOf(rows, snapshot.coverage, {
        epochId: 1n,
        chainId: snapshot.header.chainId,
        root: snapshot.header.root,
        coverageRoot: snapshot.header.coverageRoot,
      });
      expect(result.ok, `${name}: ${result.reason}`).toBe(true);
    }
  });

  it("keeps a leaf whose assets were written unsorted", () => {
    // messy-assets has one leaf with descending, duplicated assets. A version row
    // stores it as written; only the hash sorts. If diff or resolveAsOf normalised
    // the list into a different set, this root would not match.
    const snapshot = parseSnapshot(testdata("messy-assets.ndjson"));
    const rows = diff([], snapshot.leaves, 1n);
    const stored = rows.find((r) => r.account === snapshot.leaves[0]!.account)!;
    expect(stored.assets).toHaveLength(4);
    expect(assetsHash(stored.assets)).toBe(assetsHash(snapshot.leaves[1]!.assets));
    expect(rootsOf(snapshot.header.chainId, [], resolveAsOf(rows, 1n)).root).toBe(
      snapshot.header.root,
    );
  });
});
