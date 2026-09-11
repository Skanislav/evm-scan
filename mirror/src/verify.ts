/**
 * Checking a mirror against the chain.
 *
 * This is the whole security argument in one function. A mirror's rows arrive from
 * a relay that is blind — it moves encrypted rows and fingerprints and cannot read
 * them — and from a publisher who is not trusted either. What makes the result
 * usable is that the client rebuilds the root itself and compares it with what
 * `HintRegistry` finalized. So the relay is an availability question and never an
 * integrity one, which is the same claim docs/RECOVERY.md makes about a snapshot
 * host, extended to a mirror that syncs deltas instead of downloading the table.
 *
 * What a match proves: these rows are exactly the ones that epoch committed.
 * What it does not prove: that the epoch's contents are *true*. The index is a hint
 * — a wallet uses it to learn which contracts are worth pulling history for, then
 * reads that history from a source it trusts. A finalized epoch is one nobody
 * challenged, not one anybody verified.
 */

import { type Hex, normalizeHex } from "./merkle.js";
import { rootsOf, type Coverage, type Leaf } from "./snapshot.js";
import { resolveAsOf, type VersionRow } from "./state.js";

/** What the registry says about a finalized epoch. */
export interface Commitment {
  /** The on-chain epoch id, from `latestFinalizedEpoch`. */
  epochId: bigint;
  /** The chain the index is *about*, which is what the leaf hash commits to. */
  chainId: bigint;
  root: Hex;
  coverageRoot: Hex;
}

export type Mismatch = "root" | "coverage-root";

export interface VerifyResult {
  ok: boolean;
  epochId: bigint;
  /** Rebuilt from the mirror's own rows. */
  root: Hex;
  coverageRoot: Hex;
  /** The resolved index at that epoch, in the publisher's leaf order. */
  leaves: Leaf[];
  mismatches: Mismatch[];
  /** Plain-language reading of the result, safe to show a user. */
  reason: string;
}

/**
 * Rebuilds the roots from a mirror's rows and compares them with the commitment.
 *
 * Both roots are checked. The index root alone would leave the asset set unchecked,
 * and the asset set is the part that cannot be recovered from the chain any other
 * way: an asset promoted locally never appears in `listAssets`, and `assetKey` is a
 * keccak that does not invert.
 */
export function verifyAsOf(
  rows: readonly VersionRow[],
  coverage: readonly Coverage[],
  commitment: Commitment,
): VerifyResult {
  const leaves = resolveAsOf(rows, commitment.epochId);
  const rebuilt = rootsOf(commitment.chainId, coverage, leaves);

  const mismatches: Mismatch[] = [];
  if (rebuilt.root !== normalizeHex(commitment.root, 32)) mismatches.push("root");
  if (rebuilt.coverageRoot !== normalizeHex(commitment.coverageRoot, 32)) {
    mismatches.push("coverage-root");
  }

  return {
    ok: mismatches.length === 0,
    epochId: commitment.epochId,
    root: rebuilt.root,
    coverageRoot: rebuilt.coverageRoot,
    leaves,
    mismatches,
    reason: explain(mismatches, leaves.length, commitment.epochId),
  };
}

/**
 * Says what the comparison means — including the part that cannot be determined.
 *
 * A mismatch has two causes that look identical from here: the mirror is still
 * syncing and is missing rows, or the mirror is serving rows the publisher never
 * committed. Nothing on the chain distinguishes them — `IndexPublished` carries no
 * leaf count, so there is no number to compare against — and inventing a
 * completeness signal would be worse than admitting there isn't one. The honest
 * move is to say both, and let a sync that has just finished be re-checked.
 */
function explain(mismatches: readonly Mismatch[], leaves: number, epochId: bigint): string {
  if (mismatches.length === 0) {
    return `verified: these ${leaves} accounts are exactly what epoch ${epochId} committed`;
  }
  const which =
    mismatches.length === 2
      ? "neither root matches"
      : mismatches[0] === "root"
        ? "the index root does not match"
        : "the coverage root does not match";
  return (
    `unverified at epoch ${epochId}: ${which}. Either this mirror has not finished ` +
    `syncing, or it is serving rows the publisher did not commit — the chain does ` +
    `not say which, so re-check once sync is idle before treating it as dishonest`
  );
}
