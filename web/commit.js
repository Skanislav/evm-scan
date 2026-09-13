// commit.js — the commit half of "commit, then discover".
//
// The page is deliberately standalone: sweep the chains the reader picks, review the
// exact pairs, sign the list, POST it. The Overview page (index.html) is the other
// half — it reads the stored list back and applies it as the filter. Neither page
// references the other's DOM; the hand-off is the daemon's asset-commit endpoint.
//
// What the signature means, and does not: EIP-712 AssetCommit(account, digest,
// deadline) under the "evm-scan assets" domain. The digest is keccak256 over the
// sorted (uint64be chain_id || address) pairs — assetCommitDigest here must match
// internal/api/assetsig.go byte for byte. Signing discloses the exact list publicly
// and saves future balance calls; it never registers, promotes, or pays to index
// anything, and it moves no funds.

const $ = (id) => document.getElementById(id);
const ADDRESS_RE = /^0x[0-9a-fA-F]{40}$/;
const ADDRESS_CI_RE = /^0x[0-9a-fA-F]{40}$/i;

// Everything read off a chain is somebody else's text; names come from contracts
// anyone can deploy and from a fetched token list, so all of it goes through esc.
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, ch =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));
const short = (h, n = 6) => (h && h.length > 2 * n + 4) ? h.slice(0, n + 2) + '…' + h.slice(-n) : (h || '');
const num = (n) => (n === undefined || n === null) ? '—' : Number(n).toLocaleString('en-US');
const hostOf = (u) => { try { return new URL(u).host; } catch { return u; } };
const hexToBytes = (h) => {
  const s = h.startsWith('0x') ? h.slice(2) : h;
  const out = new Uint8Array(s.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(s.substr(i * 2, 2), 16);
  return out;
};

// formatUnits scales an integer string by decimals without floating point.
function formatUnits(raw, decimals) {
  if (raw === undefined || raw === null) return '—';
  const d = Number.isInteger(decimals) ? decimals : 0;
  const s0 = String(raw);
  const neg = s0.startsWith('-');
  const s = (neg ? s0.slice(1) : s0).padStart(d + 1, '0');
  let whole = d ? s.slice(0, -d) : s;
  const frac = d ? s.slice(-d).replace(/0+$/, '') : '';
  whole = whole.replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  return (neg ? '-' : '') + whole + (frac ? '.' + frac.slice(0, 6) : '');
}

// The daemon API, same origin. Writes here are reader-signed, so they never carry
// the operator token (server.go's guarded() exempts /asset-commit).
async function api(path, opts) {
  const res = await fetch(path, opts);
  const text = await res.text();
  let body;
  try { body = text ? JSON.parse(text) : {}; } catch { body = { error: text }; }
  if (!res.ok) {
    const err = new Error(body.detail || body.error || res.statusText);
    err.status = res.status;
    throw err;
  }
  return body;
}

// ---------------------------------------------------------------------------
// Account
// ---------------------------------------------------------------------------

let ACCOUNT = '';       // normalized lowercase-with-0x, checksummed form unused here
let EXISTING = null;    // the stored commit, if any: { assets: [{chain_id, address}], signed_at, deadline }

async function setAccount(raw) {
  $('account-err').hidden = true;
  const v = raw.trim();
  if (!ADDRESS_CI_RE.test(v)) {
    ACCOUNT = '';
    $('account-err').textContent = 'not an address — paste the 0x… form';
    $('account-err').hidden = false;
    $('sweep-go').disabled = true;
    return;
  }
  ACCOUNT = v.toLowerCase();
  $('done').hidden = true;
  updateSweepButton();
  try { await walletSwitchTo(1); } catch { /* no wallet connected; sweep RPCs carry the chain */ }
  EXISTING = await loadExisting(ACCOUNT);
}

async function loadExisting(account) {
  const el = $('commit-existing');
  try {
    const c = await api(`/v1/accounts/${account}/asset-commit`);
    el.hidden = false;
    el.innerHTML = `This account already has a signed list: <strong>${num(c.assets.length)}</strong> pairs, signed
      ${esc(c.signed_at)}. Signing below <strong>replaces</strong> it atomically — pairs this sweep did not reach are
      carried into the new list unless you uncheck them.`;
    return c;
  } catch (e) {
    if (e.status === 404) {
      el.hidden = true;
      return null;
    }
    el.hidden = false;
    el.textContent = `could not read the existing list: ${e.message || e}`;
    return null;
  }
}

// A wallet is only needed for the signature, but connecting here pre-fills the
// account and guarantees the signer will be the account on screen.
async function connectWallet() {
  if (!window.ethereum) throw new Error('no injected wallet found — this needs MetaMask or another EIP-1193 wallet');
  const [acct] = await window.ethereum.request({ method: 'eth_requestAccounts' });
  if (!acct) throw new Error('the wallet offered no account');
  $('account').value = acct;
  await setAccount(acct);
}

// Ask the wallet onto mainnet so the chainId in its UI matches what it is signing
// for. eth_signTypedData_v4 is chain-agnostic here (the domain carries no chainId),
// but a wallet on some L2 signing for mainnet holdings confuses readers.
async function walletSwitchTo(chainId) {
  if (!window.ethereum) return;
  const hex = '0x' + chainId.toString(16);
  const current = await window.ethereum.request({ method: 'eth_chainId' });
  if (String(current).toLowerCase() !== hex) {
    await window.ethereum.request({ method: 'wallet_switchEthereumChain', params: [{ chainId: hex }] });
  }
}

// ---------------------------------------------------------------------------
// The sweep — the same lens read the index page runs, without its index UI.
//
// Curation is the lever: the Uniswap default list (~1.7k contracts over ~25 chains)
// sweeps in seconds; every contract gets read, nothing is narrowed by the index
// filter (a filter miss means "not indexed", never "no balance").
// ---------------------------------------------------------------------------

const VIEM_MODULE = 'https://esm.sh/viem@2.56.3?bundle';
const VIEM_CHAINS_MODULE = 'https://esm.sh/viem@2.56.3/es2022/chains.mjs';
const DEFAULT_TOKEN_LIST = 'https://tokens.uniswap.org';
const LENS_BATCH = 24;          // EIP-170 reply ceiling: ~920 B/token → 24 per call
const CHAIN_CONCURRENCY = 4;
const CHAIN_TIMEOUT_MS = 20000;

// Testnets that were shut down and still appear in token lists: their public
// endpoints refuse eth_call. Dropped by chain id.
const RETIRED_CHAINS = new Set([5, 80001, 420, 421613, 84531]);

// Per-chain RPC overrides, shared with index.html's keys so a reader who pointed
// the Overview at their own node gets the same here.
const chainRpcKeyFor = (id) => `evmscan.chain-rpc.${id}`;
function rpcForChain(id, viemChain) {
  try {
    const own = localStorage.getItem(chainRpcKeyFor(id));
    if (own) return { url: own, mine: true };
  } catch { /* private window */ }
  const pub = viemChain?.rpcUrls?.default?.http?.[0] || '';
  return { url: pub, mine: false };
}

let viemModule = null;
const viem = () => (viemModule ||= import(VIEM_MODULE));
let viemChains = null;
async function allChains() {
  if (!viemChains) {
    viemChains = import(VIEM_CHAINS_MODULE).then(m =>
      Object.values(m).filter(c => c && typeof c === 'object' && c.id && c.rpcUrls)
    ).catch(e => { viemChains = null; throw e; });
  }
  return viemChains;
}

let tokenListCache = null;
async function tokenList() {
  if (!tokenListCache) {
    tokenListCache = (async () => {
      const res = await fetch(DEFAULT_TOKEN_LIST);
      if (!res.ok) throw new Error(`${DEFAULT_TOKEN_LIST}: ${res.status}`);
      const doc = await res.json();
      const byChain = new Map();
      for (const t of (doc.tokens || [])) {
        if (!ADDRESS_RE.test(t.address || '')) continue;
        if (!byChain.has(t.chainId)) byChain.set(t.chainId, []);
        byChain.get(t.chainId).push(t);
      }
      return { name: doc.name, version: doc.version, byChain };
    })().catch(e => { tokenListCache = null; throw e; });
  }
  return tokenListCache;
}

let lensArtifacts = null;
async function lenses() {
  if (!lensArtifacts) {
    lensArtifacts = api('/v1/lens').then(r => r.lenses).catch(e => { lensArtifacts = null; throw e; });
  }
  return lensArtifacts;
}

async function rpc(url, method, params) {
  const res = await fetch(url, {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method, params }),
  });
  if (!res.ok) throw new Error(`${method}: ${res.status} ${res.statusText}`);
  const body = await res.json();
  if (body.error) throw Object.assign(new Error(`${method}: ${body.error.message || JSON.stringify(body.error)}`), { data: body.error.data });
  return body.result;
}

// One deployless AssetLens call per batch: bytecode + constructor args go to
// eth_call, nothing is deployed, nothing is stored.
async function lensCall(req, endpoint) {
  const art = (await lenses()).asset;
  if (!art) throw new Error('this daemon does not serve the asset lens');
  const v = await viem();
  const encoded = v.encodeAbiParameters(JSON.parse(JSON.stringify(art.request)), [req]);
  const data = art.creation + encoded.slice(2);
  if (data.length / 2 > art.max_payload_bytes) throw new Error('payload too large; use a smaller batch');
  const out = await rpc(endpoint, 'eth_call', [{ data }, 'latest']);
  if (!out || out === '0x') throw new Error('empty reply (is this an archive-free node at head?)');
  return v.decodeAbiParameters(JSON.parse(JSON.stringify(art.reply)), out)[0];
}

const withTimeout = (p, ms, what) => Promise.race([
  p,
  new Promise((_, rej) => setTimeout(() => rej(new Error(`no answer from ${what} in ${ms / 1000}s`)), ms)),
]);

// sweepableChains: chains the list has tokens for, that viem knows, and that have
// somewhere to ask. Mainnets first, busiest first.
let CHAINS = null;
const SELECTED = new Set();
let LIST = null;
let SWEEP_STATE = 'idle'; // idle | running | done

async function loadChains() {
  const [chains, list] = await Promise.all([allChains(), tokenList()]);
  LIST = list;
  const byId = new Map(chains.map(c => [c.id, c]));
  const out = [];
  for (const [chainId, tokens] of list.byChain) {
    const c = byId.get(chainId);
    if (!c || RETIRED_CHAINS.has(chainId)) continue;
    const { url, mine } = rpcForChain(chainId, c);
    if (!url) continue;
    out.push({ id: chainId, name: c.name, testnet: !!c.testnet, tokens, rpc: url, mine, host: hostOf(url) });
  }
  out.sort((a, b) => (a.testnet - b.testnet) || (b.tokens.length - a.tokens.length));
  return out;
}

function renderChainList() {
  $('chain-list').innerHTML = CHAINS.map(c => {
    const on = SELECTED.has(c.id);
    return `<div class="chaintile ${c.testnet ? 'dim' : ''}" data-chain="${c.id}" role="checkbox" aria-checked="${on}">
      <span class="box ${on ? 'on' : ''}">${on ? '✓' : ''}</span>
      <span style="min-width:0">
        <span class="cn">${esc(c.name)}${c.testnet ? ' <span class="tag">testnet</span>' : ''}</span>
        <span class="ch" title="${esc(c.rpc)}">${c.mine ? 'your endpoint · ' : ''}${esc(c.host)} · ${num(c.tokens.length)} contracts</span>
      </span>
    </div>`;
  }).join('');
  for (const el of $('chain-list').querySelectorAll('[data-chain]')) {
    el.addEventListener('click', () => {
      const id = Number(el.dataset.chain);
      SELECTED.has(id) ? SELECTED.delete(id) : SELECTED.add(id);
      renderChainList();
    });
  }
  updateDisclosure();
  updateSweepButton();
}

function updateDisclosure() {
  const picked = CHAINS.filter(c => SELECTED.has(c.id));
  const wrap = $('sweep-before');
  if (!ACCOUNT || !picked.length) { wrap.hidden = true; return; }
  const strangers = picked.filter(c => !c.mine);
  const contracts = picked.reduce((n, c) => n + c.tokens.length, 0);
  const calls = picked.reduce((n, c) => n + Math.ceil(c.tokens.length / LENS_BATCH), 0);
  wrap.hidden = false;
  $('sweep-disclosure').innerHTML = `Every endpoint reached sees the address — <strong>${
    !strangers.length ? 'all selected endpoints are yours'
    : `${num(strangers.length)} of the ${num(picked.length)} selected ${strangers.length === 1 ? 'is' : 'are'} not yours: ${strangers.map(c => esc(c.host)).join(', ')}`
  }</strong>. <code>${esc(LIST.name)}</code> lists ${num(contracts)} contracts on them, about ${num(calls)} lens calls.
  None of it reaches this daemon until you sign below.`;
}

function updateSweepButton() {
  const go = $('sweep-go');
  const picked = CHAINS ? CHAINS.filter(c => SELECTED.has(c.id)).length : 0;
  go.disabled = !ACCOUNT || !picked || SWEEP_STATE === 'running';
  go.textContent = SWEEP_STATE === 'running' ? 'Sweeping…'
    : SWEEP_STATE === 'done' ? `Sweep again`
    : `Sweep ${num(picked)} network${picked === 1 ? '' : 's'}`;
}

async function sweepChain(chain, account) {
  const tokens = chain.tokens;
  if (!tokens.length) return { chain, held: [], calls: 0, failedCalls: 0 };
  const held = [];
  let calls = 0, failedCalls = 0, ok = 0;
  for (let i = 0; i < tokens.length; i += LENS_BATCH) {
    const slice = tokens.slice(i, i + LENS_BATCH);
    const read = (ts) => {
      calls++;
      return withTimeout(lensCall({
        account, spenders: [],
        tokens: ts.map(t => ({ token: t.address, ids: [] })),
        includeUri: false, includeCode: false,
        enumerateLimit: 0n, gasPerCall: 0n, maxStringBytes: 32n,
      }, chain.rpc), CHAIN_TIMEOUT_MS, `${chain.name} (${chain.host})`);
    };
    const collect = (ts, reply) => {
      const meta = new Map(ts.map(t => [t.address.toLowerCase(), t]));
      for (const t of (reply.tokens || [])) {
        if (!t.hasBalance || t.balance === 0n) continue;
        const m = meta.get((t.token || '').toLowerCase()) || {};
        held.push({
          chainId: chain.id, chainName: chain.name, address: t.token,
          symbol: m.symbol || t.symbol, name: m.name || t.name,
          decimals: m.decimals ?? (t.hasDecimals ? Number(t.decimals) : undefined),
          balance: String(t.balance),
        });
      }
    };
    try {
      collect(slice, await read(slice));
      ok++;
    } catch (batchError) {
      // A batched revert has no useful partial reply; retry per contract so only
      // the one that rejects the lens is omitted.
      for (const token of slice) {
        try { collect([token], await read([token])); ok++; }
        catch { failedCalls++; }
      }
    }
    $('sweep-status').textContent = `${chain.name}: ${num(Math.min(i + LENS_BATCH, tokens.length))} of ${num(tokens.length)}…`;
  }
  return {
    chain, held, calls, failedCalls,
    error: ok ? '' : 'every call failed',
  };
}

async function runSweep() {
  if (!ACCOUNT || SWEEP_STATE === 'running') return;
  const picked = CHAINS.filter(c => SELECTED.has(c.id));
  if (!picked.length) return;
  $('sweep-err').hidden = true;
  $('done').hidden = true;
  SWEEP_STATE = 'running';
  updateSweepButton();
  const done = [];
  try {
    const queue = [...picked];
    const workers = Array.from({ length: Math.min(CHAIN_CONCURRENCY, queue.length) }, async () => {
      for (;;) {
        const chain = queue.shift();
        if (!chain) return;
        try {
          const r = await sweepChain(chain, ACCOUNT);
          done.push(r);
        } catch (e) {
          done.push({ chain, held: [], calls: 0, failedCalls: 0, error: e.message || String(e) });
        }
      }
    });
    await Promise.all(workers);
  } finally {
    SWEEP_STATE = 'done';
    updateSweepButton();
    const failed = done.filter(r => r.error);
    const heldTotal = done.reduce((n, r) => n + r.held.length, 0);
    $('sweep-status').textContent =
      `${num(done.length - failed.length)} of ${num(done.length)} networks answered · ${num(heldTotal)} holding${heldTotal === 1 ? '' : 's'}`
      + (failed.length ? ` · ${num(failed.length)} failed: ${failed.map(r => r.chain.name).join(', ')}` : '');
    renderReview(done.flatMap(r => r.held), failed.length ? failed.map(r => `${r.chain.name}: ${r.error}`) : []);
  }
}

// ---------------------------------------------------------------------------
// Review, sign, commit
// ---------------------------------------------------------------------------

// The reviewed set: swept holdings plus carried pairs from the existing commit.
// Keyed chain:address so a swept holding and a carried pair merge into one row —
// the carried one is the same disclosure, just from an earlier signature.
const REVIEW = new Map(); // key -> { chainId, chainName, address, symbol, name, decimals, balance, on, swept }

function renderReview(held, sweepErrors) {
  REVIEW.clear();
  for (const h of held) {
    const key = `${h.chainId}:${h.address.toLowerCase()}`;
    REVIEW.set(key, { ...h, on: true, swept: true });
  }
  // Carry the existing commit's pairs so an unchanged chain's entries survive a
  // signature over a partial sweep. These start unchecked if the sweep did see
  // their chain and they were not held — a stale entry the sweep just disproved.
  const sweptChains = new Set(held.map(h => h.chainId));
  let carried = 0;
  for (const item of (EXISTING && EXISTING.assets) || []) {
    const key = `${item.chain_id}:${String(item.address).toLowerCase()}`;
    if (REVIEW.has(key)) continue;
    // A chain this sweep answered for is authoritative for its own pairs: an
    // absent one is gone, not merely unread. Only chains the sweep skipped carry.
    if (SELECTED.has(Number(item.chain_id))) continue;
    REVIEW.set(key, {
      chainId: Number(item.chain_id), chainName: `chain ${item.chain_id}`,
      address: item.address, symbol: short(item.address, 4), name: '', decimals: undefined,
      balance: undefined, on: true, swept: false,
    });
    carried++;
  }
  if (sweepErrors.length) {
    $('sweep-err').hidden = false;
    $('sweep-err').textContent = `Some networks did not answer and were left out of the review: ${sweepErrors.join(' · ')}`;
  }
  $('review-carried').hidden = carried === 0;
  if (carried) $('review-carried').textContent =
    `${num(carried)} entr${carried === 1 ? 'y is' : 'ies are'} carried from your previous signature — chains this sweep did not read.`;
  $('review-card').hidden = REVIEW.size === 0;
  if (!REVIEW.size) return;
  renderReviewRows();
}

function renderReviewRows() {
  const rows = [...REVIEW.values()].sort((a, b) => (a.chainId - b.chainId) || a.address.localeCompare(b.address));
  $('review-rows').innerHTML = rows.map(r => {
    const key = `${r.chainId}:${r.address.toLowerCase()}`;
    return `<div class="review-row ${r.on ? '' : 'off'}" data-key="${esc(key)}">
      <span class="box ${r.on ? 'on' : ''}">${r.on ? '✓' : ''}</span>
      <span style="min-width:0">
        <span class="sym">${esc(r.symbol || short(r.address, 4))}</span>
        <span class="nm" style="margin-left:8px">${esc(r.name || (r.swept ? '' : 'from your previous signature'))}</span>
        <div class="addr">${esc(r.address)} · chain ${num(r.chainId)}</div>
      </span>
      <span class="num">${r.swept ? formatUnits(r.balance, r.decimals) : '—'}</span>
      <span class="hint" style="text-align:right">${esc(r.chainName)}</span>
    </div>`;
  }).join('');
  for (const el of $('review-rows').querySelectorAll('[data-key]')) {
    el.addEventListener('click', () => {
      const r = REVIEW.get(el.dataset.key);
      r.on = !r.on;
      renderReviewRows();
    });
  }
  const on = [...REVIEW.values()].filter(r => r.on);
  $('review-count').textContent = `${num(on.length)} of ${num(REVIEW.size)} pairs will be signed`;
  $('sign').disabled = on.length === 0 || on.length > 200;
  if (on.length > 200) $('review-count').textContent += ` — over the 200-pair limit; uncheck ${num(on.length - 200)}`;
}

// The digest: keccak256 over pairs sorted by chain then address, each as
// uint64be(chain_id) || address — internal/api/assetsig.go byte for byte.
function commitPairs(on) {
  const byPair = new Map();
  for (const r of on) {
    byPair.set(`${r.chainId}:${r.address.toLowerCase()}`, { chain_id: r.chainId, address: r.address.toLowerCase() });
  }
  return [...byPair.values()]
    .sort((a, b) => (a.chain_id - b.chain_id) || (a.address < b.address ? -1 : a.address > b.address ? 1 : 0));
}

async function signAndCommit() {
  const on = [...REVIEW.values()].filter(r => r.on);
  const assets = commitPairs(on);
  if (!assets.length) return;
  const btn = $('sign');
  const status = $('sign-status');
  const err = $('sign-err');
  btn.disabled = true;
  err.hidden = true;
  try {
    if (!window.ethereum) throw new Error('no injected wallet found');
    const [wallet] = await window.ethereum.request({ method: 'eth_requestAccounts' });
    if (!wallet) throw new Error('the wallet offered no account');
    if (wallet.toLowerCase() !== ACCOUNT) {
      throw new Error(`the connected wallet is ${short(wallet)}, but the account being committed is ${short(ACCOUNT)} — only the account can sign for itself`);
    }
    const v = await viem();
    const bytes = new Uint8Array(28 * assets.length);
    const view = new DataView(bytes.buffer);
    assets.forEach((item, i) => {
      view.setBigUint64(28 * i, BigInt(item.chain_id));
      bytes.set(hexToBytes(item.address), 28 * i + 8);
    });
    const digest = v.keccak256(bytes);
    const deadline = String(Math.floor(Date.now() / 1000) + 3600);
    status.textContent = 'sign the asset list in your wallet — no gas, nothing is sent from it…';
    const typed = {
      types: {
        EIP712Domain: [{ name: 'name', type: 'string' }, { name: 'version', type: 'string' }],
        AssetCommit: [
          { name: 'account', type: 'address' }, { name: 'digest', type: 'bytes32' },
          { name: 'deadline', type: 'uint256' },
        ],
      },
      primaryType: 'AssetCommit',
      domain: { name: 'evm-scan assets', version: '1' },
      message: { account: wallet, digest, deadline },
    };
    const signature = await window.ethereum.request({
      method: 'eth_signTypedData_v4',
      params: [wallet, JSON.stringify(typed)],
    });
    status.textContent = 'recording your signed list…';
    const stored = await api(`/v1/accounts/${wallet.toLowerCase()}/asset-commit`, {
      method: 'POST', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ assets, deadline, signature }),
    });
    status.textContent = '';
    EXISTING = stored;
    $('done').hidden = false;
    $('done-line').innerHTML = `Your wallet signed <strong>${num(stored.assets.length)}</strong> chain-and-contract
      pairs (digest <code>${esc(stored.digest.slice(0, 10))}…</code>), valid until
      <code>${new Date(Number(stored.deadline) * 1000).toISOString()}</code>. It is stored under your address and the
      Overview now applies it as the filter: those contracts are read first, before any discovery. Re-signing with a
      later deadline replaces it.`;
    $('done-overview').href = `index.html?account=${ACCOUNT}`;
  } catch (e) {
    err.hidden = false;
    err.textContent = e.message || String(e);
    btn.disabled = false;
    status.textContent = '';
  }
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

$('connect').addEventListener('click', async () => {
  try { await connectWallet(); } catch (e) { $('account-err').hidden = false; $('account-err').textContent = e.message || String(e); }
});
$('account').addEventListener('change', () => setAccount($('account').value));
$('account').addEventListener('keydown', (e) => { if (e.key === 'Enter') setAccount($('account').value); });
$('sweep-go').addEventListener('click', runSweep);
$('chain-all').addEventListener('click', () => { for (const c of CHAINS) if (!c.testnet) SELECTED.add(c.id); renderChainList(); });
$('chain-none').addEventListener('click', () => { SELECTED.clear(); renderChainList(); });
$('review-all').addEventListener('click', () => { for (const r of REVIEW.values()) r.on = true; renderReviewRows(); });
$('review-none').addEventListener('click', () => { for (const r of REVIEW.values()) r.on = false; renderReviewRows(); });
$('sign').addEventListener('click', signAndCommit);

(async () => {
  try {
    CHAINS = await loadChains();
  } catch (e) {
    $('chain-list').innerHTML = `<div class="err">could not load chains: ${esc(e.message || e)}</div>`;
    return;
  }
  for (const c of CHAINS) if (!c.testnet) SELECTED.add(c.id);
  renderChainList();
  // Deep link from the Overview: ?account=0x…
  const q = new URLSearchParams(location.search).get('account');
  if (q && ADDRESS_CI_RE.test(q.trim())) {
    $('account').value = q.trim();
    await setAccount(q);
  }
})();
