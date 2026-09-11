/**
 * The commitment encoding, in TypeScript.
 *
 * This file is a port, not a design. Every function here has to produce the same
 * bytes as its counterpart in `internal/merkle` (Go) and `HintRegistry` (Solidity);
 * a mirror whose keccak disagrees does not fail, it computes a different root and
 * reports the publisher as dishonest. So the port is checked against vectors the Go
 * side generates — see ../testdata and test/parity.test.ts.
 *
 * Nothing in here touches the network or the database. That is deliberate: it is the
 * part that has to be right, so it is the part with no dependencies.
 */

import { keccak_256 } from "@noble/hashes/sha3";

/** A 0x-prefixed hex string. Case is not significant; `normalizeHex` settles it. */
export type Hex = string;

const ADDRESS_BYTES = 20;
const WORD = 32;

/**
 * Lowercases and validates a hex string of `bytes` length.
 *
 * Case matters more than it looks: a snapshot document writes addresses lowercase
 * (Go's `common.Address` JSON) while the manifest writes them EIP-55 checksummed
 * (`.Hex()`). Comparing or deduping those as strings would treat one address as two.
 */
export function normalizeHex(value: Hex, bytes?: number): Hex {
  if (typeof value !== "string" || !value.startsWith("0x")) {
    throw new Error(`not a 0x-prefixed hex string: ${String(value)}`);
  }
  const body = value.slice(2);
  if (!/^[0-9a-fA-F]*$/.test(body)) {
    throw new Error(`not hex: ${value}`);
  }
  if (body.length % 2 !== 0) {
    throw new Error(`odd-length hex: ${value}`);
  }
  if (bytes !== undefined && body.length !== bytes * 2) {
    throw new Error(`expected ${bytes} bytes, got ${body.length / 2}: ${value}`);
  }
  return `0x${body.toLowerCase()}`;
}

export function hexToBytes(value: Hex, bytes?: number): Uint8Array {
  const body = normalizeHex(value, bytes).slice(2);
  const out = new Uint8Array(body.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = Number.parseInt(body.slice(i * 2, i * 2 + 2), 16);
  }
  return out;
}

export function bytesToHex(bytes: Uint8Array): Hex {
  let out = "0x";
  for (const b of bytes) {
    out += b.toString(16).padStart(2, "0");
  }
  return out;
}

export function keccak(bytes: Uint8Array): Hex {
  return bytesToHex(keccak_256(bytes));
}

/**
 * Writes a uint64 big-endian into the last 8 bytes of a 32-byte word at `offset`,
 * which is where `abi.encode` puts it.
 *
 * The argument is a bigint on purpose. A chain id, a block number or a timestamp
 * fits in a JS number today, but `Number` loses precision above 2^53 and the failure
 * is a wrong hash rather than an exception — so the type system refuses the question.
 */
function writeUint64(buf: Uint8Array, offset: number, value: bigint): void {
  if (value < 0n || value > 0xffffffffffffffffn) {
    throw new Error(`not a uint64: ${value}`);
  }
  for (let i = 0; i < 8; i++) {
    buf[offset + WORD - 1 - i] = Number((value >> BigInt(8 * i)) & 0xffn);
  }
}

/**
 * keccak256 over the account's asset addresses, packed ascending and deduped.
 *
 * Mirrors `abi.encodePacked(address[])`: 20 bytes each, no padding. Sorting makes
 * the digest independent of query order and dedup keeps a repeated address from
 * changing it — both are load-bearing, because the publisher's Postgres and a
 * mirror's SQLite will not agree on row order.
 */
export function assetsHash(assets: readonly Hex[]): Hex {
  const sorted = assets
    .map((a) => normalizeHex(a, ADDRESS_BYTES))
    .sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));

  const buf = new Uint8Array(sorted.length * ADDRESS_BYTES);
  let n = 0;
  let prev: string | undefined;
  for (const asset of sorted) {
    if (asset === prev) continue;
    buf.set(hexToBytes(asset, ADDRESS_BYTES), n * ADDRESS_BYTES);
    n++;
    prev = asset;
  }
  return keccak(buf.subarray(0, n * ADDRESS_BYTES));
}

/**
 * Mirrors `HintRegistry.leafHash`: keccak256(abi.encode(account, chainId, assetsHash)).
 *
 * Three 32-byte words: the address right-aligned, the uint64 right-aligned, the
 * digest as-is.
 */
export function leafHash(account: Hex, chainId: bigint, assets: Hex): Hex {
  const buf = new Uint8Array(3 * WORD);
  buf.set(hexToBytes(account, ADDRESS_BYTES), WORD - ADDRESS_BYTES);
  writeUint64(buf, WORD, chainId);
  buf.set(hexToBytes(assets, WORD), 2 * WORD);
  return keccak(buf);
}

/** Mirrors `HintRegistry.assetKey`: keccak256(abi.encodePacked(uint64 chainId, address)). */
export function assetKey(chainId: bigint, asset: Hex): Hex {
  const buf = new Uint8Array(8 + ADDRESS_BYTES);
  const word = new Uint8Array(WORD);
  writeUint64(word, 0, chainId);
  buf.set(word.subarray(WORD - 8), 0);
  buf.set(hexToBytes(asset, ADDRESS_BYTES), 8);
  return keccak(buf);
}

/**
 * Mirrors `HintRegistry.coverageLeaf`: keccak256(abi.encode(assetKey, fromBlock, toBlock)).
 *
 * One leaf per asset an epoch scanned, declaring the block range the publisher
 * stands behind for it.
 */
export function coverageLeaf(key: Hex, fromBlock: bigint, toBlock: bigint): Hex {
  const buf = new Uint8Array(3 * WORD);
  buf.set(hexToBytes(key, WORD), 0);
  writeUint64(buf, WORD, fromBlock);
  writeUint64(buf, 2 * WORD, toBlock);
  return keccak(buf);
}

/** Combines two nodes in ascending order, matching the Solidity verifier. */
export function hashPair(a: Hex, b: Hex): Hex {
  const [lo, hi] = compareHex(a, b) <= 0 ? [a, b] : [b, a];
  const buf = new Uint8Array(2 * WORD);
  buf.set(hexToBytes(lo, WORD), 0);
  buf.set(hexToBytes(hi, WORD), WORD);
  return keccak(buf);
}

function compareHex(a: Hex, b: Hex): number {
  const x = normalizeHex(a);
  const y = normalizeHex(b);
  return x < y ? -1 : x > y ? 1 : 0;
}

export const ZERO_HASH: Hex = `0x${"00".repeat(WORD)}`;

/**
 * Builds a root over leaves in the given order.
 *
 * An odd node at a level is promoted unchanged rather than paired with itself:
 * pairing a node with itself lets a proof for an internal node be replayed as a
 * proof for a leaf. An empty tree commits to the zero hash, which no leaf can
 * produce, so an empty commitment cannot be claimed to include anything.
 */
export function buildRoot(leaves: readonly Hex[]): Hex {
  if (leaves.length === 0) return ZERO_HASH;

  let level = leaves.map((l) => normalizeHex(l, WORD));
  while (level.length > 1) {
    const next: Hex[] = [];
    for (let i = 0; i < level.length; i += 2) {
      const left = level[i]!;
      const right = level[i + 1];
      next.push(right === undefined ? left : hashPair(left, right));
    }
    level = next;
  }
  return level[0]!;
}

/** Replays an inclusion proof, the way `HintRegistry.verifyInclusion` does. */
export function verifyInclusion(root: Hex, leaf: Hex, proof: readonly Hex[]): boolean {
  let node = normalizeHex(leaf, WORD);
  for (const sibling of proof) {
    node = hashPair(node, sibling);
  }
  return node === normalizeHex(root, WORD);
}
