/**
 * A verifiable local-first mirror of an evm-scan index commitment.
 *
 * The short version: `HintRegistry` holds a merkle root; this package holds the rows
 * that root commits to, in the client's own SQLite, synced by Evolu; and it rebuilds
 * the root locally to check them. So the thing serving the rows is trusted for
 * availability and nothing else — the same argument docs/RECOVERY.md makes for a
 * snapshot host, extended to a store that syncs deltas instead of downloading the
 * whole table each epoch.
 *
 * Read in this order:
 *
 * - `merkle` — the commitment encoding, ported from Go and pinned to its vectors.
 * - `snapshot` — reading the published table, rebuilding both roots from the rows.
 * - `state` — what a mirror stores: immutable `(account, sinceEpoch)` versions.
 * - `verify` — rebuilding a past epoch's roots and comparing with the chain.
 * - `registry` — reading the finalized commitment through an injected `eth_call`.
 * - `names` — ENS names to addresses (and back) through the same `eth_call`.
 * - `ingest` — snapshot in, version rows out, on the publisher's side.
 * - `store` — the persistence seam, and a `MemoryStore`.
 * - `evolu` — the sync binding. The only Evolu-aware file; see docs/LOCALFIRST.md.
 */

export {
  assetKey,
  assetsHash,
  buildRoot,
  bytesToHex,
  coverageLeaf,
  hashPair,
  hexToBytes,
  keccak,
  leafHash,
  normalizeHex,
  verifyInclusion,
  ZERO_HASH,
  type Hex,
} from "./merkle.js";

export {
  FORMAT,
  parseHeader,
  parseSnapshot,
  rootsOf,
  sortLeaves,
  type Coverage,
  type Leaf,
  type Snapshot,
  type SnapshotHeader,
} from "./snapshot.js";

export {
  diff,
  epochsPresent,
  resolveAsOf,
  rowKey,
  sameAssets,
  type VersionRow,
} from "./state.js";

export { verifyAsOf, type Commitment, type Mismatch, type VerifyResult } from "./verify.js";

export {
  decodeGetEpoch,
  decodeLatestFinalizedEpoch,
  encodeGetEpoch,
  encodeLatestFinalizedEpoch,
  EpochStatus,
  NoFinalizedEpochError,
  readCommitment,
  readSnapshotUri,
  selector,
  type Epoch,
  type EthCall,
  type LatestFinalized,
} from "./registry.js";

export {
  coinTypeFor,
  decodeResolveAddr,
  decodeReverse,
  dnsEncode,
  encodeResolveAddr,
  encodeReverse,
  NameNotFoundError,
  namehash,
  normalizeName,
  OffchainNameError,
  resolveName,
  ResolverError,
  revertData,
  ReverseMismatchError,
  reverseName,
  UNIVERSAL_RESOLVER,
  type Resolved,
  type ResolveOptions,
  type Reversed,
} from "./names.js";

export {
  fetchSnapshot,
  ingestSnapshot,
  previousEpoch,
  syncLatest,
  type IngestResult,
  type SyncOptions,
} from "./ingest.js";

export { keysOf, MemoryStore, type MirrorStore } from "./store.js";

export {
  createMirrorStore,
  MIRROR_TABLES,
  onRowsChanged,
  scopedRowId,
  scopeOf,
  type EvoluLike,
  type IdFromString,
  type MirrorStoreConfig,
} from "./evolu.js";
