/**
 * Reading the committed root off the chain.
 *
 * This is the half of the trust argument that cannot be delegated. A mirror that
 * accepted a root from the service that served it the rows would be verifying the
 * publisher against the publisher; the root has to come from `HintRegistry` through
 * a node the client chose. So the transport is injected: pass anything that can do
 * an `eth_call` — a wallet provider, viem, a raw fetch to an RPC, the user's own
 * node — and this encodes and decodes the two calls it needs.
 *
 * The ABI work is done by hand rather than with a library, for one reason: it is
 * two calls, and adding a dependency here would put a package between the client
 * and the only number it must not get wrong. Both are pinned to blobs go-ethereum
 * packed from the compiled contract — see ../testdata/registry.json.
 */

import { bytesToHex, hexToBytes, keccak, normalizeHex, type Hex } from "./merkle.js";
import type { Commitment } from "./verify.js";

/** An `eth_call`, however the caller wants to make one. */
export type EthCall = (to: Hex, data: Hex) => Promise<Hex>;

/** `HintRegistry.EpochStatus`. Only `Finalized` is safe to build on. */
export enum EpochStatus {
  None = 0,
  Proposed = 1,
  Challenged = 2,
  Finalized = 3,
  Rejected = 4,
}

export interface Epoch {
  chainId: bigint;
  fromBlock: bigint;
  toBlock: bigint;
  root: Hex;
  coverageRoot: Hex;
  /** Where the full table was published: an ipfs:// or https:// pointer, or "". */
  uri: string;
  publisher: Hex;
  challenger: Hex;
  bond: bigint;
  publishedAt: bigint;
  challengeDeadline: bigint;
  status: EpochStatus;
  assertionId: Hex;
}

const WORD = 32;

/** The first four bytes of keccak256 over the canonical signature. */
export function selector(signature: string): Hex {
  return keccak(new TextEncoder().encode(signature)).slice(0, 10);
}

function padWord(value: bigint): string {
  if (value < 0n) throw new Error(`not a uint: ${value}`);
  const hex = value.toString(16);
  if (hex.length > 64) throw new Error(`does not fit in a word: ${value}`);
  return hex.padStart(64, "0");
}

export function encodeLatestFinalizedEpoch(chainId: bigint): Hex {
  // The parameter is a uint64. Padding a wider value into the word would produce
  // calldata the contract reverts on, reported as a node error rather than as the
  // caller's out-of-range chain id.
  if (chainId < 0n || chainId > 0xffffffffffffffffn) {
    throw new Error(`not a uint64 chain id: ${chainId}`);
  }
  return `${selector("latestFinalizedEpoch(uint64)")}${padWord(chainId)}`;
}

export function encodeGetEpoch(epochId: bigint): Hex {
  return `${selector("getEpoch(uint256)")}${padWord(epochId)}`;
}

/** A view over returndata that reads words by index rather than by byte offset. */
class Words {
  private readonly bytes: Uint8Array;

  constructor(data: Hex) {
    this.bytes = hexToBytes(data);
    if (this.bytes.length % WORD !== 0) {
      throw new Error(`returndata is ${this.bytes.length} bytes, not a whole number of words`);
    }
  }

  get length(): number {
    return this.bytes.length / WORD;
  }

  word(i: number): Uint8Array {
    if (i < 0 || i >= this.length) {
      throw new Error(`returndata has ${this.length} words, asked for word ${i}`);
    }
    return this.bytes.subarray(i * WORD, (i + 1) * WORD);
  }

  uint(i: number): bigint {
    let out = 0n;
    for (const b of this.word(i)) out = (out << 8n) | BigInt(b);
    return out;
  }

  bool(i: number): boolean {
    return this.uint(i) !== 0n;
  }

  hash(i: number): Hex {
    return bytesToHex(this.word(i));
  }

  address(i: number): Hex {
    return bytesToHex(this.word(i).subarray(WORD - 20));
  }

  /**
   * A byte offset, converted to a word index.
   *
   * Dynamic components are located by an offset in bytes relative to the start of
   * the enclosing tuple's head — not to the start of the returndata. Getting that
   * base wrong is the mistake that yields a plausible wrong root, which is why the
   * base is always passed in explicitly here.
   */
  offsetWords(i: number, baseWord: number): number {
    const bytes = this.uint(i);
    if (bytes % BigInt(WORD) !== 0n) throw new Error(`offset ${bytes} is not word-aligned`);
    return baseWord + Number(bytes / BigInt(WORD));
  }

  /** A `string`, read from its length word and following bytes. */
  utf8(atWord: number): string {
    const length = Number(this.uint(atWord));
    const start = (atWord + 1) * WORD;
    if (start + length > this.bytes.length) {
      throw new Error(`string of ${length} bytes runs past the end of the returndata`);
    }
    return new TextDecoder().decode(this.bytes.subarray(start, start + length));
  }
}

/**
 * Decodes a `HintRegistry.Epoch` tuple whose head starts at `baseWord`.
 *
 * The tuple is dynamic because of `uri`, so it is reached through an offset and its
 * own `uri` offset is relative to the tuple, not to the call's returndata.
 */
function decodeEpochAt(w: Words, baseWord: number): Epoch {
  const at = (i: number): number => baseWord + i;
  return {
    chainId: w.uint(at(0)),
    fromBlock: w.uint(at(1)),
    toBlock: w.uint(at(2)),
    root: w.hash(at(3)),
    coverageRoot: w.hash(at(4)),
    uri: w.utf8(w.offsetWords(at(5), baseWord)),
    publisher: w.address(at(6)),
    challenger: w.address(at(7)),
    bond: w.uint(at(8)),
    publishedAt: w.uint(at(9)),
    challengeDeadline: w.uint(at(10)),
    status: Number(w.uint(at(11))) as EpochStatus,
    assertionId: w.hash(at(12)),
  };
}

export interface LatestFinalized {
  found: boolean;
  epochId: bigint;
  epoch: Epoch | null;
}

/**
 * Decodes `latestFinalizedEpoch(uint64) -> (bool, uint256, Epoch)`.
 *
 * `found` is checked before the struct is read, and the struct is not returned
 * without it. A registry with no finalized epoch for a chain answers with a zeroed
 * tuple — and a zero root is a root every empty mirror rebuilds, so a decoder that
 * skipped the flag would report a fresh, empty client as verified.
 */
export function decodeLatestFinalizedEpoch(data: Hex): LatestFinalized {
  const w = new Words(data);
  const found = w.bool(0);
  const epochId = w.uint(1);
  if (!found) return { found: false, epochId, epoch: null };
  return { found: true, epochId, epoch: decodeEpochAt(w, w.offsetWords(2, 0)) };
}

/** Decodes `getEpoch(uint256) -> Epoch`. */
export function decodeGetEpoch(data: Hex): Epoch {
  const w = new Words(data);
  return decodeEpochAt(w, w.offsetWords(0, 0));
}

export class NoFinalizedEpochError extends Error {
  constructor(chainId: bigint) {
    super(
      `the registry has no finalized epoch for chain ${chainId}: there is nothing ` +
        `to verify a mirror against yet`,
    );
    this.name = "NoFinalizedEpochError";
  }
}

/**
 * Fetches the commitment a mirror should be checked against.
 *
 * The latest *finalized* epoch, deliberately — a proposed one is inside its
 * challenge window and may still be rejected. `HintRegistry.contractsOfCallback`
 * makes the same choice, and for the same reason.
 */
export async function readCommitment(
  call: EthCall,
  registry: Hex,
  chainId: bigint,
): Promise<Commitment> {
  const to = normalizeHex(registry, 20);
  const answer = decodeLatestFinalizedEpoch(await call(to, encodeLatestFinalizedEpoch(chainId)));
  if (!answer.found || answer.epoch === null) throw new NoFinalizedEpochError(chainId);

  const epoch = answer.epoch;
  if (epoch.status !== EpochStatus.Finalized) {
    // latestFinalizedEpoch should never return anything else; if it does, the
    // deployment is not the contract this was written against.
    throw new Error(
      `epoch ${answer.epochId} is status ${epoch.status}, not finalized (${EpochStatus.Finalized})`,
    );
  }
  if (epoch.chainId !== chainId) {
    throw new Error(
      `asked for chain ${chainId} and got an epoch about chain ${epoch.chainId}`,
    );
  }

  return {
    epochId: answer.epochId,
    chainId: epoch.chainId,
    root: epoch.root,
    coverageRoot: epoch.coverageRoot,
  };
}

/** Where the epoch's full table was published, for a mirror that needs to bootstrap. */
export async function readSnapshotUri(
  call: EthCall,
  registry: Hex,
  epochId: bigint,
): Promise<string> {
  const epoch = decodeGetEpoch(await call(normalizeHex(registry, 20), encodeGetEpoch(epochId)));
  return epoch.uri;
}
