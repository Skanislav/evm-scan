// lensread.js — the client-side read, in one place.
//
// Two pages perform the same read against an RPC the reader named: read.js, the
// reference reader, and landing.html's lookup card. Both need the same four
// things — a JSON-RPC call, a name resolved through the Universal Resolver, the
// AssetLens artifact this origin serves, and a deployless eth_call that batches
// under EIP-170's reply ceiling. This module is that implementation; neither page
// carries its own.
//
// It knows nothing about either page's DOM on purpose: every function takes the
// RPC url it should talk to, so the choice of host stays with the caller, which
// is where the reader made it. index.html keeps a separate lens call for the
// cross-chain sweep, which batches across chains and has its own shape.

import * as R from './ensrec.js';

const VIEM_MODULE = 'https://esm.sh/viem@2.56.3?bundle';
let viemModule = null;

/** viem, imported on first use — it is most of a megabyte and a page that never
 *  performs a read never pays for it. */
export const viem = () => (viemModule ||= import(VIEM_MODULE));

export const DEFAULT_NAME_RPC = 'https://ethereum-rpc.publicnode.com';

export const esc = (s) => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
export const short = (a, n = 4) => a ? `${a.slice(0, 2 + n)}…${a.slice(-n)}` : '';
export const host = (u) => { try { return new URL(u).host; } catch { return u; } };
export const num = (n) => Number(n).toLocaleString('en-US');
export const ADDRESS_RE = /^0x[0-9a-fA-F]{40}$/;

export async function rpc(url, method, params) {
  const res = await fetch(url, {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method, params }),
  });
  if (!res.ok) throw new Error(`${method}: ${res.status} ${res.statusText}`);
  const body = await res.json();
  if (body.error) {
    throw Object.assign(new Error(`${method}: ${body.error.message || JSON.stringify(body.error)}`), { data: body.error.data });
  }
  return body.result;
}

// A typed name becomes an address through the Universal Resolver, on an RPC the
// caller names. That host sees the name; it does not see who asked.
export async function resolveAddress(name, rpcUrl) {
  const v = await viem();
  const norm = R.normalizeName(name);
  const inner = v.encodeFunctionData({
    abi: [{ type: 'function', name: 'addr', stateMutability: 'view', inputs: [{ name: 'node', type: 'bytes32' }], outputs: [{ type: 'address' }] }],
    functionName: 'addr', args: [R.namehash((b) => v.keccak256(b), norm)],
  });
  const out = await rpc(rpcUrl, 'eth_call', [{ to: R.UNIVERSAL_RESOLVER, data: R.encodeResolve(R.dnsEncode(norm), inner) }, 'latest']);
  const { result } = R.decodeResolve(out);
  if (!result || result.length < 66) throw new Error(`${norm} does not resolve to an address`);
  const address = v.getAddress('0x' + result.slice(-40));
  if (/^0x0{40}$/.test(address)) throw new Error(`${norm} does not resolve to an address`);
  return { address, name: norm };
}

let lensArtifacts = null;
/** The AssetLens calling convention, from this origin. It carries no account:
 *  the bytecode is the same for everyone who asks. */
export async function lens(base = '') {
  if (!lensArtifacts) {
    lensArtifacts = fetch(`${base}/v1/lens`).then(async r => {
      if (!r.ok) throw new Error(`this origin does not serve the lens (${r.status})`);
      return (await r.json()).lenses.asset;
    }).catch(e => { lensArtifacts = null; throw e; });
  }
  return lensArtifacts;
}

export async function lensCall(url, art, req) {
  const v = await viem();
  const encoded = v.encodeAbiParameters(art.request, [req]);
  const data = art.creation + encoded.slice(2);
  if (data.length / 2 > art.max_payload_bytes) throw new Error('payload too large');
  const out = await rpc(url, 'eth_call', [{ data }, 'latest']);
  if (!out || out === '0x') throw new Error('empty reply from the node');
  return v.decodeAbiParameters(art.reply, out)[0];
}

// onProgress, when given, is called after each batch with {done, total, calls}.
// A long list is many round trips and a caller may want to say so while they land.
export async function balances(url, account, tokens, base = '', onProgress = null) {
  const art = await lens(base);
  const rows = [];
  let info = null;
  let batch = 20;
  let i = 0;
  let calls = 0;
  while (i < tokens.length) {
    const chunk = tokens.slice(i, i + batch);
    try {
      const reply = await lensCall(url, art, {
        account, spenders: [],
        tokens: chunk.map(t => ({ token: t, ids: [] })),
        includeUri: false, includeCode: false,
        enumerateLimit: 0n, gasPerCall: 0n, maxStringBytes: 64n,
      });
      calls++;
      info ||= { chain: reply.chain, account: reply.account };
      for (const t of reply.tokens) rows.push(t);
      i += chunk.length;
      if (onProgress) onProgress({ done: i, total: tokens.length, calls, rows });
    } catch (e) {
      calls++;
      // A reply over EIP-170's ceiling fails outright rather than truncating; halve
      // the batch and retry, and only give up at one.
      if (batch > 1) { batch = Math.max(1, Math.floor(batch / 2)); continue; }
      throw e;
    }
  }
  if (!info) {
    const reply = await lensCall(url, art, { account, spenders: [], tokens: [], includeUri: false, includeCode: false, enumerateLimit: 0n, gasPerCall: 0n, maxStringBytes: 64n });
    calls++;
    info = { chain: reply.chain, account: reply.account };
  }
  return { rows, info, calls };
}

export function fmtUnits(raw, decimals) {
  const s = BigInt(raw).toString();
  if (decimals === undefined || decimals === null) return s;
  const d = Number(decimals);
  if (d === 0) return num(s);
  const pad = s.padStart(d + 1, '0');
  const whole = pad.slice(0, -d), frac = pad.slice(-d).replace(/0+$/, '');
  const w = BigInt(whole).toLocaleString('en-US');
  return frac ? `${w}.${frac.slice(0, 6)}` : w;
}
