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

import { normalizeHex, type Hex } from "./merkle.js";
import { readCommitment, readSnapshotUri, type EthCall } from "./registry.js";
import { parseSnapshot, type Snapshot } from "./snapshot.js";
import { diff, resolveAsOf, type VersionRow } from "./state.js";
import type { MirrorStore } from "./store.js";
import { verifyAsOf, type Commitment } from "./verify.js";

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

  // Read the rows back and verify them the way a wallet will, before declaring the
  // epoch done.
  //
  // The check above passed on the *document*, whose leaves are hashed in the order
  // they were written; this one passes on the *store*, whose leaves are hashed in
  // sorted account order. Those two orders agree today — leaves reach merkle.Build
  // in SnapshotIndex order (hintreg/publisher.go, `Index: i` over rows selected
  // `ORDER BY account`) and the snapshot endpoint streams them back `ORDER BY idx`.
  // But a merkle root is built from a sequence, so if they ever diverged, ingest
  // would accept the epoch and every wallet would fail it forever, blaming an
  // unfinished sync or a dishonest relay. Checking here costs the publisher one
  // root build per epoch and turns that into a loud failure on the machine that
  // can actually fix it.
  // Coverage is read back too, not reused from the document: a store that dropped
  // or reordered it would otherwise pass here and fail in every wallet, which is
  // the exact failure this check exists to catch.
  const check = verifyAsOf(
    await store.rows(),
    await store.coverage(commitment.epochId),
    commitment,
  );
  if (!check.ok) {
    throw new Error(
      `stored rows do not rebuild epoch ${commitment.epochId} (${check.mismatches.join(", ")}): ` +
        `the document verified but what was written from it did not — either the ` +
        `leaf order the publisher committed is not ascending by account, or the ` +
        `store did not keep the coverage rows as they were given`,
    );
  }

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
  // Normalized on both sides: a `Commitment` is a public type, and one built by
  // hand from a checksummed or uppercase root would otherwise never match a
  // rebuilt root, which is always lowercase.
  if (snapshot.root !== normalizeHex(commitment.root, 32)) {
    throw new Error(
      `snapshot does not match epoch ${commitment.epochId}: rebuilt root ` +
        `${snapshot.root}, registry holds ${commitment.root}`,
    );
  }
  if (snapshot.coverageRoot !== normalizeHex(commitment.coverageRoot, 32)) {
    throw new Error(
      `snapshot does not match epoch ${commitment.epochId}: rebuilt coverage root ` +
        `${snapshot.coverageRoot}, registry holds ${commitment.coverageRoot}`,
    );
  }
}

export interface SyncOptions {
  /** Where the registry lives. Note: `call` must reach *that* chain — see below. */
  registry: Hex;
  /** The chain the index is *about*, which is what the leaves commit to. */
  chainId: bigint;
  /**
   * An `eth_call` against the chain the **registry** is deployed on.
   *
   * That is not necessarily `chainId`. The live deployment indexes mainnet and keeps
   * its registry on Base, so a caller that pointed this at the indexed chain would
   * be calling an address with no code and reading an empty answer as "no epoch
   * yet". Confusing the two is the standing bug in this codebase.
   */
  call: EthCall;
  /** Overridable so tests need no network; defaults to `fetchSnapshot`. */
  fetch?: (uri: string) => Promise<string>;
  /**
   * Where to get the table, when the epoch was published before `uri` existed.
   * Those epochs carry `uri = ""` permanently, so the chain cannot say where to
   * look — but their roots are on chain, so a document from anywhere still checks.
   */
  snapshotUri?: string;
}

/**
 * The whole publisher-side flow: ask the chain what is finalized, fetch that
 * epoch's table, check it, and append the delta.
 *
 * This is the function to run on a timer. It returns what happened, including the
 * uninteresting case — an epoch already ingested does nothing and says so, so
 * calling this more often than epochs are published is free.
 */
export async function syncLatest(
  store: MirrorStore,
  options: SyncOptions,
): Promise<IngestResult & { commitment: Commitment }> {
  const commitment = await readCommitment(options.call, options.registry, options.chainId);

  const already = await store.ingestedEpochs();
  if (already.includes(commitment.epochId)) {
    return {
      commitment,
      epochId: commitment.epochId,
      wrote: false,
      rows: [],
      leaves: 0,
      changed: 0,
      tombstoned: 0,
    };
  }

  const uri =
    options.snapshotUri ??
    (await readSnapshotUri(options.call, options.registry, commitment.epochId));
  const document = await (options.fetch ?? fetchSnapshot)(uri);

  return { commitment, ...(await ingestSnapshot(store, document, commitment)) };
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
