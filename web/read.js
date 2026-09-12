// read.js — the reader's end of a commitment.
//
// index.html looks a wallet up; the index commits what it found to a merkle root; and
// this page is the separate flow that reads an account back from that commitment,
// through ENS, with the daemon out of the path. Every account has a name under the
// registry's resolver, <hex>.hints.<parent>, served by HintResolver. Asking it for
// evmscan.contracts reverts OffchainLookup at the registry's gateways; this page
// follows it — the reader's own choice, made here, with the gateway named on screen —
// and the callback verifies the answer against the latest finalized root on chain
// before it is returned. A gateway can withhold an answer and cannot forge one.
//
// The balances are then read at head with AssetLens, one deployless eth_call per
// batch, against an RPC the reader names. The only request to this origin is for the
// lens bytecode, which is the same for everyone.
//
// A wallet's own list used to be publishable to its owner's ENS name and readable
// here as a second mode. That cost about 31,000 gas per contract and stored per
// wallet what a vote counter stores once for everyone, so the list went and the vote
// (index.html, "ask for these to be indexed") took its place. What a vote buys is a
// place in the index, and the index is what this page reads.

import * as R from './ensrec.js';

const $ = (id) => document.getElementById(id);
const VIEM_MODULE = 'https://esm.sh/viem@2.56.3?bundle';
let viemModule = null;
const viem = () => (viemModule ||= import(VIEM_MODULE));

// The same storage keys index.html uses, so a reader who named an RPC there has it
// here too.
const NAME_RPC_KEY = 'evmscan.name-rpc';
const DEFAULT_NAME_RPC = 'https://ethereum-rpc.publicnode.com';
const chainRpcKey = (id) => `evmscan.chain-rpc.${id}`;
const PUBLIC_RPC = {
  1: 'https://ethereum-rpc.publicnode.com',
  8453: 'https://base-rpc.publicnode.com',
  11155111: 'https://ethereum-sepolia-rpc.publicnode.com',
};
const CHAIN_NAME = { 1: 'Ethereum mainnet', 8453: 'Base', 11155111: 'Sepolia' };

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const short = (a, n = 4) => a ? `${a.slice(0, 2 + n)}…${a.slice(-n)}` : '';
const host = (u) => { try { return new URL(u).host; } catch { return u; } };
const num = (n) => Number(n).toLocaleString('en-US');
const ADDRESS_RE = /^0x[0-9a-fA-F]{40}$/;

function store(k, v) { try { v ? localStorage.setItem(k, v) : localStorage.removeItem(k); } catch { /* private window */ } }
function load(k) { try { return localStorage.getItem(k) || ''; } catch { return ''; } }

async function rpc(url, method, params) {
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

// ---------------------------------------------------------------------------
// The trail: what happened, in order, naming every host that was touched.
// ---------------------------------------------------------------------------
const trail = [];
const left = [];
function step(t, d, tone) {
  trail.push({ t, d, tone });
  const el = $('trail');
  el.hidden = false;
  el.innerHTML = trail.map((s, i) => `
    <div class="step ${s.tone || ''}"><span class="n">${i + 1}</span>
      <div><div class="t">${s.t}</div>${s.d ? `<div class="d">${s.d}</div>` : ''}</div></div>`).join('');
}
function disclose(what) {
  left.push(what);
  $('disclose').hidden = false;
  $('disclose-list').innerHTML = left.map(x => `<li>${x}</li>`).join('');
}
function reset() {
  trail.length = 0; left.length = 0;
  $('trail').hidden = true; $('trail').innerHTML = '';
  $('disclose').hidden = true; $('result').hidden = true;
  $('err').hidden = true;
}

// ---------------------------------------------------------------------------
// ENS
// ---------------------------------------------------------------------------
const ensRpc = () => $('ens-rpc').value.trim() || DEFAULT_NAME_RPC;

async function deps() {
  const v = await viem();
  const url = ensRpc();
  return { call: (to, data) => rpc(url, 'eth_call', [{ to, data }, 'latest']), keccak: (b) => v.keccak256(b) };
}

// A typed name becomes an address through the Universal Resolver, the same way the
// lookup page does it. Only the registry mode needs this, to build the hint name.
async function resolveAddress(name) {
  const v = await viem();
  const norm = R.normalizeName(name);
  const inner = v.encodeFunctionData({
    abi: [{ type: 'function', name: 'addr', stateMutability: 'view', inputs: [{ name: 'node', type: 'bytes32' }], outputs: [{ type: 'address' }] }],
    functionName: 'addr', args: [R.namehash((b) => v.keccak256(b), norm)],
  });
  const out = await rpc(ensRpc(), 'eth_call', [{ to: R.UNIVERSAL_RESOLVER, data: R.encodeResolve(R.dnsEncode(norm), inner) }, 'latest']);
  const { result } = R.decodeResolve(out);
  if (!result || result.length < 66) throw new Error(`${norm} does not resolve to an address`);
  const address = v.getAddress('0x' + result.slice(-40));
  if (/^0x0{40}$/.test(address)) throw new Error(`${norm} does not resolve to an address`);
  return { address, name: norm };
}

// ERC-3668, by hand, so the gateway that was called can be named on screen. viem
// would follow it too, silently; the whole point of this mode is that the reader
// sees the hop.
const OFFCHAIN_ABI = [{
  type: 'error', name: 'OffchainLookup',
  inputs: [
    { name: 'sender', type: 'address' }, { name: 'urls', type: 'string[]' }, { name: 'callData', type: 'bytes' },
    { name: 'callbackFunction', type: 'bytes4' }, { name: 'extraData', type: 'bytes' },
  ],
}];

async function followOffchain(revertData, url) {
  const v = await viem();
  let data = revertData;
  for (let hop = 0; hop < 4; hop++) {
    const { args } = v.decodeErrorResult({ abi: OFFCHAIN_ABI, data });
    const [sender, urls, callData, callbackFunction, extraData] = args;
    let response = null, used = '', lastErr = null;
    for (const tmpl of urls) {
      used = tmpl.replace('{sender}', sender.toLowerCase()).replace('{data}', callData);
      try {
        const res = tmpl.includes('{data}')
          ? await fetch(used)
          : await fetch(used, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ sender, data: callData }) });
        if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
        response = (await res.json()).data;
        if (typeof response !== 'string' || !response.startsWith('0x')) throw new Error('gateway answered without a data field');
        break;
      } catch (e) { lastErr = e; response = null; }
    }
    if (response === null) throw new Error(`no gateway answered (${lastErr ? lastErr.message : 'none listed'})`);
    step(`Followed the gateway, in this browser`, `${esc(host(used))} · sender ${esc(short(sender))} · the gateway saw the account in the calldata`, 'amber');
    disclose(`the gateway at <code>${esc(host(used))}</code> saw the account being asked about, unpacked from calldata`);

    const callback = callbackFunction + v.encodeAbiParameters(
      [{ type: 'bytes' }, { type: 'bytes' }], [response, extraData]).slice(2);
    try {
      const out = await rpc(url, 'eth_call', [{ to: sender, data: callback }, 'latest']);
      return { out, sender, gateway: used };
    } catch (e) {
      const d = String(e.data || '');
      if (d.startsWith(R.SEL_OFFCHAIN_LOOKUP)) { data = d; continue; }
      throw new Error(`the callback refused the gateway's answer: ${e.message}`);
    }
  }
  throw new Error('too many offchain hops');
}

// ---------------------------------------------------------------------------
// The two reads
// ---------------------------------------------------------------------------

async function readRegistry(typed, parent) {
  if (!parent) throw new Error('the registry mode needs the parent name the resolver hangs under');
  let address, label = '';
  if (ADDRESS_RE.test(typed)) address = typed;
  else {
    const r = await resolveAddress(typed);
    address = r.address; label = r.name;
    step(`Resolved ${esc(r.name)} to ${esc(short(address))}`, `Universal Resolver on ${esc(host(ensRpc()))}`);
  }
  const hint = R.hintName(address, parent);
  const d = await deps();
  const url = ensRpc();

  // The contracts record is the one that reverts OffchainLookup; the rest are plain.
  let value, resolver;
  try {
    const r = await R.readText(d, hint, R.KEY_CONTRACTS);
    value = r.value; resolver = r.resolver;
    step(`Read <code>${esc(R.KEY_CONTRACTS)}</code> on ${esc(hint)}`, `answered on chain without a gateway · resolver ${esc(short(resolver))}`);
  } catch (e) {
    if (e && e.notFound) throw new Error(`${hint} has no resolver — is ${R.normalizeName(parent)} the parent a HintResolver was attached to?`);
    if (!(e && e.offchain)) throw e;
    step(`Asked for <code>${esc(R.KEY_CONTRACTS)}</code> on ${esc(hint)}`, `the resolver reverted OffchainLookup, as HintResolver does for this record`);
    const { out } = await followOffchain(e.data, url);
    const dec = R.decodeResolve(out);
    value = R.decodeString(dec.result); resolver = dec.resolver;
    step(`Verified on chain`, `the callback handed the gateway's answer to HintRegistry.contractsOfCallback, which recomputed the leaf and checked the proof against the latest finalized root before returning it`, 'green');
  }
  disclose(`the ENS RPC at <code>${esc(host(ensRpc()))}</code> saw the hint name, which carries the address`);

  const contracts = R.parseContracts(value);
  const side = {};
  for (const k of [R.KEY_CHAIN, R.KEY_EPOCH, R.KEY_RANGE, R.KEY_ROOT]) {
    side[k] = await R.readText(d, hint, k).then(r => r.value).catch(() => '');
  }
  const chainId = R.parseChain(side[R.KEY_CHAIN]) ?? 1;
  step(`Read the commitment's provenance`,
    `epoch ${esc(side[R.KEY_EPOCH] || '?')} · blocks ${esc(side[R.KEY_RANGE] || '?')} · root ${esc(short(side[R.KEY_ROOT] || '', 6))} · chain ${esc(String(chainId))}`);
  return { kind: 'registry', name: label || hint, hint, address, chainId, contracts, raw: value, side };
}

// ---------------------------------------------------------------------------
// Balances at head
// ---------------------------------------------------------------------------
let lensArtifacts = null;
async function lens() {
  if (!lensArtifacts) {
    lensArtifacts = fetch('/v1/lens').then(async r => {
      if (!r.ok) throw new Error(`this origin does not serve the lens (${r.status})`);
      return (await r.json()).lenses.asset;
    }).catch(e => { lensArtifacts = null; throw e; });
  }
  return lensArtifacts;
}

async function lensCall(url, art, req) {
  const v = await viem();
  const encoded = v.encodeAbiParameters(art.request, [req]);
  const data = art.creation + encoded.slice(2);
  if (data.length / 2 > art.max_payload_bytes) throw new Error('payload too large');
  const out = await rpc(url, 'eth_call', [{ data }, 'latest']);
  if (!out || out === '0x') throw new Error('empty reply from the node');
  return v.decodeAbiParameters(art.reply, out)[0];
}

async function balances(url, account, tokens) {
  const art = await lens();
  const rows = [];
  let info = null;
  let batch = 20;
  let i = 0;
  while (i < tokens.length) {
    const chunk = tokens.slice(i, i + batch);
    try {
      const reply = await lensCall(url, art, {
        account, spenders: [],
        tokens: chunk.map(t => ({ token: t, ids: [] })),
        includeUri: false, includeCode: false,
        enumerateLimit: 0n, gasPerCall: 0n, maxStringBytes: 64n,
      });
      info ||= { chain: reply.chain, account: reply.account };
      for (const t of reply.tokens) rows.push(t);
      i += chunk.length;
    } catch (e) {
      // A reply over EIP-170's ceiling fails outright rather than truncating; halve
      // the batch and retry, and only give up at one.
      if (batch > 1) { batch = Math.max(1, Math.floor(batch / 2)); continue; }
      throw e;
    }
  }
  if (!info) {
    const reply = await lensCall(url, art, { account, spenders: [], tokens: [], includeUri: false, includeCode: false, enumerateLimit: 0n, gasPerCall: 0n, maxStringBytes: 64n });
    info = { chain: reply.chain, account: reply.account };
  }
  return { rows, info };
}

const STD = { 20: 'ERC-20', 21: 'ERC-721', 55: 'ERC-1155', 0: '?' };

function fmtUnits(raw, decimals) {
  const s = BigInt(raw).toString();
  if (decimals === undefined || decimals === null) return s;
  const d = Number(decimals);
  if (d === 0) return num(s);
  const pad = s.padStart(d + 1, '0');
  const whole = pad.slice(0, -d), frac = pad.slice(-d).replace(/0+$/, '');
  const w = BigInt(whole).toLocaleString('en-US');
  return frac ? `${w}.${frac.slice(0, 6)}` : w;
}

function render(r, bal, rpcUrl) {
  $('result').hidden = false;
  $('r-name').textContent = r.name;
  $('r-addr').textContent = r.address || '';
  const verdict = $('r-verdict');
  verdict.className = 'verdict verified';
  verdict.textContent = `verified · epoch ${r.side[R.KEY_EPOCH] || '?'}`;

  const held = bal.rows.filter(t => t.hasBalance && t.balance > 0n).length;
  const unread = bal.rows.filter(t => !t.hasBalance).length;
  const native = bal.info.account.balance;
  $('r-totals').innerHTML =
    `${num(r.contracts.length)} contract${r.contracts.length === 1 ? '' : 's'} committed · ${num(held)} hold${held === 1 ? 's' : ''} a balance now` +
    (unread ? ` · ${num(unread)} could not be read` : '') +
    ` · ${esc(fmtUnits(native, 18))} native at block ${num(bal.info.chain.blockNumber)} on ${esc(host(rpcUrl))}`;

  $('rows').innerHTML = bal.rows.map(t => {
    const zero = !t.hasBalance || t.balance === 0n;
    const bal_ = !t.hasBalance ? '<span class="empty">could not read</span>'
      : t.standard === 55 ? '<span class="empty">per id · not read</span>'
      : esc(fmtUnits(t.balance, t.hasDecimals ? Number(t.decimals) : (t.standard === 21 ? 0 : undefined)));
    return `<div class="tablerow ${zero ? 'dim' : ''}">
      <span class="sym">${esc(t.symbol || short(t.token))}${!t.isContract ? '<span class="tag flag">no code</span>' : ''}</span>
      <span class="nmcell" title="${esc(t.name)}">${esc(t.name || '')}</span>
      <span class="bal">${bal_}${t.hasBalance && t.balance === 0n ? '<span class="tag">none now</span>' : ''}</span>
      <span class="std">${esc(STD[t.standard] || String(t.standard))}</span>
      <span class="ad"><a href="index.html#${esc(t.token)}" title="${esc(t.token)}">${esc(short(t.token, 6))}</a></span>
    </div>`;
  }).join('');

  $('r-foot').innerHTML = `The list is what the index committed for this account in epoch ${esc(r.side[R.KEY_EPOCH] || '?')},
    verified against root <code>${esc(short(r.side[R.KEY_ROOT] || '', 6))}</code> by the registry's own callback before this page
    saw it. It covers the contracts the index keeps, not the chain: a contract nobody asked for is absent whether or not this
    account holds it — the lookup page is where to ask.`;
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------
function chainRpcFor(id) {
  return load(chainRpcKey(id)) || PUBLIC_RPC[id] || '';
}

async function run() {
  reset();
  const typed = $('name').value.trim();
  if (!typed) return;
  const go = $('go');
  go.disabled = true;
  try {
    store(NAME_RPC_KEY, $('ens-rpc').value.trim() === DEFAULT_NAME_RPC ? '' : $('ens-rpc').value.trim());
    const r = await readRegistry(typed, $('parent').value.trim());

    if (!r.address) throw new Error(`${r.name} does not resolve to an address, so there is no account to read balances for`);

    // The balances RPC: what the reader typed for this chain, else a public one.
    let url = $('chain-rpc').value.trim();
    if (url && url !== chainRpcFor(r.chainId)) store(chainRpcKey(r.chainId), url);
    url = url || chainRpcFor(r.chainId);
    if (!url) throw new Error(`no RPC for chain ${r.chainId}; type one under "balances, read at head from"`);
    $('chain-rpc').value = url;
    $('chain-rpc-note').textContent = `chain ${r.chainId}${CHAIN_NAME[r.chainId] ? ` · ${CHAIN_NAME[r.chainId]}` : ''} · this one sees the address`;

    if (!r.contracts.length) {
      step('The record is empty', 'nothing to read balances for');
      render(r, { rows: [], info: { chain: { blockNumber: 0n }, account: { balance: 0n } } }, url);
      return;
    }

    step(`Fetched the lens bytecode`, `GET /v1/lens on this origin · the same bytes for everyone · no account in the request`);
    const bal = await balances(url, r.address, r.contracts);
    step(`Read ${num(r.contracts.length)} balance${r.contracts.length === 1 ? '' : 's'} at head`,
      `deployless eth_call to ${esc(host(url))} · block ${num(bal.info.chain.blockNumber)} · one consistent snapshot per batch`, 'green');
    disclose(`the balances RPC at <code>${esc(host(url))}</code> saw the account and every contract asked about`);
    disclose(`this origin was asked for the lens bytecode only`);
    render(r, bal, url);
  } catch (e) {
    const el = $('err');
    el.hidden = false;
    el.textContent = e.message || String(e);
  } finally {
    go.disabled = false;
  }
}

async function init() {
  $('ens-rpc').value = load(NAME_RPC_KEY) || DEFAULT_NAME_RPC;
  $('chain-rpc').value = load(chainRpcKey(1)) || load('evmscan.chain-rpc') || PUBLIC_RPC[1];
  $('go').addEventListener('click', run);
  $('name').addEventListener('keydown', e => { if (e.key === 'Enter') run(); });

  // The parent comes from the deployment when it has a resolver attached; a typed one
  // wins, so a reader can point this page at somebody else's.
  fetch('/v1/status').then(r => r.ok ? r.json() : null).then(s => {
    const parent = s && s.registry && s.registry.ens_parent;
    if (parent) { $('parent').value = parent; $('parent-note').textContent = 'from this deployment'; }
    else $('parent-note').textContent = 'this deployment has no resolver attached · type the parent of one that does';
  }).catch(() => { $('parent-note').textContent = 'could not ask this deployment · type the parent'; });

  const q = new URLSearchParams(location.search);
  if (q.get('parent')) $('parent').value = q.get('parent');
  if (q.get('name')) { $('name').value = q.get('name'); run(); }
}

init();

// For the console and for tests: the pieces, so a list can be read without a name
// that carries one. Nothing on the page calls these through here.
window.evmscanRead = { readRegistry, followOffchain, balances, render, deps, R };
