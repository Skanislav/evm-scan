/**
 * ENS names, resolved through the client's own `eth_call`.
 *
 * A mirror already talks to the chain through an injected `EthCall` to read the
 * committed root (registry.ts). Names go through the same seam: one call to ENS's
 * Universal Resolver — the same contract at the same address on mainnet (ENSv1) and
 * Sepolia (ENSv2), speaking the same interface — and the name has become an address.
 * That is the whole contract with the rest of this package: nothing here ever
 * touches a row, and the daemon is not involved at all.
 *
 * Two things are deliberately not done. An `OffchainLookup` revert (a CCIP-Read
 * name) is reported with its gateway URLs and not followed; whether to trust a
 * third-party gateway is the client's call, not this library's. And normalization
 * is NFC + lowercase, not ENSIP-15 — the same transform as `internal/ens.Normalize`
 * and the page, so the three agree with each other, and the normalized form is
 * returned alongside the answer so a name the ENS app would render differently is
 * visible.
 *
 * ABI work is by hand, as in registry.ts, and pinned to blobs go-ethereum packed
 * from the Universal Resolver ABI — see ../testdata/names.json.
 */

import { bytesToHex, hexToBytes, keccak, normalizeHex, type Hex } from "./merkle.js";
import type { EthCall } from "./registry.js";

/** ENS deploys the Universal Resolver at this address on every chain it exists on. */
export const UNIVERSAL_RESOLVER: Hex = "0xeeeeeeee14d718c2b47d9923deab1335e144eeee";

const WORD = 32;
const ZERO_NODE: Hex = `0x${"00".repeat(WORD)}`;

/** Selectors, derived from signatures so a typo fails loudly. */
const SEL = {
  resolve: sel("resolve(bytes,bytes)"),
  reverse: sel("reverse(bytes,uint256)"),
  addr: sel("addr(bytes32)"),
  resolverNotFound: sel("ResolverNotFound(bytes)"),
  resolverNotContract: sel("ResolverNotContract(bytes,address)"),
  unsupportedProfile: sel("UnsupportedResolverProfile(bytes4)"),
  resolverError: sel("ResolverError(bytes)"),
  reverseMismatch: sel("ReverseAddressMismatch(string,bytes)"),
  offchainLookup: sel("OffchainLookup(address,string[],bytes,bytes4,bytes)"),
} as const;

function sel(signature: string): Hex {
  return keccak(new TextEncoder().encode(signature)).slice(0, 10);
}

// --------------------------------------------------------------------------
// Names
// --------------------------------------------------------------------------

/**
 * NFC + lowercase, one trailing dot trimmed, no empty or blank labels, no label over
 * 63 bytes (the DNS wire limit). Mirrors `internal/ens.Normalize`.
 */
export function normalizeName(name: string): string {
  const n = name.trim().replace(/\.$/, "").normalize("NFC");
  if (!n) throw new Error("ens: empty name");
  if (n.length > 512) throw new Error("ens: name longer than 512 characters");
  const enc = new TextEncoder();
  const labels = n.split(".").map((l) => {
    if (!l) throw new Error(`ens: empty label in "${name}"`);
    if (/[\s\p{Cc}]/u.test(l)) {
      throw new Error(`ens: label "${l}" contains whitespace or a control character`);
    }
    if (enc.encode(l).length > 63) throw new Error(`ens: label "${l}" is longer than 63 bytes`);
    return l.toLowerCase();
  });
  return labels.join(".");
}

/** ENSIP-1 namehash of an already normalized name; "" is the zero node. */
export function namehash(name: string): Hex {
  let node = ZERO_NODE;
  if (name === "") return node;
  const enc = new TextEncoder();
  for (const label of name.split(".").reverse()) {
    const labelHash = keccak(enc.encode(label));
    node = keccak(concat(hexToBytes(node), hexToBytes(labelHash)));
  }
  return node;
}

/** ENSIP-10 wire format: each label length-prefixed, then a zero byte. */
export function dnsEncode(name: string): Uint8Array {
  const enc = new TextEncoder();
  const parts: Uint8Array[] = [];
  if (name !== "") {
    for (const label of name.split(".")) {
      const b = enc.encode(label);
      if (b.length === 0 || b.length > 63) {
        throw new Error(`ens: label "${label}" is not 1..63 bytes`);
      }
      parts.push(Uint8Array.of(b.length), b);
    }
  }
  parts.push(Uint8Array.of(0));
  return concat(...parts);
}

/** ENSIP-19: mainnet and its testnets use coin type 60; every other chain derives one. */
export function coinTypeFor(chainId: bigint): bigint {
  return chainId === 1n || chainId === 11155111n ? 60n : 0x80000000n | chainId;
}

// --------------------------------------------------------------------------
// ABI
// --------------------------------------------------------------------------

function concat(...parts: Uint8Array[]): Uint8Array {
  let n = 0;
  for (const p of parts) n += p.length;
  const out = new Uint8Array(n);
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

function word(value: bigint): Uint8Array {
  if (value < 0n || value >= 1n << 256n) throw new Error(`does not fit in a word: ${value}`);
  return hexToBytes(`0x${value.toString(16).padStart(64, "0")}`);
}

/** `bytes`: length word, then the data padded to a whole number of words. */
function dynamicBytes(b: Uint8Array): Uint8Array {
  const padded = new Uint8Array(Math.ceil(b.length / WORD) * WORD);
  padded.set(b);
  return concat(word(BigInt(b.length)), padded);
}

/** `resolve(bytes name, bytes data)` with `data` = `addr(bytes32 node)`. */
export function encodeResolveAddr(normalizedName: string): Hex {
  const inner = concat(hexToBytes(SEL.addr), hexToBytes(namehash(normalizedName)));
  return encodeResolve(dnsEncode(normalizedName), inner);
}

function encodeResolve(dns: Uint8Array, data: Uint8Array): Hex {
  const first = dynamicBytes(dns);
  const head = concat(word(BigInt(2 * WORD)), word(BigInt(2 * WORD + first.length)));
  return bytesToHex(concat(hexToBytes(SEL.resolve), head, first, dynamicBytes(data)));
}

/** `reverse(bytes lookupAddress, uint256 coinType)`. */
export function encodeReverse(address: Hex, coinType: bigint): Hex {
  const addr = hexToBytes(normalizeHex(address, 20));
  const head = concat(word(BigInt(2 * WORD)), word(coinType));
  return bytesToHex(concat(hexToBytes(SEL.reverse), head, dynamicBytes(addr)));
}

/** A view over ABI-encoded data that reads words by index. */
class Words {
  private readonly bytes: Uint8Array;

  constructor(data: Hex | Uint8Array) {
    this.bytes = typeof data === "string" ? hexToBytes(data) : data;
    if (this.bytes.length % WORD !== 0) {
      throw new Error(`abi: ${this.bytes.length} bytes is not a whole number of words`);
    }
  }

  word(i: number): Uint8Array {
    if (i < 0 || (i + 1) * WORD > this.bytes.length) {
      throw new Error(`abi: asked for word ${i} of ${this.bytes.length / WORD}`);
    }
    return this.bytes.subarray(i * WORD, (i + 1) * WORD);
  }

  uint(i: number): bigint {
    let out = 0n;
    for (const b of this.word(i)) out = (out << 8n) | BigInt(b);
    return out;
  }

  address(i: number): Hex {
    return bytesToHex(this.word(i).subarray(WORD - 20));
  }

  /** The dynamic value whose byte offset (from the start of this view) is word `i`. */
  dynamic(i: number): Uint8Array {
    const offset = this.uint(i);
    if (offset % BigInt(WORD) !== 0n || offset >= BigInt(this.bytes.length)) {
      throw new Error(`abi: offset ${offset} is not a word inside ${this.bytes.length} bytes`);
    }
    const start = Number(offset);
    const length = new Words(this.bytes.subarray(start, start + WORD)).uint(0);
    const end = start + WORD + Number(length);
    if (end > this.bytes.length) throw new Error(`abi: dynamic value runs past the end`);
    return this.bytes.subarray(start + WORD, end);
  }

  string(i: number): string {
    return new TextDecoder().decode(this.dynamic(i));
  }

  strings(i: number): string[] {
    const offset = Number(this.uint(i));
    const arr = new Words(this.bytes.subarray(offset));
    const n = Number(arr.uint(0));
    // Element offsets are relative to the start of the array body (after the length).
    const body = new Words(this.bytes.subarray(offset + WORD));
    const out: string[] = [];
    for (let k = 0; k < n; k++) out.push(body.string(k));
    return out;
  }
}

/** `resolve`'s `(bytes result, address resolver)`, with `result` = `abi.encode(address)`. */
export function decodeResolveAddr(data: Hex): { address: Hex; resolver: Hex } {
  const w = new Words(data);
  const result = w.dynamic(0);
  if (result.length !== WORD) {
    throw new Error(`ens: addr() answered ${result.length} bytes, not one word`);
  }
  return { address: bytesToHex(result.subarray(WORD - 20)), resolver: w.address(1) };
}

/** `reverse`'s `(string primary, address resolver, address reverseResolver)`. */
export function decodeReverse(data: Hex): { name: string; resolver: Hex; reverseResolver: Hex } {
  const w = new Words(data);
  return { name: w.string(0), resolver: w.address(1), reverseResolver: w.address(2) };
}

// --------------------------------------------------------------------------
// Errors
// --------------------------------------------------------------------------

/** The name has no resolver, or its resolver answers with the zero address. */
export class NameNotFoundError extends Error {
  constructor(readonly name: string) {
    super(`ens: ${name} does not resolve to an address`);
    this.name = "NameNotFoundError";
  }
}

/**
 * The name lives behind an ERC-3668 gateway. Reported, never followed: the URLs are
 * here so a client that decides to trust one can, with its own HTTP client.
 */
export class OffchainNameError extends Error {
  constructor(
    readonly name: string,
    readonly urls: readonly string[],
  ) {
    super(`ens: ${name} resolves offchain (CCIP-Read) via ${urls.join(", ")}`);
    this.name = "OffchainNameError";
  }
}

/** The address claims a primary name whose forward record points elsewhere. */
export class ReverseMismatchError extends Error {
  constructor(
    readonly address: Hex,
    readonly primary: string,
  ) {
    super(`ens: ${address} claims "${primary}", which does not resolve back to it`);
    this.name = "ReverseMismatchError";
  }
}

/** Any other revert the Universal Resolver raised, by selector. */
export class ResolverError extends Error {
  constructor(
    readonly selector: Hex,
    readonly data: Hex,
  ) {
    super(`ens: universal resolver reverted with ${selector}`);
    this.name = "ResolverError";
  }
}

/**
 * The revert data a failed `eth_call` carried, if the caller's transport exposed it.
 *
 * Transports differ: a raw JSON-RPC client has it at `error.data`, viem nests it, some
 * only quote it in the message. This looks in the usual places and, failing those,
 * for the first hex blob in the message. Null means "not a revert I can read", which
 * callers treat as the endpoint failing rather than the resolver answering.
 */
export function revertData(err: unknown): Hex | null {
  const seen = new Set<unknown>();
  const visit = (e: unknown, depth: number): Hex | null => {
    if (!e || typeof e !== "object" || seen.has(e) || depth > 4) return null;
    seen.add(e);
    const o = e as Record<string, unknown>;
    for (const key of ["data", "error", "cause"]) {
      const v = o[key];
      if (typeof v === "string" && /^0x[0-9a-fA-F]{8,}$/.test(v)) return v.toLowerCase() as Hex;
      const inner = visit(v, depth + 1);
      if (inner) return inner;
    }
    if (typeof o["message"] === "string") {
      const m = /0x[0-9a-fA-F]{8,}/.exec(o["message"]);
      if (m) return m[0].toLowerCase() as Hex;
    }
    return null;
  };
  return visit(err, 0);
}

function classify(err: unknown, name: string, address: Hex): Error | null {
  const data = revertData(err);
  if (!data) return null;
  const selector = data.slice(0, 10) as Hex;
  const body = `0x${data.slice(10)}` as Hex;
  switch (selector) {
    case SEL.resolverNotFound:
    case SEL.resolverNotContract:
      return new NameNotFoundError(name);
    case SEL.offchainLookup: {
      // OffchainLookup(address sender, string[] urls, bytes callData, bytes4 callback, bytes extra)
      let urls: string[] = [];
      try {
        urls = new Words(body).strings(1);
      } catch {
        /* malformed: report without URLs rather than not at all */
      }
      return new OffchainNameError(name, urls);
    }
    case SEL.reverseMismatch: {
      let primary = "";
      try {
        primary = new Words(body).string(0);
      } catch {
        /* as above */
      }
      return new ReverseMismatchError(address, primary);
    }
    default:
      return new ResolverError(selector, data);
  }
}

// --------------------------------------------------------------------------
// Resolution
// --------------------------------------------------------------------------

export interface ResolveOptions {
  /** Override the Universal Resolver address; the canonical one otherwise. */
  universalResolver?: Hex;
}

export interface Resolved {
  /** The account, lowercase hex. */
  address: Hex;
  /** The form that was hashed — echoed so a normalization surprise is visible. */
  name: string;
  /** The resolver the Universal Resolver found for the name. */
  resolver: Hex;
}

/**
 * Forward resolution: the address a name points at, through `call`.
 *
 * Throws `NameNotFoundError`, `OffchainNameError` or `ResolverError` when the chain
 * answered; anything else is the transport's own failure, rethrown as is.
 */
export async function resolveName(
  call: EthCall,
  name: string,
  opts: ResolveOptions = {},
): Promise<Resolved> {
  const norm = normalizeName(name);
  const to = normalizeHex(opts.universalResolver ?? UNIVERSAL_RESOLVER, 20);
  let out: Hex;
  try {
    out = await call(to, encodeResolveAddr(norm));
  } catch (err) {
    throw classify(err, norm, "0x") ?? err;
  }
  const { address, resolver } = decodeResolveAddr(out);
  if (/^0x0{40}$/.test(address)) throw new NameNotFoundError(norm);
  return { address, name: norm, resolver };
}

export interface Reversed {
  /** The primary name, already confirmed by the Universal Resolver to point back. */
  name: string;
  resolver: Hex;
  reverseResolver: Hex;
}

/**
 * Reverse resolution: the primary name an address claims on `chainId`, or null when
 * it claims none. The Universal Resolver checks the forward record itself and reverts
 * `ReverseAddressMismatch` when it disagrees, which surfaces as `ReverseMismatchError`
 * — a claim the chain contradicts, not the same thing as no claim.
 */
export async function reverseName(
  call: EthCall,
  address: Hex,
  chainId: bigint,
  opts: ResolveOptions = {},
): Promise<Reversed | null> {
  const addr = normalizeHex(address, 20);
  const to = normalizeHex(opts.universalResolver ?? UNIVERSAL_RESOLVER, 20);
  let out: Hex;
  try {
    out = await call(to, encodeReverse(addr, coinTypeFor(chainId)));
  } catch (err) {
    const known = classify(err, addr, addr);
    if (known instanceof NameNotFoundError) return null;
    throw known ?? err;
  }
  const r = decodeReverse(out);
  return r.name === "" ? null : r;
}
