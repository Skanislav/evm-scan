/**
 * What a mirror stores, and how it gets back to a root.
 *
 * The problem this shape solves: a mirror syncs continuously, but a root is a
 * statement about one epoch. If rows were mutable — one row per account, overwritten
 * whenever its assets change — then a client mid-sync would hold a mixture of two
 * epochs and could rebuild neither. Worse, a row overwritten in place changes its
 * fingerprint, so range-based reconciliation would re-transfer it forever.
 *
 * So rows are immutable versions: `(account, sinceEpoch) -> assets`, appended and
 * never edited. `resolveAsOf(rows, n)` then picks, per account, the newest version
 * at or below epoch `n` — a consistent cut through a set that is still growing. It
 * is the same trick Aztec's note tree uses for a different reason: append-only
 * history is what makes a commitment to a past state still checkable later.
 *
 * `sinceEpoch` is the *on-chain* epoch id, never the publisher's local one. A wallet
 * gets its epoch number from `latestFinalizedEpoch` on the registry, and keying by
 * anything else would mean a client could not look up what it just synced.
 */

import { assetsHash, normalizeHex, type Hex } from "./merkle.js";
import type { Leaf } from "./snapshot.js";

/**
 * One immutable version of one account's asset set.
 *
 * An empty `assets` is a tombstone: the account was in the index and no longer is.
 * In practice this barely happens — `interactions` only accumulates and
 * `SnapshotIndex` has no asset-status filter, so an account that appears once keeps
 * appearing — but a mirror that could not represent a removal would have to answer
 * a shrinking index by resyncing from scratch.
 */
export interface VersionRow {
  account: Hex;
  sinceEpoch: bigint;
  assets: Hex[];
}

/** Ascending by account, compared as bytes — the order the publisher's tree used. */
function byAccount(a: { account: Hex }, b: { account: Hex }): number {
  const x = normalizeHex(a.account, 20);
  const y = normalizeHex(b.account, 20);
  return x < y ? -1 : x > y ? 1 : 0;
}

/**
 * The index as of `epoch`: the newest version of every account at or below it,
 * tombstones removed, in the publisher's leaf order.
 *
 * Rows above `epoch` are ignored rather than an error. That is the point — a client
 * that has already synced part of epoch 8 can still verify epoch 7 against the root
 * the registry finalized, instead of waiting for a quiet moment that never comes.
 */
export function resolveAsOf(rows: readonly VersionRow[], epoch: bigint): Leaf[] {
  const newest = new Map<string, VersionRow>();
  for (const row of rows) {
    if (row.sinceEpoch > epoch) continue;
    const account = normalizeHex(row.account, 20);
    const held = newest.get(account);
    if (held === undefined || row.sinceEpoch > held.sinceEpoch) {
      newest.set(account, { ...row, account });
    } else if (row.sinceEpoch === held.sinceEpoch && !sameAssets(row.assets, held.assets)) {
      // Two different asset sets claiming the same (account, epoch) cannot both be
      // what was committed. Resolving it by row order would make the root depend on
      // sync arrival order, which is exactly the bug this layout exists to avoid.
      throw new Error(
        `conflicting versions for ${account} at epoch ${row.sinceEpoch}: ` +
          `a relay is serving rows the publisher did not write`,
      );
    }
  }

  const leaves: Leaf[] = [];
  for (const row of newest.values()) {
    if (row.assets.length === 0) continue; // tombstone
    leaves.push({ account: row.account, assets: row.assets.map((a) => normalizeHex(a, 20)) });
  }
  return leaves.sort(byAccount);
}

/** Compares two asset sets the way the leaf hash does: sorted and deduped. */
export function sameAssets(a: readonly Hex[], b: readonly Hex[]): boolean {
  return assetsHash(a) === assetsHash(b);
}

/**
 * The version rows that carry `next` forward from `prev`.
 *
 * This is what the publisher's ingest emits per epoch, and it is the only reason the
 * mirror is cheaper than re-downloading the table: an epoch that touched 400
 * accounts writes 400 rows, not 540,000. Range-based reconciliation then moves only
 * those rows, because every other fingerprint in the tree is unchanged.
 */
export function diff(
  prev: readonly Leaf[],
  next: readonly Leaf[],
  epoch: bigint,
): VersionRow[] {
  const before = new Map<string, Hex[]>();
  for (const leaf of prev) {
    before.set(normalizeHex(leaf.account, 20), leaf.assets);
  }

  const rows: VersionRow[] = [];
  const seen = new Set<string>();
  for (const leaf of next) {
    const account = normalizeHex(leaf.account, 20);
    seen.add(account);
    const held = before.get(account);
    if (held !== undefined && sameAssets(held, leaf.assets)) continue;
    rows.push({ account, sinceEpoch: epoch, assets: leaf.assets.map((a) => normalizeHex(a, 20)) });
  }
  for (const [account] of before) {
    if (!seen.has(account)) {
      rows.push({ account, sinceEpoch: epoch, assets: [] });
    }
  }
  return rows.sort(byAccount);
}

/**
 * The row's identity, as a string a store can use as a primary key.
 *
 * Ingest has to be idempotent: it runs again after a crash, or on an epoch already
 * partly written, and a duplicated row would leave two versions of one account at
 * one epoch — which `resolveAsOf` rejects, so the failure would surface as "the
 * relay is lying" rather than "ingest ran twice". Keying by (account, epoch) makes
 * the second write a no-op instead.
 */
export function rowKey(account: Hex, sinceEpoch: bigint): string {
  return `${normalizeHex(account, 20)}:${sinceEpoch}`;
}

/** The epochs a set of rows has anything to say about, ascending. */
export function epochsPresent(rows: readonly VersionRow[]): bigint[] {
  const seen = new Set<bigint>();
  for (const row of rows) seen.add(row.sinceEpoch);
  return [...seen].sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
}
