/**
 * The Evolu binding: a `MirrorStore` that syncs.
 *
 * Everything above this file is Evolu-free and tested. This is the only place that
 * knows a relay exists, and it deliberately supplies nothing the verification
 * depends on — Evolu gives sync, local SQLite and encryption; the integrity comes
 * from the client rebuilding the keccak root and comparing it with the registry.
 * Evolu's own fingerprint tree (XOR-ed truncated SHA-256, reconciled by range) is a
 * different tree for a different job, and no part of this codebase confuses the two.
 *
 * ## Why Evolu is a good fit here
 *
 * - **Range-based set reconciliation** moves only the rows a client is missing, in
 *   a number of round trips that grows logarithmically. An epoch that changed 400
 *   accounts costs 400 rows, not a re-download of the table.
 * - **Immutable rows suit it.** Version rows are never edited, so their fingerprints
 *   never change, so reconciliation converges instead of re-sending.
 * - **The relay is blind and interchangeable** — it moves encrypted rows and
 *   fingerprints and cannot read them. That is the same trust posture
 *   docs/RECOVERY.md already argues for a snapshot host: availability only.
 * - **`upsert` takes a caller-supplied id**, so `createIdFromString(rowKey(...))`
 *   makes a repeated ingest a no-op by construction rather than by bookkeeping.
 *
 * ## What is public here, and what that means for encryption
 *
 * The index is public data — token logs anyone can read. Owner-scoped encryption is
 * therefore not protecting anything; it is simply what Evolu does, and it costs
 * nothing. The publisher holds a `SharedOwner` (write key); wallets get the
 * `SharedReadonlyOwner` derived from it, which carries the id and the decryption key
 * and no ability to write. Handing that to the world is the intended use.
 *
 * ## Status: not executed in this repository
 *
 * `@evolu/common` requires Node >= 24.20.0 and this repo's toolchain is older, so
 * this file is written against the published API and typechecked in isolation — it
 * has not been run against a relay here. The layers it plugs into are tested
 * without it (see ../test), which is the point of the `MirrorStore` seam: if this
 * binding is wrong, it is wrong in ways a first run against a relay will show, and
 * it cannot make a verified mirror report a false pass.
 */

import { normalizeHex, type Hex } from "./merkle.js";
import type { Coverage } from "./snapshot.js";
import { rowKey, type VersionRow } from "./state.js";
import type { MirrorStore } from "./store.js";

/**
 * The Evolu surface this file uses, declared rather than imported.
 *
 * Depending on `@evolu/common` types directly would make this file — and so the
 * whole package — uninstallable on a toolchain Evolu does not support, for the sake
 * of one adapter. The shapes below are the documented ones; `createMirrorStore`
 * takes an instance and does not care how it was built.
 */
export interface EvoluLike {
  upsert(
    table: string,
    row: Record<string, unknown>,
    options?: { onComplete?: () => void; onlyValidate?: boolean },
  ): { ok: boolean; error?: unknown };
  createQuery(build: (db: unknown) => unknown): unknown;
  loadQuery(query: unknown): Promise<readonly Record<string, unknown>[]>;
  subscribeQuery(query: unknown): (listener: () => void) => () => void;
}

/** `createIdFromString` from `@evolu/common`, passed in to keep this file portable. */
export type IdFromString = (value: string) => string;

/**
 * The tables a mirror keeps.
 *
 * Pass this to `createEvolu(deps)(mirrorSchema, { name, transports })`. The column
 * types come from `@evolu/common` and are named here rather than imported, for the
 * reason in the header; the shape is what matters:
 *
 * ```ts
 * import { createEvolu, createIdFromString, id, maxLength, NonEmptyString } from "@evolu/common";
 * import { evoluWebDeps } from "@evolu/web";
 *
 * const IndexRowId = id("IndexRow");
 * const Hex42 = maxLength(42, NonEmptyString);
 * const Digits20 = maxLength(20, NonEmptyString);
 * const AssetList = maxLength(65_536, NonEmptyString);
 *
 * const Schema = {
 *   indexRow: {
 *     id: IndexRowId,       // createIdFromString(scopedRowId(...))
 *     scope: Hex42,         // `${registry}:${chainId}` — see scopeOf
 *     account: Hex42,
 *     sinceEpoch: Digits20, // a uint256 epoch id, as decimal text
 *     assets: AssetList,    // JSON array of addresses, "[]" for a tombstone
 *   },
 *   epochMeta: {
 *     id: id("EpochMeta"),
 *     scope: Hex42,
 *     epoch: Digits20,
 *     coverage: AssetList,  // JSON array of {asset, fromBlock, toBlock}
 *     ingested: Digits20,   // "1" once the epoch's rows are all written
 *   },
 * };
 * ```
 *
 * Numbers are stored as decimal text throughout. An epoch id is a `uint256` and a
 * block number a `uint64`; SQLite integers and JS numbers both lose the top of
 * those ranges, and a silently rounded epoch id would resolve the wrong cut.
 */
export const MIRROR_TABLES = { rows: "indexRow", meta: "epochMeta" } as const;

/**
 * Scopes a row to one registry on one chain.
 *
 * `leafHash` commits to the chain id, so rows about two chains are about two
 * different trees; and two deployments of the registry on the same chain are two
 * different sets of commitments. A mirror that mixed either would rebuild a root
 * from a union nobody committed.
 */
export function scopeOf(registry: Hex, chainId: bigint): string {
  return `${normalizeHex(registry, 20)}:${chainId}`;
}

/** The deterministic row id: the scope, then the row's own key. */
export function scopedRowId(registry: Hex, chainId: bigint, row: VersionRow): string {
  return `${scopeOf(registry, chainId)}:${rowKey(row.account, row.sinceEpoch)}`;
}

function scopedMetaId(registry: Hex, chainId: bigint, epoch: bigint): string {
  return `${scopeOf(registry, chainId)}:epoch:${epoch}`;
}

export interface MirrorStoreConfig {
  evolu: EvoluLike;
  createIdFromString: IdFromString;
  /** The registry the commitments live in. */
  registry: Hex;
  /** The chain the index is *about*. */
  chainId: bigint;
}

/**
 * Wraps an Evolu instance as a `MirrorStore`.
 *
 * Reads go through `loadQuery`, which resolves against local SQLite — so a mirror
 * answers offline, with whatever it last synced, and says so through verification
 * rather than through a spinner.
 */
export function createMirrorStore(config: MirrorStoreConfig): MirrorStore {
  const { evolu, createIdFromString, registry, chainId } = config;
  const scope = scopeOf(registry, chainId);

  const rowsQuery = evolu.createQuery((db) =>
    (db as SelectableDb)
      .selectFrom(MIRROR_TABLES.rows)
      .select(["account", "sinceEpoch", "assets"])
      .where("scope", "=", scope),
  );
  const metaQuery = evolu.createQuery((db) =>
    (db as SelectableDb)
      .selectFrom(MIRROR_TABLES.meta)
      .select(["epoch", "coverage", "ingested"])
      .where("scope", "=", scope),
  );

  const put = (table: string, row: Record<string, unknown>): void => {
    const result = evolu.upsert(table, row);
    if (!result.ok) {
      throw new Error(`evolu rejected a ${table} row: ${JSON.stringify(result.error)}`);
    }
  };

  return {
    async rows(): Promise<VersionRow[]> {
      const loaded = await evolu.loadQuery(rowsQuery);
      return loaded.map((r) => ({
        account: normalizeHex(String(r["account"]), 20),
        sinceEpoch: BigInt(String(r["sinceEpoch"])),
        assets: (JSON.parse(String(r["assets"])) as string[]).map((a) => normalizeHex(a, 20)),
      }));
    },

    async putRows(rows: readonly VersionRow[]): Promise<void> {
      // upsert on a deterministic id, so the same row written twice is one row.
      // That is what makes a retried ingest safe: two versions of one account at
      // one epoch is a state resolveAsOf refuses.
      for (const row of rows) {
        put(MIRROR_TABLES.rows, {
          id: createIdFromString(scopedRowId(registry, chainId, row)),
          scope,
          account: normalizeHex(row.account, 20),
          sinceEpoch: row.sinceEpoch.toString(),
          assets: JSON.stringify(row.assets.map((a) => normalizeHex(a, 20))),
        });
      }
    },

    async coverage(epoch: bigint): Promise<Coverage[]> {
      const loaded = await evolu.loadQuery(metaQuery);
      const row = loaded.find((r) => String(r["epoch"]) === epoch.toString());
      if (row === undefined) return [];
      const parsed = JSON.parse(String(row["coverage"])) as {
        asset: string;
        fromBlock: string;
        toBlock: string;
      }[];
      return parsed.map((c) => ({
        asset: normalizeHex(c.asset, 20),
        fromBlock: BigInt(c.fromBlock),
        toBlock: BigInt(c.toBlock),
      }));
    },

    async putCoverage(epoch: bigint, coverage: readonly Coverage[]): Promise<void> {
      put(MIRROR_TABLES.meta, {
        id: createIdFromString(scopedMetaId(registry, chainId, epoch)),
        scope,
        epoch: epoch.toString(),
        coverage: JSON.stringify(
          coverage.map((c) => ({
            asset: normalizeHex(c.asset, 20),
            fromBlock: c.fromBlock.toString(),
            toBlock: c.toBlock.toString(),
          })),
        ),
        ingested: "0",
      });
    },

    async ingestedEpochs(): Promise<bigint[]> {
      const loaded = await evolu.loadQuery(metaQuery);
      return loaded
        .filter((r) => String(r["ingested"]) === "1")
        .map((r) => BigInt(String(r["epoch"])))
        .sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
    },

    async markIngested(epoch: bigint): Promise<void> {
      const loaded = await evolu.loadQuery(metaQuery);
      const held = loaded.find((r) => String(r["epoch"]) === epoch.toString());
      put(MIRROR_TABLES.meta, {
        id: createIdFromString(scopedMetaId(registry, chainId, epoch)),
        scope,
        epoch: epoch.toString(),
        // Keep whatever coverage was written; this call only flips the flag.
        coverage: held === undefined ? "[]" : String(held["coverage"]),
        ingested: "1",
      });
    },
  };
}

/** The sliver of Kysely's builder the queries above use. */
interface SelectableDb {
  selectFrom(table: string): {
    select(columns: string[]): {
      where(column: string, op: string, value: unknown): unknown;
    };
  };
}

/**
 * Watches for rows arriving from the relay.
 *
 * A mirror is verified *after* reconciliation settles, not during it: mid-sync a
 * client holds a partial set, and a partial set rebuilds a root that matches
 * nothing. So the useful pattern is to re-verify on this callback and treat a
 * mismatch as "not yet" until it stops changing — the chain carries no leaf count,
 * so there is no completeness signal to wait on instead.
 */
export function onRowsChanged(config: MirrorStoreConfig, listener: () => void): () => void {
  const { evolu, registry, chainId } = config;
  const query = evolu.createQuery((db) =>
    (db as SelectableDb)
      .selectFrom(MIRROR_TABLES.rows)
      .select(["account"])
      .where("scope", "=", scopeOf(registry, chainId)),
  );
  return evolu.subscribeQuery(query)(listener);
}
