/**
 * What the mirror needs from a store, and an in-memory one.
 *
 * The interface exists so that the logic which has to be right — diffing, resolving
 * an epoch, rebuilding a root — is testable without a database, a relay or a
 * network. Evolu implements this interface (see ./evolu.ts) and nothing above this
 * line knows that: swap in IndexedDB, a plain SQLite file, or an object in memory
 * and the verification is identical, because verification is a hash of rows and not
 * a property of where they were kept.
 *
 * That is also the honest boundary of the local-first claim. Evolu supplies sync
 * and encryption; it supplies no integrity that this codebase relies on. Its
 * fingerprint tree reconciles ranges, and it is not the tree HintRegistry commits
 * to — the two are bound only by the client rebuilding the keccak root itself.
 */

import { normalizeHex, type Hex } from "./merkle.js";
import type { Coverage } from "./snapshot.js";
import { rowKey, type VersionRow } from "./state.js";

/**
 * A mirror's persistence.
 *
 * Writes are idempotent by contract: `putRows` on a row that is already stored must
 * be a no-op, not a second copy. Ingest reruns after a crash, and two versions of
 * one account at one epoch is a state `resolveAsOf` refuses — so a store that
 * duplicated rows would turn "ingest ran twice" into "the relay is lying".
 *
 * What the contract deliberately does *not* pin down is which value survives when
 * one key is written twice with **different** assets: `MemoryStore` keeps the first,
 * the Evolu binding's `upsert` keeps the last, and neither is more right. A caller
 * must never do it — `diff` emits at most one row per account per epoch, and an
 * epoch already ingested returns early — and a store that hid such a write either
 * way would be hiding the conflict `resolveAsOf` exists to catch. Relying on either
 * order is therefore a bug in the caller, not a difference to smooth over here.
 */
export interface MirrorStore {
  /** Every version row held locally, in no particular order. */
  rows(): Promise<VersionRow[]>;
  /** Inserts rows not already present, keyed by (account, sinceEpoch). */
  putRows(rows: readonly VersionRow[]): Promise<void>;
  /** The asset set and per-asset ranges an epoch committed. */
  coverage(epoch: bigint): Promise<Coverage[]>;
  putCoverage(epoch: bigint, coverage: readonly Coverage[]): Promise<void>;
  /**
   * Epochs whose ingest finished. Written last, so an interrupted ingest is retried
   * rather than assumed complete.
   */
  ingestedEpochs(): Promise<bigint[]>;
  markIngested(epoch: bigint): Promise<void>;
}

/** A `MirrorStore` in a Map. Used by the tests, and useful for a one-off check. */
export class MemoryStore implements MirrorStore {
  private readonly versions = new Map<string, VersionRow>();
  private readonly coverageByEpoch = new Map<bigint, Coverage[]>();
  private readonly ingested = new Set<bigint>();

  async rows(): Promise<VersionRow[]> {
    return [...this.versions.values()].map((r) => ({ ...r, assets: [...r.assets] }));
  }

  async putRows(rows: readonly VersionRow[]): Promise<void> {
    for (const row of rows) {
      const key = rowKey(row.account, row.sinceEpoch);
      if (this.versions.has(key)) continue;
      this.versions.set(key, {
        account: normalizeHex(row.account, 20),
        sinceEpoch: row.sinceEpoch,
        assets: row.assets.map((a) => normalizeHex(a, 20)),
      });
    }
  }

  async coverage(epoch: bigint): Promise<Coverage[]> {
    return (this.coverageByEpoch.get(epoch) ?? []).map((c) => ({ ...c }));
  }

  async putCoverage(epoch: bigint, coverage: readonly Coverage[]): Promise<void> {
    this.coverageByEpoch.set(
      epoch,
      coverage.map((c) => ({ ...c, asset: normalizeHex(c.asset, 20) })),
    );
  }

  async ingestedEpochs(): Promise<bigint[]> {
    return [...this.ingested].sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
  }

  async markIngested(epoch: bigint): Promise<void> {
    this.ingested.add(epoch);
  }
}

/** The rows a store holds, keyed the way `putRows` dedupes them. */
export function keysOf(rows: readonly VersionRow[]): Set<string> {
  return new Set(rows.map((r) => rowKey(r.account, r.sinceEpoch)));
}

export type { Hex };
