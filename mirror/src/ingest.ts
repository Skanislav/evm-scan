/**
 * Turning a published snapshot into version rows.
 *
 * This runs on the publisher's side, as the Evolu owner holding the write key. It
 * fetches the epoch's snapshot — the document `docs/RECOVERY.md` describes, already
 * served at `/v1/epochs/{id}/snapshot` and already pointed at by the `uri` in the
 * `IndexPublished` log — checks it against the root the registry finalized, diffs it
 * against what the store already resolves to, and appends only what changed.
 *
 * Note which side does the expensive thing. The publisher downloads the whole table
 * every epoch, which costs nothing because it is the machine that produced it. What
 * the wallets get is the delta, moved by range-based reconciliation. That asymmetry
 * is deliberate: it is why no new endpoint had to be added to the daemon for any of
 * this.
 *
 * The snapshot is verified *before* a row is written. A mirror that ingested an
 * unchecked document would hand its readers rows that fail against the chain, and
 * the reader could not tell that from a mid-sync gap.
 */

import { parseSnapshot, type Snapshot } from "./snapshot.js";
import { diff, resolveAsOf, type VersionRow } from "./state.js";
import type { MirrorStore } from "./store.js";
import type { Commitment } from "./verify.js";

export interface IngestResult {
  epochId: bigint;
  /** False when the epoch was already ingested and nothing was written. */
  wrote: boolean;
  rows: VersionRow[];
  leaves: number;
  /** Accounts whose asset set changed, and accounts that left the index. */
  changed: number;
  tombstoned: number;
}

/**
 * Ingests one epoch's snapshot into the store.
 *
 * `commitment` is what the registry says about this epoch, and the snapshot has to
 * match it in both roots or nothing is written. The epoch id in the commitment is
 * the on-chain one, which is what `sinceEpoch` records — a wallet looks epochs up by
 * that number and by no other.
 */
export async function ingestSnapshot(
  store: MirrorStore,
  document: string,
  commitment: Commitment,
): Promise<IngestResult> {
  const already = await store.ingestedEpochs();
  if (already.includes(commitment.epochId)) {
    return {
      epochId: commitment.epochId,
      wrote: false,
      rows: [],
      leaves: 0,
      changed: 0,
      tombstoned: 0,
    };
  }

  const snapshot = parseSnapshot(document);
  assertMatches(snapshot, commitment);

  // The state this store already resolves to at the epoch just below this one.
  // Anything above would mean ingesting epochs out of order, which `previousEpoch`
  // guards against.
  const rows = await store.rows();
  const previous = resolveAsOf(rows, previousEpoch(already, commitment.epochId));
  const written = diff(previous, snapshot.leaves, commitment.epochId);

  await store.putRows(written);
  await store.putCoverage(commitment.epochId, snapshot.coverage);
  // Last, so an ingest killed halfway is retried rather than assumed done. Retrying
  // is safe because putRows is keyed by (account, sinceEpoch).
  await store.markIngested(commitment.epochId);

  return {
    epochId: commitment.epochId,
    wrote: true,
    rows: written,
    leaves: snapshot.leaves.length,
    changed: written.filter((r) => r.assets.length > 0).length,
    tombstoned: written.filter((r) => r.assets.length === 0).length,
  };
}

/**
 * The epoch to diff against: the newest one already ingested below this one.
 *
 * Ingesting out of order is refused rather than merged. A diff written against the
 * wrong baseline produces version rows that resolve to a set nobody committed, and
 * the mismatch would surface later as a root failure with no trace of the cause.
 */
export function previousEpoch(ingested: readonly bigint[], epochId: bigint): bigint {
  const below = ingested.filter((e) => e < epochId);
  const above = ingested.filter((e) => e > epochId);
  if (above.length > 0) {
    throw new Error(
      `epoch ${epochId} is older than ${above[0]}, which is already ingested: ` +
        `epochs must be ingested in order, because each diff is against the last`,
    );
  }
  return below.length === 0 ? 0n : below[below.length - 1]!;
}

function assertMatches(snapshot: Snapshot, commitment: Commitment): void {
  if (snapshot.header.chainId !== commitment.chainId) {
    throw new Error(
      `snapshot is about chain ${snapshot.header.chainId}, commitment about ` +
        `${commitment.chainId}`,
    );
  }
  if (snapshot.root !== commitment.root) {
    throw new Error(
      `snapshot does not match epoch ${commitment.epochId}: rebuilt root ` +
        `${snapshot.root}, registry holds ${commitment.root}`,
    );
  }
  if (snapshot.coverageRoot !== commitment.coverageRoot) {
    throw new Error(
      `snapshot does not match epoch ${commitment.epochId}: rebuilt coverage root ` +
        `${snapshot.coverageRoot}, registry holds ${commitment.coverageRoot}`,
    );
  }
}

/** Fetches a snapshot from an http(s) uri. `ipfs://` needs a gateway url instead. */
export async function fetchSnapshot(uri: string): Promise<string> {
  if (uri === "") {
    throw new Error(
      "this epoch was published with no uri, so the chain does not say where its " +
        "table is; pass a mirror's url explicitly",
    );
  }
  if (uri.startsWith("ipfs://")) {
    throw new Error(`ipfs:// is not fetched directly; pass a gateway url for ${uri}`);
  }
  const response = await fetch(uri);
  if (!response.ok) {
    throw new Error(`fetch ${uri}: ${response.status} ${response.statusText}`);
  }
  return response.text();
}
