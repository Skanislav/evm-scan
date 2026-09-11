/**
 * The Evolu adapter, against a fake Evolu.
 *
 * This does not test Evolu — it tests the adapter's side of the contract, which is
 * where the bugs that matter live: a row id that is not deterministic (so a retried
 * ingest doubles rows), a missing scope (so two chains rebuild one root), a bigint
 * put through a JS number (so an epoch id rounds). The fake implements the
 * documented `upsert`/`createQuery`/`loadQuery` surface and nothing else.
 *
 * What remains unverified here is the binding to the real library: `@evolu/common`
 * needs Node >= 24.20.0, which this toolchain does not have. See the note at the
 * top of ../src/evolu.ts.
 */

import { describe, expect, it } from "vitest";

import { ingestSnapshot } from "../src/ingest.js";
import { createMirrorStore, scopedRowId, scopeOf, type EvoluLike } from "../src/evolu.js";
import { type Hex } from "../src/merkle.js";
import { parseSnapshot, rootsOf, type Leaf } from "../src/snapshot.js";
import { diff, resolveAsOf } from "../src/state.js";
import { verifyAsOf, type Commitment } from "../src/verify.js";

const REGISTRY: Hex = "0x00000000000000000000000000000000000000ee";
const CHAIN = 8453n;

const account = (n: number): Hex => `0x${n.toString(16).padStart(40, "0")}`;
const asset = (n: number): Hex => `0x${n.toString(16).padStart(40, "0")}`;

/** A recorded query: the table, the columns, and the single where clause. */
interface FakeQuery {
  table: string;
  columns: string[];
  where: [string, string, unknown];
}

/**
 * An Evolu stand-in: a Map of rows by id, and a query interpreter that handles the
 * one shape the adapter builds.
 */
class FakeEvolu implements EvoluLike {
  readonly tables = new Map<string, Map<string, Record<string, unknown>>>();
  upserts = 0;

  upsert(table: string, row: Record<string, unknown>): { ok: boolean; error?: unknown } {
    const id = row["id"];
    if (typeof id !== "string" || id === "") {
      return { ok: false, error: "a row needs a string id" };
    }
    this.upserts++;
    const rows = this.tables.get(table) ?? new Map();
    rows.set(id, { ...row });
    this.tables.set(table, rows);
    return { ok: true };
  }

  createQuery(build: (db: unknown) => unknown): unknown {
    const recorded: Partial<FakeQuery> = {};
    const db = {
      selectFrom: (table: string) => {
        recorded.table = table;
        return {
          select: (columns: string[]) => {
            recorded.columns = columns;
            return {
              where: (column: string, op: string, value: unknown) => {
                recorded.where = [column, op, value];
                return recorded;
              },
            };
          },
        };
      },
    };
    build(db);
    return recorded as FakeQuery;
  }

  async loadQuery(query: unknown): Promise<readonly Record<string, unknown>[]> {
    const q = query as FakeQuery;
    const rows = [...(this.tables.get(q.table)?.values() ?? [])];
    const [column, , value] = q.where;
    return rows.filter((r) => r[column] === value);
  }

  subscribeQuery(): (listener: () => void) => () => void {
    return () => () => {};
  }
}

/** Deterministic, like `createIdFromString`: same string in, same id out. */
const fakeIdFromString = (value: string): string => `id-${value}`;

const store = (evolu: FakeEvolu, chainId = CHAIN) =>
  createMirrorStore({ evolu, createIdFromString: fakeIdFromString, registry: REGISTRY, chainId });

function document(chainId: bigint, leaves: Leaf[]): string {
  const coverage = [{ asset: asset(0xa1), fromBlock: 100n, toBlock: 900n }];
  const roots = rootsOf(chainId, coverage, leaves);
  const lines = [
    JSON.stringify({
      format: "evmscan-snapshot/1",
      chain_id: Number(chainId),
      from_block: 1,
      to_block: 900,
      root: roots.root,
      coverage_root: roots.coverageRoot,
      leaf_count: leaves.length,
      asset_count: coverage.length,
    }),
    JSON.stringify({ t: "coverage", asset: asset(0xa1), from_block: 100, to_block: 900 }),
    ...leaves.map((l) => JSON.stringify({ t: "leaf", account: l.account, assets: l.assets })),
  ];
  return lines.join("\n");
}

const LEAVES: Leaf[] = [
  { account: account(1), assets: [asset(0xa1)] },
  { account: account(2), assets: [asset(0xa1)] },
];
const DOC = document(CHAIN, LEAVES);

function commitment(doc: string, epochId: bigint): Commitment {
  const snapshot = parseSnapshot(doc);
  return {
    epochId,
    chainId: snapshot.header.chainId,
    root: snapshot.root,
    coverageRoot: snapshot.coverageRoot,
  };
}

describe("row identity", () => {
  it("gives a row the same id every time", () => {
    const row = { account: account(1), sinceEpoch: 4n, assets: [asset(0xa1)] };
    expect(scopedRowId(REGISTRY, CHAIN, row)).toBe(scopedRowId(REGISTRY, CHAIN, row));
    // Case of the incoming address must not change the id.
    expect(
      scopedRowId(REGISTRY, CHAIN, { ...row, account: account(1).toUpperCase().replace("0X", "0x") }),
    ).toBe(scopedRowId(REGISTRY, CHAIN, row));
  });

  it("separates the same account on two chains, and two registries", () => {
    const row = { account: account(1), sinceEpoch: 4n, assets: [] };
    expect(scopedRowId(REGISTRY, 1n, row)).not.toBe(scopedRowId(REGISTRY, 8453n, row));
    const other: Hex = "0x00000000000000000000000000000000000000ef";
    expect(scopedRowId(other, CHAIN, row)).not.toBe(scopedRowId(REGISTRY, CHAIN, row));
    expect(scopeOf(REGISTRY, CHAIN)).toBe(`${REGISTRY}:8453`);
  });
});

describe("the store contract, through the adapter", () => {
  it("round-trips version rows including a tombstone", async () => {
    const evolu = new FakeEvolu();
    const s = store(evolu);
    await s.putRows([
      { account: account(1), sinceEpoch: 4n, assets: [asset(0xa1), asset(0xb2)] },
      { account: account(2), sinceEpoch: 9n, assets: [] },
    ]);

    const rows = await s.rows();
    expect(rows).toHaveLength(2);
    const first = rows.find((r) => r.account === account(1))!;
    expect(first.sinceEpoch).toBe(4n);
    expect(first.assets).toEqual([asset(0xa1), asset(0xb2)]);
    expect(rows.find((r) => r.account === account(2))!.assets).toEqual([]);
  });

  it("keeps an epoch id that a JS number would round", async () => {
    const evolu = new FakeEvolu();
    const s = store(evolu);
    const huge = 2n ** 200n + 7n; // an epoch id is a uint256
    await s.putRows([{ account: account(1), sinceEpoch: huge, assets: [] }]);
    expect((await s.rows())[0]!.sinceEpoch).toBe(huge);
  });

  it("writes one row when the same row is put twice", async () => {
    const evolu = new FakeEvolu();
    const s = store(evolu);
    const rows = [{ account: account(1), sinceEpoch: 4n, assets: [asset(0xa1)] }];
    await s.putRows(rows);
    await s.putRows(rows);
    expect(evolu.upserts).toBe(2); // both writes happened...
    expect(await s.rows()).toHaveLength(1); // ...and landed on one row
  });

  it("does not show one chain's rows to another", async () => {
    const evolu = new FakeEvolu();
    await store(evolu, 1n).putRows([{ account: account(1), sinceEpoch: 4n, assets: [] }]);
    await store(evolu, 8453n).putRows([{ account: account(2), sinceEpoch: 4n, assets: [] }]);

    expect((await store(evolu, 1n).rows()).map((r) => r.account)).toEqual([account(1)]);
    expect((await store(evolu, 8453n).rows()).map((r) => r.account)).toEqual([account(2)]);
  });

  it("marks an epoch ingested without losing its coverage", async () => {
    const evolu = new FakeEvolu();
    const s = store(evolu);
    await s.putCoverage(4n, [{ asset: asset(0xa1), fromBlock: 100n, toBlock: 900n }]);
    expect(await s.ingestedEpochs()).toEqual([]);

    await s.markIngested(4n);
    expect(await s.ingestedEpochs()).toEqual([4n]);
    expect(await s.coverage(4n)).toEqual([
      { asset: asset(0xa1), fromBlock: 100n, toBlock: 900n },
    ]);
  });

  it("has nothing to say about an epoch it never ingested", async () => {
    const s = store(new FakeEvolu());
    expect(await s.coverage(99n)).toEqual([]);
    expect(await s.ingestedEpochs()).toEqual([]);
  });
});

describe("a full ingest through the adapter", () => {
  it("ends with a mirror that verifies against the commitment", async () => {
    const evolu = new FakeEvolu();
    const s = store(evolu);
    const c = commitment(DOC, 4n);

    const result = await ingestSnapshot(s, DOC, c);
    expect(result.wrote).toBe(true);

    const verified = verifyAsOf(await s.rows(), await s.coverage(4n), c);
    expect(verified.ok, verified.reason).toBe(true);
    expect(resolveAsOf(await s.rows(), 4n)).toHaveLength(2);
  });

  it("is idempotent when the whole ingest runs again", async () => {
    const evolu = new FakeEvolu();
    const s = store(evolu);
    const c = commitment(DOC, 4n);

    await ingestSnapshot(s, DOC, c);
    const again = await ingestSnapshot(s, DOC, c);
    expect(again.wrote).toBe(false);
    expect(verifyAsOf(await s.rows(), await s.coverage(4n), c).ok).toBe(true);
  });

  it("survives a crash between writing rows and marking the epoch done", async () => {
    const evolu = new FakeEvolu();
    const s = store(evolu);
    const c = commitment(DOC, 4n);

    // The crash window: the rows of epoch 4 are written, markIngested never ran.
    const written = diff([], parseSnapshot(DOC).leaves, 4n);
    await s.putRows(written);
    await s.putCoverage(4n, [{ asset: asset(0xa1), fromBlock: 100n, toBlock: 900n }]);
    expect(await s.ingestedEpochs()).toEqual([]);
    expect(await s.rows()).toHaveLength(2);

    // The retry re-diffs from nothing and rewrites the same ids, so the rows it
    // lands on are the rows already there rather than a second version of each.
    const retry = await ingestSnapshot(s, DOC, c);
    expect(retry.wrote).toBe(true);
    expect(await s.rows()).toHaveLength(2);
    expect(verifyAsOf(await s.rows(), await s.coverage(4n), c).ok).toBe(true);
  });

  it("refuses a row Evolu rejects rather than reporting a partial write as done", async () => {
    const evolu = new FakeEvolu();
    const s = createMirrorStore({
      evolu,
      createIdFromString: () => "", // an id Evolu would refuse
      registry: REGISTRY,
      chainId: CHAIN,
    });
    await expect(
      s.putRows([{ account: account(1), sinceEpoch: 4n, assets: [] }]),
    ).rejects.toThrow(/evolu rejected an? indexRow row/);
  });
});
