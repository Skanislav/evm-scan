// ensrec.js — the ENS text records an account's index is served under, and the calls
// that read them.
//
// Two pages use this and neither can see the other's scope: hints.js keeps a browser's
// memory of an account in this format, and read.js reads the registry's answer for an
// account from its ENS name. The format therefore lives here and only here, and it is
// the one HintResolver.sol serves:
//
//   evmscan.contracts   lowercase 0x-prefixed addresses joined by commas, sorted, unique
//                       (HintResolver.contractsText renders the merkle leaf's sorted
//                       asset list the same way)
//   evmscan.chain       the chain id those contracts live on, in decimal
//   evmscan.epoch, evmscan.range, evmscan.root
//                       which commitment answered
//
// No imports. The one hash this needs, keccak256 for a namehash, is handed in by the
// caller so that the module works wherever the caller already has one and never pulls
// a second copy of viem onto a page.

export const KEY_CONTRACTS = 'evmscan.contracts';
export const KEY_CHAIN = 'evmscan.chain';
export const KEY_EPOCH = 'evmscan.epoch';
export const KEY_RANGE = 'evmscan.range';
export const KEY_ROOT = 'evmscan.root';

// The ENS registry and Universal Resolver, at the same addresses on every chain ENS
// deploys to.
export const ENS_REGISTRY = '0x00000000000C2E074eC69A0dFb2997BA6C7d2e1e';
export const UNIVERSAL_RESOLVER = '0xeEeEEEeE14D718C2B47D9923Deab1335E144EeEe';

// Function selectors, fixed here rather than derived: a page that has to hash a
// signature to make a call needs keccak before it can do anything at all.
export const SEL_RESOLVER = '0x0178b8bf';   // ENS.resolver(bytes32)
export const SEL_OWNER = '0x02571be3';      // ENS.owner(bytes32)
export const SEL_TEXT = '0x59d1d43c';       // text(bytes32,string)
export const SEL_RESOLVE = '0x9061b923';    // UniversalResolver.resolve(bytes,bytes)

// Revert selectors a read can come back with.
export const SEL_RESOLVER_NOT_FOUND = '0x77209fe8';
export const SEL_OFFCHAIN_LOOKUP = '0x556f1830';

const ADDRESS_RE = /^0x[0-9a-fA-F]{40}$/;

// ---------------------------------------------------------------------------
// The list itself
// ---------------------------------------------------------------------------

// formatContracts renders a set of addresses as the record value. Lowercased, made
// unique and sorted so the same holdings always produce the same string, whatever
// order the page happened to have them in.
export function formatContracts(addrs) {
  const seen = new Set();
  for (const a of addrs || []) {
    if (!ADDRESS_RE.test(a)) throw new Error(`not an address: ${a}`);
    seen.add(a.toLowerCase());
  }
  return [...seen].sort().join(',');
}

// parseContracts is the inverse. It refuses a malformed entry rather than skipping
// it: a record that half-parses would show a wallet missing things without saying so,
// and "entry 7 is not an address" is a complaint a person can act on.
export function parseContracts(s) {
  const text = (s || '').trim();
  if (!text) return [];
  const out = [];
  const seen = new Set();
  text.split(',').forEach((raw, i) => {
    const a = raw.trim();
    if (!ADDRESS_RE.test(a)) throw new Error(`entry ${i + 1} of the ${KEY_CONTRACTS} record is not an address: "${a.slice(0, 48)}"`);
    const k = a.toLowerCase();
    if (!seen.has(k)) { seen.add(k); out.push(k); }
  });
  return out;
}

// parseChain reads the decimal chain id; null when the record is absent or not a
// number, so a caller can fall back to a default rather than to NaN.
export function parseChain(s) {
  const t = (s || '').trim();
  return /^[0-9]{1,20}$/.test(t) ? Number(t) : null;
}

// ---------------------------------------------------------------------------
// Names
// ---------------------------------------------------------------------------

// NFC + lowercase, one trailing dot trimmed, no empty or blank labels. The same
// transform as internal/ens.Normalize and the mirror's normalizeName, and deliberately
// not ENSIP-15; the form actually hashed is the one to echo on screen.
export function normalizeName(name) {
  const n = String(name || '').trim().replace(/\.$/, '').normalize('NFC').toLowerCase();
  if (!n) throw new Error('empty name');
  const labels = n.split('.');
  if (labels.some(l => !l || /\s/.test(l))) throw new Error(`"${name}" has an empty or blank label`);
  return n;
}

// The name HintResolver serves an account's index under: `<hex>.hints.<parent>`, the
// hex without its 0x, matching internal/ens.HintName.
export function hintName(account, parent) {
  if (!ADDRESS_RE.test(account)) throw new Error(`not an address: ${account}`);
  return `${account.slice(2).toLowerCase()}.hints.${normalizeName(parent)}`;
}

// namehash and DNS wire encoding of a normalized name. `keccak` takes bytes and
// returns 0x-hex.
export function namehash(keccak, norm) {
  let node = new Uint8Array(32);
  const enc = new TextEncoder();
  for (const l of norm.split('.').reverse()) {
    const label = hexToBytes(keccak(enc.encode(l)));
    const both = new Uint8Array(64);
    both.set(node, 0);
    both.set(label, 32);
    node = hexToBytes(keccak(both));
  }
  return bytesToHex(node);
}

export function dnsEncode(norm) {
  const enc = new TextEncoder();
  const parts = [];
  for (const l of norm.split('.')) {
    const b = enc.encode(l);
    if (b.length > 63) throw new Error(`label "${l}" is longer than 63 bytes`);
    parts.push(new Uint8Array([b.length]), b);
  }
  parts.push(new Uint8Array([0]));
  return bytesToHex(concat(parts));
}

// ---------------------------------------------------------------------------
// ABI, by hand
//
// Two calls with string and bytes arguments is not a dependency's worth of work, and
// doing it here keeps the module free of one.
// ---------------------------------------------------------------------------

const word = (n) => BigInt(n).toString(16).padStart(64, '0');
const strip = (h) => String(h).replace(/^0x/, '');

// A dynamic bytes/string tail: length word then the bytes, padded to 32.
function tail(bytes) {
  const padded = new Uint8Array(Math.ceil(bytes.length / 32) * 32);
  padded.set(bytes);
  return word(bytes.length) + strip(bytesToHex(padded));
}

// encodeDynamic lays out `items` — each {static: hex-word} or {dynamic: bytes} — as
// an ABI tuple: heads first, tails after, offsets relative to the tuple start.
function encodeDynamic(items) {
  let head = '';
  let tails = '';
  let offset = items.length * 32;
  for (const it of items) {
    if (it.dynamic) {
      head += word(offset);
      const t = tail(it.dynamic);
      tails += t;
      offset += t.length / 2;
    } else {
      head += strip(it.static);
    }
  }
  return head + tails;
}

const utf8 = (s) => new TextEncoder().encode(s);

export function encodeText(node, key) {
  return SEL_TEXT + encodeDynamic([{ static: word('0x' + strip(node)) }, { dynamic: utf8(key) }]);
}

// resolve(bytes name, bytes data) on the Universal Resolver.
export function encodeResolve(dnsName, inner) {
  return SEL_RESOLVE + encodeDynamic([{ dynamic: hexToBytes(dnsName) }, { dynamic: hexToBytes(inner) }]);
}

// decodeString reads one ABI-encoded string return value.
export function decodeString(out) {
  const body = strip(out);
  if (body.length < 128) return '';
  const off = Number(BigInt('0x' + body.slice(0, 64)));
  const len = Number(BigInt('0x' + body.slice(off * 2, off * 2 + 64)));
  if (!len) return '';
  const hex = body.slice(off * 2 + 64, off * 2 + 64 + len * 2);
  return new TextDecoder().decode(hexToBytes('0x' + hex));
}

// decodeBytes reads one ABI-encoded bytes return value, as a resolver's callback
// returns the profile's result when it is called directly rather than through the
// Universal Resolver.
export function decodeBytes(out) {
  const body = strip(out);
  if (body.length < 128) return '0x';
  const off = Number(BigInt('0x' + body.slice(0, 64)));
  const len = Number(BigInt('0x' + body.slice(off * 2, off * 2 + 64)));
  return '0x' + body.slice(off * 2 + 64, off * 2 + 64 + len * 2);
}

// decodeResolve reads the Universal Resolver's (bytes result, address resolver) and
// returns the inner result still encoded, for decodeString.
export function decodeResolve(out) {
  const body = strip(out);
  const off = Number(BigInt('0x' + body.slice(0, 64)));
  const len = Number(BigInt('0x' + body.slice(off * 2, off * 2 + 64)));
  const result = '0x' + body.slice(off * 2 + 64, off * 2 + 64 + len * 2);
  const resolver = '0x' + body.slice(64 + 24, 128);
  return { result, resolver };
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// readText reads one text record through the Universal Resolver, which is the call
// any ENS client makes. `call(to, data)` is one eth_call on whichever RPC the caller
// chose; the revert data it raises is what tells a missing name from an offchain one.
//
// It does not follow OffchainLookup. It throws with `offchain: true` and the revert
// data, and the caller decides whether following a gateway is its reader's choice.
export async function readText({ call, keccak }, name, key) {
  const norm = normalizeName(name);
  const node = namehash(keccak, norm);
  const data = encodeResolve(dnsEncode(norm), encodeText(node, key));
  let out;
  try {
    out = await call(UNIVERSAL_RESOLVER, data);
  } catch (e) {
    const d = String((e && e.data) || '');
    const m = String((e && e.message) || '');
    if (d.startsWith(SEL_RESOLVER_NOT_FOUND) || m.includes(SEL_RESOLVER_NOT_FOUND)) {
      throw Object.assign(new Error(`${norm} has no resolver`), { notFound: true, name: norm });
    }
    if (d.startsWith(SEL_OFFCHAIN_LOOKUP) || m.includes(SEL_OFFCHAIN_LOOKUP)) {
      throw Object.assign(new Error(`${norm} answers ${key} through an offchain gateway`), { offchain: true, name: norm, data: d });
    }
    throw e;
  }
  const { result, resolver } = decodeResolve(out);
  return { value: decodeString(result), resolver, name: norm, node };
}

// ---------------------------------------------------------------------------
// bytes
// ---------------------------------------------------------------------------

export function hexToBytes(h) {
  const s = strip(h);
  if (s.length % 2) throw new Error('odd-length hex');
  const out = new Uint8Array(s.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(s.substr(i * 2, 2), 16);
  return out;
}

export function bytesToHex(b) {
  let s = '0x';
  for (const x of b) s += x.toString(16).padStart(2, '0');
  return s;
}

function concat(parts) {
  const n = parts.reduce((a, p) => a + p.length, 0);
  const out = new Uint8Array(n);
  let o = 0;
  for (const p of parts) { out.set(p, o); o += p.length; }
  return out;
}
