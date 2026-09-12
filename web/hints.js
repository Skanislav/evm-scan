// The private lookup.
//
// Every other way this page answers "what does this account hold" puts the address
// somewhere a server reads it: a path segment on /v1/accounts, calldata at an
// ERC-3668 gateway, an eth_call to whoever runs the RPC. Each of those is
// unforgeable and none of them is unobservable — the operator learns who asked,
// every time, and learns it before they answer.
//
// This path inverts that. The index publishes one membership filter over its
// (account, contract) pairs; it is a static file, byte-identical for every visitor,
// so asking for it says nothing about who is asking. The promoted asset list is
// public too. With both in hand the membership test runs here, in the browser, and
// the daemon is never told which account it was run for.
//
// What that buys is narrow, and saying so precisely is most of this file's job:
//
//   - The index covers *promoted* assets, not the chain. A miss means "this
//     deployment does not index that contract", never "you hold none of it". So
//     this panel reports what the index knows and refuses to call it a portfolio.
//     Narrowing is sound here only because the candidate set *is* the index — the
//     same reasoning that makes it unsound for the cross-chain sweep, which asks a
//     token list a question the index was never built to answer.
//   - The filter is true as of a block, not as of head. An interaction newer than
//     that block is absent from the file, and that is the one way this can produce
//     a false negative. The block is on screen for exactly that reason.
//   - A hit can be wrong about one pair in 256. That is what the live balance read
//     is for: the filter says where to look, the chain says what is there.
//
// The module is imported lazily by index.html and handed its primitives on
// window.evmscanHints, because the filter arithmetic has one implementation on this
// page and a second copy here would be a second thing to keep byte-identical with
// the Go writer in internal/hintfilter.

import * as R from './ensrec.js';

const H = window.evmscanHints;
const $ = (id) => document.getElementById(id);
const KIND_ACCOUNT_TOKEN = 2;

const fmtInt = (n) => Number(n).toLocaleString('en-US');
const fmtBytes = (n) => n < 1024 ? `${n} B`
  : n < 1024 * 1024 ? `${(n / 1024).toFixed(0)} KB`
  : `${(n / (1024 * 1024)).toFixed(1)} MB`;

// Balances arrive as integer strings; the lens reports decimals from the chain and
// marks the ones it could not read, so an unreadable decimals is shown as raw units
// rather than silently scaled by 18.
function fmtUnits(raw, decimals) {
  if (raw === undefined || raw === null) return '';
  if (decimals === undefined || decimals === null) return `${raw} units`;
  const d = Number(decimals);
  const s = String(raw).padStart(d + 1, '0');
  const whole = s.slice(0, s.length - d) || '0';
  const frac = d ? s.slice(s.length - d).replace(/0+$/, '') : '';
  const grouped = BigInt(whole).toLocaleString('en-US');
  return frac ? `${grouped}.${frac.slice(0, 6)}` : grouped;
}

// ---------------------------------------------------------------------------
// The panel
// ---------------------------------------------------------------------------

const body = $('private-body');

function fail(e) {
  body.innerHTML = `<p class="err">${H.esc(e.message || String(e))}</p>`;
}

// Draw the standing explanation plus whatever the manifest says about the file this
// deployment is actually publishing. The numbers come from /v1/hints rather than
// from anything hardcoded here, because a deployment that indexes a different chain
// or promotes a different number of assets should say its own numbers.
async function render() {
  const chainId = H.chainId();
  const name = `index-${chainId}`;
  let m = null;
  try {
    m = (await H.manifest()).find(f => f.name === name) || null;
  } catch (e) {
    return fail(new Error(`could not read /v1/hints: ${e.message}`));
  }

  if (!m) {
    // A deployment with no committed epoch has no index filter to publish. That is
    // an ordinary state, not a failure, and the panel says which of the two it is.
    body.innerHTML = `<p class="prose">This deployment publishes no index filter for chain
      ${H.esc(String(chainId))} yet. One is built from a snapshot when an epoch is committed, so
      until then the only way to ask is <code>Look up</code>, which names the address to the
      daemon.</p>`;
    return;
  }

  // The card around this already says what leaves the browser, what stays, and what
  // the answer is and is not worth. This part is the file's own numbers — which the
  // card cannot know, because they come from whatever this deployment publishes —
  // and the button.
  // A filter is right about its own chain and silent about every other: the keys
  // carry the chain id inside what gets hashed, so testing one chain's file against
  // another's assets misses every probe. The card names the chain in hand so that a
  // refusal below reads as a mismatch rather than as an empty wallet.
  body.innerHTML = `
    <div class="bound" id="private-bound">
      <div class="label" style="margin-bottom:9px">a filter is bound to its chain</div>
      <div class="bound-ok">testing against ${H.esc(H.chainName())} · <code>${H.esc(name)}.xorf</code> is this
        chain's filter, and the keys carry the chain inside what gets hashed</div>
    </div>
    <div class="label" style="margin-bottom:10px">the filter this deployment publishes</div>
    <div class="kvgrid" id="private-facts" style="gap:22px; margin-bottom:16px"></div>
    <div class="row" style="gap:10px; margin:0; flex-wrap:wrap">
      <button class="btn btn-primary btn-sm" id="private-go">Ask the filter</button>
      <span class="hint" id="private-status"></span>
    </div>
    <div class="err" id="private-err" hidden></div>
    <div id="private-out" style="margin-top:18px"></div>`;

  const facts = [
    [fmtInt(m.count), 'pairs in the filter', `${m.structure} over 64-bit keys`],
    [fmtBytes(m.bytes), 'downloaded once', 'cached, and revalidated with an ETag'],
    [fmtInt(m.to_block || 0), 'true as of this block', (m.epoch_id > 0) ? `committed in epoch ${m.epoch_id}` : 'not yet named by an epoch'],
  ];
  $('private-facts').innerHTML = facts.map(([v, k, note]) => `
    <div>
      <div class="k">${H.esc(k)}</div>
      <div class="v" style="font-size:20px">${H.esc(v)}</div>
      <div class="hint" style="margin-top:3px; font-size:11.5px">${H.esc(note)}</div>
    </div>`).join('');

  $('private-go').addEventListener('click', () => run(m).catch(e => {
    const el = $('private-err');
    el.hidden = false;
    el.textContent = e.message || String(e);
  }));
}

// ---------------------------------------------------------------------------
// The lookup itself
// ---------------------------------------------------------------------------

async function run(m) {
  const typed = $('account').value.trim();
  $('private-err').hidden = true;
  if (!typed) throw new Error('type an address above first');

  const go = $('private-go');
  const status = $('private-status');
  const out = $('private-out');
  go.disabled = true;
  try {
    // Names resolve the way they always do on this page: in the browser, through the
    // reader's own RPC. The daemon has no endpoint that takes a name and this panel
    // is not about to be the first.
    status.textContent = 'resolving…';
    const resolved = await H.resolveAccount(typed);
    const account = resolved.address;

    status.textContent = `downloading ${fmtBytes(m.bytes)}…`;
    const f = await H.filter(m.name);

    // Take the kind from the file, not from the name. A filter keyed by single token
    // — the tokens-N list — would happily accept pair keys and answer no to all of
    // them, and "this account has touched nothing" is precisely the wrong answer to
    // arrive at by testing the wrong file. The subkey is derived per kind, so getting
    // this wrong is silent rather than loud unless it is checked here.
    if (f.kind !== KIND_ACCOUNT_TOKEN) {
      throw new Error(`${m.name}.xorf is a ${f.kind === 1 ? 'token' : `kind-${f.kind}`} filter, not the (account, contract) one this reads`);
    }

    // The manifest was resolved when the panel opened; the network chip can have moved
    // since. Testing this chain's filter against another chain's assets misses every
    // probe and reports "0 of N" — a false negative with a confident sentence around
    // it, which is worse than an error. The filter is right about its own chain, so
    // the mismatch is the one thing to refuse.
    if (Number(f.chainId) !== Number(H.chainId())) {
      const msg = `refused: you asked for ${H.chainName()} (chain ${H.chainId()}) while holding chain `
        + `${f.chainId}'s filter. Every probe would miss and it would report a confident `
        + `"0 of ${(H.assets() || []).length}" — a false negative with a sentence around it, which is worse than an error.`;
      const card = $('private-bound');
      if (card) {
        card.innerHTML = `<div class="label" style="margin-bottom:9px">a filter is bound to its chain</div>
          <div class="bound-bad">${H.esc(msg)}</div>`;
      }
      throw new Error(msg);
    }

    // Every asset the index covers, tested locally. Eight keccaks and eight array
    // probes: the cost of this is not the test, it is the download above, which is
    // why the download is the thing that happens once and is cached.
    status.textContent = 'testing locally…';
    const sub = await H.subkey(new Uint8Array(0), f.chainId, f.kind);
    const assets = H.assets() || [];
    if (!assets.length) throw new Error('the asset list has not loaded yet; try again in a moment');
    const hits = [];
    for (const a of assets) {
      if (H.contains(f, await H.pairKey(sub, account, a.address))) hits.push(a);
    }

    renderResult(out, { account, resolved, assets, hits, filter: f, manifest: m });
    status.textContent = '';

    if (hits.length) await confirm(out, account, hits);
  } finally {
    go.disabled = false;
  }
}

// A typed address costs nothing to turn into an address. A typed *name* does: it is
// resolved through an RPC, and that endpoint sees the name and hands back the address,
// which is most of what the filter test was about not disclosing. The daemon is still
// not the one who saw it — but "nothing left the browser" would be false, so the note
// names the host instead of rounding down to a claim that is nearly true.
function nameNote(resolved) {
  if (!resolved.name || !resolved.rpc) return '';
  let host = resolved.rpc;
  try { host = new URL(resolved.rpc).host; } catch { /* keep the raw string */ }
  return ` Resolving <span class="addr">${H.esc(resolved.name)}</span> did go out: <code>${H.esc(host)}</code>
    was asked for it and answered with this address.`;
}

// The private lookup's last answer, kept so the watchlist can be built from it. The
// hosted lookup leaves its holdings on the page; this one leaves them nowhere else,
// and a reader who asked without naming themselves is exactly the reader who wants
// the list that cannot be read.
let PRIVATE_HITS = [];

function renderResult(out, r) {
  const { account, resolved, assets, hits, manifest } = r;
  PRIVATE_HITS = hits.map(a => ({ address: a.address, symbol: a.symbol || '', name: a.name || '', standard: a.standard, unlisted: a.unlisted }));
  
  // How far behind head the answer is, and whether the digest is the chain's word or
  // the host's. A manifest an epoch names was fixed inside a bonded publishIndex; one
  // no epoch names was built from the daemon's live table and vouched for by nobody.
  const head = H.head() || 0;
  const behind = head && manifest.to_block ? head - manifest.to_block : 0;
  const digest = manifest.keccak256 ? `${manifest.keccak256.slice(0, 6)}…${manifest.keccak256.slice(-4)}` : '';
  const bound = (manifest.epoch_id || 0) > 0;
  const onchain = bound ? (H.epochs() || []).find(e => e.id === manifest.epoch_id) : null;
  const epochLabel = onchain && onchain.onchain_epoch_id != null ? `epoch ${onchain.onchain_epoch_id}` : `epoch ${manifest.epoch_id}`;

  out.innerHTML = `
    <div style="display:flex; gap:14px; flex-wrap:wrap; align-items:baseline">
      <div class="kicker">the index, as of block ${H.esc(fmtInt(manifest.to_block || 0))}</div>
      ${digest ? `<span class="colnote">${H.esc(manifest.name)}.xorf · digest ${H.esc(digest)}</span>` : ''}
    </div>
    <p class="prose" style="margin:8px 0 4px">
      <strong>${H.esc(String(hits.length))}</strong> of ${H.esc(String(assets.length))} indexed
      contract${assets.length === 1 ? '' : 's'} ${hits.length === 1 ? 'has' : 'have'} a row for
      <span class="addr">${H.esc(resolved.name || account)}</span>.
    </p>
    <p class="hint" style="margin:0 0 6px">
      Worked out from a file this browser already had. The daemon served the file and learned
      nothing about the address it was tested against.${nameNote(resolved)}
    </p>
    ${behind > 0 ? `<div style="font:400 11.5px/1.5 var(--mono); color:var(--ink-65)">as of filter block
      ${H.esc(fmtInt(manifest.to_block))} — ${H.esc(fmtInt(behind))} block${behind === 1 ? '' : 's'} behind head.
      anything promoted since is invisible here until the next rebuild.</div>` : ''}
    <div class="ebound" style="margin-bottom:14px">
      <span class="ebadge ${bound ? '' : 'rolling'}">${bound ? 'epoch-bound' : 'rolling'}</span>
      <div style="min-width:0">${bound
        ? `This digest is not the host's word for it. The publisher computed the filter from the same snapshot
           that produced the merkle root of ${H.esc(epochLabel)}, and named a manifest carrying that digest
           <em>inside</em> the bonded <code>publishIndex</code> transaction — so the chain says where to look and
           what should be found there. Fixed at a block somebody bonded, which is why it is checkable and behind.
           <code>evmscan-verify -filter</code> is the check.`
        : `This digest is the host's word for it. No finalized epoch names this filter yet, so it was built from
           the daemon's live table: current, and vouched for by nobody — the same daemon states the bytes and their
           digest, and a lying one agrees with itself. Once a publisher commits an epoch, the file served here is
           the one that epoch named, and its digest can be checked against the chain without asking us.`}</div>
    </div>
    <div class="scroll"><div id="private-rows"></div></div>`;

  const rows = $('private-rows');
  if (!hits.length) {
    rows.innerHTML = `<p class="empty">No indexed contract has a row for this account. That means
      none of the contracts <em>this deployment indexes</em> has seen it — it says nothing about
      what the account holds elsewhere.</p>`;
    return;
  }
  // The same impersonation check the wallet table and the triage card run. This panel
  // draws its own rows rather than going through renderHoldings, so without this the
  // one reader who came here *because* they did not want to name their address is the
  // one reader who is not told that the contract they hold is wearing somebody else's
  // name. Only the strong tier: a list of balances is not where a reader adjudicates a
  // symbol, it is where they need a contract they already own to stop lying to them.
  rows.innerHTML = hits.map(a => {
    const fake = H.lookalike ? H.lookalike(a) : null;
    const impostor = fake && fake.tier === 'impersonates' ? fake : null;
    return `
    <div class="tablerow cols-hold" data-addr="${H.esc(a.address)}"
         style="grid-template-columns: 2fr 1.4fr 1fr">
      <span>
        <strong>${H.esc(a.symbol || '—')}</strong>
        <span class="hint" style="margin-left:8px">${H.esc(a.name || '')}</span>${impostor
          ? ` <span class="tag tag-flag" title="This contract is not ${H.esc(impostor.target)}. Its symbol renders as ${H.esc(impostor.target)} and is a different string. A curated token list carries the real ${H.esc(impostor.target)} and does not carry this address.">not ${H.esc(impostor.target)}</span>`
          : ''}
        <div class="addr">${H.esc(a.address)}</div>
      </span>
      <span class="num" data-balance>…</span>
      <span class="num"><span class="tag">in the index</span></span>
    </div>`;
  }).join('');
}

// Confirm every hit against the chain.
//
// This is not belt and braces, it is the other half of the design: a filter is never
// trusted, it only says where to look. About one hit in 256 is the filter guessing,
// and a pair that was real when the filter was built may have been the account's
// last interaction with a token it has since spent to zero. Only the live read
// distinguishes those, and the read is one call for all of them.
//
// It is also where the privacy claim gets its honest caveat. The lens runs wherever
// the reader pointed it; with no endpoint of their own, that is the daemon, which
// hands back the address the filter test just avoided disclosing. The panel says
// which of the two happened, afterwards, naming the host.
async function confirm(out, account, hits) {
  const note = document.createElement('p');
  note.className = 'hint';
  note.style.marginTop = '14px';
  out.appendChild(note);

  const mine = H.chainRpc();
  if (!mine) {
    note.innerHTML = `Balances not read. The filter test above stayed in this tab, but reading a
      balance needs a node, and this browser has none of its own configured — asking the daemon for
      it would hand over the address the test just avoided sending.
      <button class="linkbtn" id="private-read-anyway">read them through the daemon anyway</button>`;
    $('private-read-anyway').addEventListener('click', () => {
      note.textContent = 'reading through the daemon…';
      readThroughDaemon(out, account, hits, note);
    });
    return;
  }

  note.textContent = `reading balances from ${new URL(mine).host}…`;
  try {
    const rows = await H.lensBalances(account, hits.map(a => a.address));
    paint(out, rows);
    // At head, not at the block the filter names. The two numbers on this page are
    // true at different times and a reader comparing them deserves to know which is
    // which: the membership above is as of the filter's block, the balances are now.
    note.innerHTML = `Balances read at head from <code>${H.esc(new URL(mine).host)}</code> in your
      browser — not as of the block above, which is only when the index last said these contracts
      had seen this account. This deployment served the filter and nothing else: it was never told
      which address it was for, and never asked for a balance.`;
  } catch (e) {
    note.innerHTML = `Your RPC could not serve this (${H.esc(e.message)}). Nothing was read, and the
      address was not sent anywhere else to compensate.`;
  }
}

async function readThroughDaemon(out, account, hits, note) {
  try {
    const p = await H.api(H.onChain(
      `/v1/accounts/${account}/portfolio?tokens=${hits.map(a => a.address).join(',')}`));
    paint(out, p.tokens || []);
    note.innerHTML = `Balances read through the daemon at block
      ${H.esc(fmtInt(p.as_of_block || 0))}, which now knows the address — you asked it to. That is a
      later block than the one the filter names above: membership is as of the index, balances are
      as of now. The filter test before it still cost nothing.`;
  } catch (e) {
    note.innerHTML = `Could not read balances: ${H.esc(e.message)}`;
  }
}

// The lens reports a token it could not read as hasBalance=false rather than as a
// zero, so "no balance" and "could not tell" stay different answers here too.
function paint(out, rows) {
  const by = new Map();
  for (const t of rows) {
    const addr = (t.address || t.token || '').toLowerCase();
    if (!addr) continue;
    const has = t.address !== undefined ? (t.balance !== undefined && t.balance !== null)
      : !!t.hasBalance;
    const dec = t.decimals !== undefined ? t.decimals : (t.hasDecimals ? Number(t.decimals) : undefined);
    by.set(addr, has ? { balance: String(t.balance), decimals: dec } : null);
  }
  for (const row of out.querySelectorAll('[data-addr]')) {
    const cell = row.querySelector('[data-balance]');
    const v = by.get(row.dataset.addr.toLowerCase());
    if (v === undefined) { cell.textContent = 'not read'; cell.className = 'num empty'; continue; }
    if (v === null) { cell.textContent = 'unreadable'; cell.className = 'num empty'; continue; }
    const n = fmtUnits(v.balance, v.decimals);
    cell.textContent = n === '0' ? '0' : n;
    cell.className = 'num';
  }
}

render().catch(fail);
// index.html calls this when the network chip moves while the panel is open, so the
// card above names the filter the picked chain publishes rather than the one the
// panel opened on.
window.evmscanPrivateRender = () => render().catch(fail);

// ---------------------------------------------------------------------------
// The private watchlist
//
// The panel above reads a filter somebody else published. This writes one, and the
// inversion is the whole point: blind the keys under a secret only the reader holds
// and the file becomes testable by them and opaque to everyone else, including
// whoever stores it. A filter can be tested but never enumerated, so the dictionary
// attack *is* the read path — you walk a public token list and test each entry, and
// anyone else holding the same file and the same list learns nothing from either.
//
// docs/PRIVACY.md has the threat model. Three things from it belong on screen rather
// than in a document, because they are choices the reader lives with:
//
//   - A passkey is the strongest secret and the least portable. WebAuthn binds a
//     credential to an origin, so a watchlist blinded on a hosted page cannot be
//     opened on localhost or on the reader's own daemon — and for a project whose
//     premise is self-hosting, that has to be said *before* they build a file they
//     cannot reopen, not after.
//   - A published blinded filter is an offline oracle against its own secret: guess,
//     derive, test a thousand popular tokens, and a hit rate far above 0.4% confirms
//     the guess. Against 32 bytes of authenticator entropy that is hopeless; against
//     a human password it is only as hard as the KDF, and WebCrypto's best here is
//     PBKDF2. So the password option says it is the weak one at the moment of choice.
//   - Losing the secret loses the read, and there is no recovery path, because a
//     recoverable blinding is not a blinding.
//
// The file carries a descriptor saying how to re-derive its secret — which provider,
// which credential, which salt — but never the secret. That is what lets the reader
// open a file months later without remembering anything but the passkey tap: the
// file knows what to ask for.
// ---------------------------------------------------------------------------

const KIND_TOKEN = 1;
const SIGN_MESSAGE = 'evmscan/xorf/v1';
const PBKDF2_ITERATIONS = 600000;

const PROVIDERS = {
  passkey: {
    label: 'A passkey',
    strength: 'strongest',
    note: `32 bytes of authenticator entropy — nobody guesses that. The catch is portability:
      WebAuthn binds a credential to this origin, so a list blinded here cannot be opened on
      your own daemon or on localhost.`,
  },
  wallet: {
    label: 'Your wallet',
    strength: 'portable',
    note: `A fixed message, signed deterministically — Ledger and Trezor both use RFC 6979, so
      the same message gives the same bytes forever. Readable on any deployment. A BIP-39
      passphrase gives a different address and therefore a different list, for free.`,
  },
  password: {
    label: 'A password',
    strength: 'weakest — compatibility',
    note: `Stretched with PBKDF2 at 600,000 iterations, because WebCrypto has no Argon2id and
      this page ships no vendored code. A published file is an offline oracle against its own
      password, and that is markedly weaker against a GPU than the two above. Use a passkey
      unless you need this.`,
  },
};

const watchPanel = () => $('watch-panel');

// What the reader holds, from whichever lookup answered: the hosted one leaves its
// rows on the page, the private one leaves them here. Read at click time, because a
// reader looks up more than one account per visit.
function watchSource() {
  const seen = new Set();
  const out = [];
  const hosted = (window.evmscanHoldings ? window.evmscanHoldings() : []) || [];
  for (const h of hosted.concat(PRIVATE_HITS)) {
    const k = (h.address || '').toLowerCase();
    if (!k || seen.has(k)) continue;
    seen.add(k);
    out.push(h);
  }
  return out;
}

// Only fungible contracts. A token list is a list of fungible tokens and a KindToken
// filter is keyed by one address, so NFT rows would go in and never be asked about.
// Same rule as the hint: a set the reader has already triaged should not come back
// untriaged in the one artifact here they might keep for years.
function watchTokens() {
  return watchSource()
    .filter(h => h.address && !h.aside && (!h.standard || h.standard === 'erc20'))
    .map(h => ({ address: h.address, symbol: h.symbol || '' }));
}

export function openWatchlist() {
  const tokens = watchTokens();

  watchPanel().innerHTML = `
    <p class="prose" style="margin:0 0 14px; max-width:74ch">
      Your holdings, saved as a small file that only you can read. Each contract goes in blinded
      under a secret you hold — a passkey, a wallet signature or a password — so the file can sit
      anywhere, this daemon included, and reveal nothing. Open it later with the same secret and
      the page finds your contracts again without asking anyone which ones they are.
    </p>
    <div class="row" style="gap:10px; margin-bottom:16px; flex-wrap:wrap">
      <button class="btn btn-secondary btn-sm" id="watch-tab-build">Build one</button>
      <button class="btn btn-secondary btn-sm" id="watch-tab-open">Open one</button>
    </div>
    <div id="watch-build"></div>
    <div id="watch-read" hidden></div>
    <div class="err" id="watch-err" hidden></div>`;

  // Build reads the holdings when it is clicked, not when the panel opened: the panel
  // can be open before the lookup, and the lookup can be the private one.
  $('watch-tab-build').addEventListener('click', () => {
    renderBuild(watchTokens());
    $('watch-build').hidden = false; $('watch-read').hidden = true;
  });
  $('watch-tab-open').addEventListener('click', () => {
    $('watch-build').hidden = true; $('watch-read').hidden = false;
  });

  renderBuild(tokens);
  renderRead();
  // With nothing to build from, open on the half that works. Reading a watchlist needs
  // no account and no lookup, and landing on "look up an account first" would suggest
  // otherwise to the one reader who came here holding a file.
  if (!tokens.length) $('watch-tab-open').click();
}

function watchErr(e) {
  const el = $('watch-err');
  el.hidden = false;
  el.textContent = e.message || String(e);
}

function providerPicker(idPrefix) {
  return Object.entries(PROVIDERS).map(([key, p], i) => `
    <label style="display:block; margin:10px 0; cursor:pointer">
      <input type="radio" name="${idPrefix}-secret" value="${key}"${i === 0 ? ' checked' : ''}>
      <strong>${H.esc(p.label)}</strong>
      <span class="kicker" style="margin-left:8px">${H.esc(p.strength)}</span>
      <div class="hint" style="margin:2px 0 0 22px; max-width:70ch">${p.note}</div>
    </label>`).join('');
}

const pickedProvider = (idPrefix) =>
  (document.querySelector(`input[name="${idPrefix}-secret"]:checked`) || {}).value || 'passkey';

// ---------------------------------------------------------------------------
// Building
// ---------------------------------------------------------------------------

function renderBuild(tokens) {
  const el = $('watch-build');
  if (!tokens.length) {
    const any = watchSource().length;
    el.innerHTML = any
      ? `<p class="empty">This account's holdings are all NFTs, and a watchlist holds fungible
          contracts only — a token list has nothing to test an NFT contract against.</p>`
      : `<p class="empty">Nothing to build from yet. Look an account up above — by name, or without
          naming yourself — and its holdings become the list.</p>`;
    return;
  }
  el.innerHTML = `
    <div class="kicker">${H.esc(String(tokens.length))} contract${tokens.length === 1 ? '' : 's'} to save</div>
    <p class="hint" style="margin:6px 0 4px">
      ${tokens.map(t => H.esc(t.symbol || H.short(t.address))).join(' · ')}
    </p>
    <p class="hint" style="margin:10px 0 0; max-width:74ch">
      The file hides <em>which</em> contracts, never <em>how many</em>: the count is in the header
      and the length follows from it. Padding to a bucket would fix that and is not built.
    </p>
    <div style="margin-top:14px">${providerPicker('build')}</div>
    <div class="row" id="watch-pw-row" hidden style="margin:8px 0">
      <input class="input" type="password" id="watch-pw" style="flex:1 1 280px"
             autocomplete="new-password" placeholder="a password you will not lose">
    </div>
    <div class="row" style="gap:10px; margin-top:12px; flex-wrap:wrap">
      <button class="btn btn-primary btn-sm" id="watch-build-go">Build and download</button>
      <span class="hint" id="watch-build-status"></span>
    </div>
    <p class="hint" style="margin-top:12px; max-width:74ch">
      Lose the secret and the list is unreadable. There is no recovery path and there should not be
      one — a blinding you can be talked out of is not a blinding.
    </p>`;

  for (const r of el.querySelectorAll('input[name="build-secret"]')) {
    r.addEventListener('change', () => { $('watch-pw-row').hidden = pickedProvider('build') !== 'password'; });
  }
  $('watch-build-go').addEventListener('click', () => build(tokens).catch(watchErr));
}

// Derive a secret and, with it, the descriptor that says how to derive it again. The
// two are produced together on purpose: a file whose descriptor disagrees with how it
// was actually built is one nobody can open, and the failure surfaces months later.
async function deriveForBuild(kind, status) {
  if (kind === 'passkey') {
    status.textContent = 'registering a passkey…';
    const credId = await H.passkeyCreate('evm-scan watchlist');
    status.textContent = 'tap the passkey again to derive the key…';
    const secret = await H.passkeySecret(credId);
    return { secret, desc: { kdf: 'webauthn-prf', cred_id: credId, input: H.bytesToB64url(new TextEncoder().encode(SIGN_MESSAGE)) } };
  }
  if (kind === 'wallet') {
    status.textContent = 'sign the message in your wallet…';
    const { secret, account } = await H.walletSecret();
    return { secret, desc: { kdf: 'eip191', account, message: SIGN_MESSAGE } };
  }
  const pw = $('watch-pw').value;
  if (!pw) throw new Error('type a password first');
  status.textContent = 'stretching the password…';
  const { secret, kdfSalt } = await H.passwordSecret(pw);
  return { secret, desc: { kdf: 'pbkdf2', kdf_salt: kdfSalt, iterations: PBKDF2_ITERATIONS } };
}

async function build(tokens) {
  $('watch-err').hidden = true;
  const status = $('watch-build-status');
  const btn = $('watch-build-go');
  btn.disabled = true;
  try {
    const { secret, desc } = await deriveForBuild(pickedProvider('build'), status);
    const chainId = H.chainId();
    status.textContent = 'building…';
    const bytes = await H.buildWatchlist(tokens.map(t => t.address), chainId,
      secret, { ...desc, chain_id: chainId, built_at: new Date().toISOString() });

    // The download is the durable artifact. The secret is deliberately not: it lives
    // for the life of the page and is re-derived by a tap or a signature next time.
    const blob = new Blob([bytes], { type: 'application/octet-stream' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `watchlist-${chainId}-${new Date().toISOString().slice(0, 10)}.xorf`;
    // Attached before the click: Chrome fires it on a detached node, Firefox does not.
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
    status.innerHTML = `${H.esc(String(tokens.length))} contracts, ${H.esc(String(bytes.length))} bytes,
      blinded under ${H.esc(desc.kdf)}. Saved.`;
  } finally {
    btn.disabled = false;
  }
}

// ---------------------------------------------------------------------------
// Reading one back
// ---------------------------------------------------------------------------

function renderRead() {
  $('watch-read').innerHTML = `
    <p class="prose" style="margin:0 0 12px">
      Open a file you built earlier. It is tested against the token list this page already loads,
      which is what reading a blinded filter means: the file never gives up its contents, so the
      only way in is to ask it about addresses you can name.
    </p>
    <div class="row" style="gap:10px; flex-wrap:wrap">
      <input type="file" id="watch-file" accept=".xorf,application/octet-stream" class="input" style="flex:1 1 280px">
    </div>
    <div id="watch-file-info" style="margin-top:14px"></div>`;
  $('watch-file').addEventListener('change', (e) => {
    const f = e.target.files && e.target.files[0];
    if (f) openFile(f).catch(watchErr);
  });
}

async function openFile(file) {
  $('watch-err').hidden = true;
  const info = $('watch-file-info');
  info.innerHTML = '<span class="empty">reading…</span>';

  const buf = await file.arrayBuffer();
  const f = H.decode(buf);
  if (f.kind !== KIND_TOKEN) {
    throw new Error(`that is a ${f.kind === 2 ? '(account, contract)' : `kind-${f.kind}`} filter, not a watchlist`);
  }
  // A file that is not blinded is readable by everyone who holds it. Say so rather
  // than opening it quietly, because the whole reason to keep one of these anywhere
  // is the belief that it is opaque.
  const d = f.desc || {};
  info.innerHTML = `
    <div class="kicker">${H.esc(String(f.count))} contracts · chain ${H.esc(String(f.chainId))} ·
      ${f.blinded ? H.esc(d.kdf || 'blinded') : 'NOT blinded'}${d.built_at ? ' · built ' + H.esc(String(d.built_at).slice(0, 10)) : ''}</div>
    ${f.blinded ? '' : `<p class="hint" style="margin:6px 0 0">This file is not blinded — anyone
      holding it can test it without a secret.</p>`}
    <div style="margin-top:12px">${f.blinded ? providerPicker('read') : ''}</div>
    <div class="row" id="watch-read-pw-row" hidden style="margin:8px 0">
      <input class="input" type="password" id="watch-read-pw" style="flex:1 1 280px"
             autocomplete="current-password" placeholder="the password it was built with">
    </div>
    <div class="row" style="gap:10px; margin-top:12px; flex-wrap:wrap">
      <button class="btn btn-primary btn-sm" id="watch-read-go">Open it</button>
      <span class="hint" id="watch-read-status"></span>
    </div>
    <div id="watch-read-out" style="margin-top:16px"></div>`;

  // The descriptor says which provider built the file, so preselect it rather than
  // making the reader remember. They can still override — a descriptor is a hint from
  // the file, and the file is the thing whose honesty is in question.
  const preset = { 'webauthn-prf': 'passkey', eip191: 'wallet', pbkdf2: 'password', argon2id: 'password' }[d.kdf];
  if (preset) {
    const r = document.querySelector(`input[name="read-secret"][value="${preset}"]`);
    if (r) r.checked = true;
  }
  const syncPw = () => { $('watch-read-pw-row').hidden = !f.blinded || pickedProvider('read') !== 'password'; };
  for (const r of info.querySelectorAll('input[name="read-secret"]')) r.addEventListener('change', syncPw);
  syncPw();

  $('watch-read-go').addEventListener('click', () => readBack(f, d).catch(watchErr));
}

// Re-derive using what the descriptor recorded. A passkey needs its credential id and
// a PBKDF2 secret needs its salt, and neither is guessable from the file's keys — which
// is why they travel in the header instead of in the reader's memory.
async function deriveForRead(kind, d, status) {
  if (kind === 'passkey') {
    if (!d.cred_id) throw new Error('this file does not name a credential, so its passkey cannot be found');
    status.textContent = 'tap the passkey…';
    return await H.passkeySecret(d.cred_id);
  }
  if (kind === 'wallet') {
    status.textContent = 'sign the message in your wallet…';
    const { secret, account } = await H.walletSecret();
    // A different account signs a different message and derives a different secret,
    // and the file would then report nothing found — which is the same thing it says
    // for a wrong password, and which here has a knowable cause. The descriptor
    // recorded who built it, so say so rather than letting the reader conclude their
    // watchlist is empty.
    if (d.account && account && d.account.toLowerCase() !== account.toLowerCase()) {
      throw new Error(`this file was built by ${H.short(d.account)}, but your wallet is offering ${H.short(account)} — switch accounts and try again`);
    }
    return secret;
  }
  const pw = $('watch-read-pw').value;
  if (!pw) throw new Error('type the password it was built with');
  status.textContent = 'stretching the password…';
  return (await H.passwordSecret(pw, d.kdf_salt)).secret;
}

async function readBack(f, d) {
  const status = $('watch-read-status');
  const out = $('watch-read-out');
  const btn = $('watch-read-go');
  btn.disabled = true;
  try {
    const secret = f.blinded ? await deriveForRead(pickedProvider('read'), d, status) : new Uint8Array(0);
    const sub = await H.subkey(secret, f.chainId, f.kind);

    // The candidate set. The public token list is the dictionary, and the reader's
    // current holdings are added because a watchlist is usually built from them and a
    // long-tail contract that no list carries would otherwise be invisible in its own
    // file — the one failure that would make this feature look broken to the person
    // it works for.
    status.textContent = 'walking the token list…';
    const candidates = new Map();
    for (const h of watchSource()) {
      if (h.address) candidates.set(h.address.toLowerCase(), { address: h.address, symbol: h.symbol || '', from: 'your holdings' });
    }
    try {
      const list = await H.tokenList();
      for (const t of (list.byChain.get(Number(f.chainId)) || [])) {
        const k = t.address.toLowerCase();
        if (!candidates.has(k)) candidates.set(k, { address: t.address, symbol: t.symbol || '', from: list.name });
      }
    } catch { /* no list reachable; holdings alone still answer for most files */ }

    const found = [];
    for (const c of candidates.values()) {
      if (H.contains(f, await H.tokenKey(sub, c.address))) found.push(c);
    }

    status.textContent = '';
    const missing = Number(f.count) - found.length;
    out.innerHTML = `
      <p class="prose" style="margin:0 0 6px">
        <strong>${H.esc(String(found.length))}</strong> of the ${H.esc(String(f.count))} contracts in
        this file turned up in ${H.esc(String(candidates.size))} addresses worth asking about.
      </p>
      ${found.length === 0 ? `<p class="hint">Nothing matched. Either this is the wrong secret — a
        wrong one is indistinguishable from an empty file, by design — or the contracts in it are not
        on any list this page can walk.</p>`
        : missing > 0 ? `<p class="hint">The other ${H.esc(String(missing))} are in the file but were
          never asked about: the dictionary is only as wide as the token list plus what is on screen.</p>`
        : ''}
      <div class="scroll" style="margin-top:10px">${found.map(c => `
        <div class="tablerow" style="grid-template-columns: 1.6fr 2fr 1fr">
          <span><strong>${H.esc(c.symbol || '—')}</strong></span>
          <span class="addr">${H.esc(c.address)}</span>
          <span class="hint num">${H.esc(c.from)}</span>
        </div>`).join('')}</div>`;
  } finally {
    btn.disabled = false;
  }
}

// ---------------------------------------------------------------------------
// What happens after a lookup
//
// A balance read at head is true for one moment and belongs to nobody once the tab
// closes. Three things outlive it, and they answer different questions:
//
//   - The set of contracts this account holds is remembered here, automatically, as
//     the same two records a commitment is made of. On the next visit it is asked
//     about first instead of being rediscovered by whatever the index happens to
//     hold. It is deliberately not secret: these balances are public on chain, so a
//     note that saves someone work they could already do gives away nothing.
//
//   - The held contracts the index does not keep can be voted for. A vote is a
//     priority signal for what gets indexed next, counted once per account and
//     stored blinded; the registry keeps the same counter on chain. Once a
//     contract is indexed its rows are committed to a root, and read.html reads
//     the account back from the registry's ENS name with this daemon out of the
//     path.
//
//   - The holdings the index does not keep get offered the on-chain action:
//     requestIndexing funds the asset, the publisher commits it to a root, and
//     HintRegistry answers for it from then on, with this daemon out of the path.
//
// The list format lives in ensrec.js and nowhere else.
// ---------------------------------------------------------------------------

// commitFor is the set this browser remembers and the vote offers: everything on
// screen that the reader has not set aside. `aside` is the reader's own verdict,
// stored per account in this browser, and honouring it here is what makes that
// verdict mean anything — the memory is what the next lookup spends, so one rebuilt
// from everything on screen would hand the reader their dust back every visit.
//
// ERC-1155 contracts are left out: the lens reads them per id, so a reader page that
// found one in the list could not confirm the balance, and a row it cannot confirm is
// a row it has to explain.
function commitFor(holdings, chainId) {
  const addrs = (holdings || [])
    .filter(h => h.address && !h.aside && h.standard !== 'erc1155')
    .map(h => h.address);
  const text = R.formatContracts(addrs);
  return { chainId, text, contracts: text ? text.split(',') : [] };
}

const CACHE_PREFIX = 'evmscan.seen.';
const cacheKey = (account) => `${CACHE_PREFIX}${account.toLowerCase()}`;

// Stored as the two records HintResolver serves, under their record keys, so the
// memory and the registry's answer share one format (ensrec.js). One
// representation, not two that can disagree.
function storeSeen(account, commit) {
  try {
    if (!commit.contracts.length) { localStorage.removeItem(cacheKey(account)); return; }
    localStorage.setItem(cacheKey(account), JSON.stringify({
      [R.KEY_CONTRACTS]: commit.text, [R.KEY_CHAIN]: String(commit.chainId),
    }));
  } catch { /* private window, or full; a cache that cannot be written is not an error */ }
}

// Returns { chainId, contracts } or null. Anything that does not parse — including
// the base64 filter an earlier version of this page stored under the same key — is
// no cache, not a broken one.
export function loadSeen(account) {
  try {
    const s = localStorage.getItem(cacheKey(account));
    if (!s) return null;
    const rec = JSON.parse(s);
    const contracts = R.parseContracts(rec[R.KEY_CONTRACTS]);
    const chainId = R.parseChain(rec[R.KEY_CHAIN]);
    return contracts.length && chainId !== null ? { chainId, contracts } : null;
  } catch { return null; }
}

export async function afterLookup(account, holdings, indexed) {
  const commit = commitFor(holdings, H.chainId());
  storeSeen(account, commit);
  renderPreserve(account, holdings, indexed, commit);
}

// ---------------------------------------------------------------------------
// The on-chain actions
// ---------------------------------------------------------------------------

// Which of these does the index actually keep? `indexed` is what /v1/accounts
// returned, which is the index's own answer — not the filter's. A filter says where
// to look and is allowed to be wrong; this decides whether to spend money, so it
// asks the thing that knows.
function unkept(holdings, indexed) {
  const kept = new Set((indexed || []).map(a => (a.address || '').toLowerCase()));
  return (holdings || []).filter(h => h.address && !kept.has(h.address.toLowerCase()));
}

const KIND_BY_STANDARD = { erc20: 20, erc721: 21, erc1155: 55 };

function renderPreserve(account, holdings, indexed, commit) {
  const section = $('preserve');
  const body = $('preserve-body');
  if (!section || !body) return;

  const rows = unkept(holdings, indexed);
  const kept = new Set((indexed || []).map(a => (a.address || '').toLowerCase()));
  const reg = H.registry() || {};
  section.hidden = false;
  $('preserve-count').textContent = rows.length ? ` · ${rows.length}` : '';

  const n = commit.contracts.length;
  const wanted = commit.contracts.filter(a => !kept.has(a));
  const voteCard = wanted.length ? `
    <div class="commit" id="vote">
      <div class="unkept-head" style="margin-top:18px">ask for these to be indexed</div>
      <p class="hint" style="margin:10px 0 0; max-width:74ch; text-wrap:pretty">
        ${H.esc(String(wanted.length))} of the ${H.esc(String(n))} contract${n === 1 ? '' : 's'} above are held and not indexed,
        and not set aside. One vote each, counted once per account. Votes order what gets promoted next and, once
        enough accounts have asked for a contract, promote it on their own — a spam verdict still outranks them, and
        nothing about a balance changes. Free: no wallet, no gas.
      </p>
      <div class="row" style="gap:10px; margin-top:12px; flex-wrap:wrap">
        <button class="btn btn-primary btn-sm" id="vote-send">Vote for ${H.esc(String(wanted.length))}</button>
        <span class="hint" id="vote-status"></span>
      </div>
      <p class="hint" style="margin:8px 0 0; max-width:74ch; text-wrap:pretty">
        <strong>What this tells the deployment:</strong> your address and these ${H.esc(String(wanted.length))} contracts,
        which a lookup by name already told it and a private lookup did not. The vote is hashed under a
        per-deployment salt, so the daemon counts you once and holds no list of who holds what.
      </p>
      ${reg.address ? `
      <div class="row" style="gap:10px; margin-top:14px; flex-wrap:wrap">
        <button class="btn btn-secondary btn-sm" id="vote-chain">Vote on chain ${H.esc(String(reg.chain_id ?? ''))}</button>
        <span class="hint" id="vote-chain-status"></span>
      </div>
      <p class="hint" style="margin:8px 0 0; max-width:74ch; text-wrap:pretty">
        The same vote, on the registry itself — <code>HintRegistry.vote</code> from your wallet, one transaction,
        gas only. Counted once per address by the contract and read by every indexer that mirrors the registry,
        this one included, so it outlives this deployment. Your wallet address is on chain with it.
      </p>` : ''}
    </div>` : '';

  if (!rows.length) {
    body.innerHTML = `<p class="hint" style="max-width:74ch">Everything above is already an indexed
      asset, so it is committed to a root and served from the registry — this deployment could stop
      running and the answer would still be there.</p>${voteCard}`;
    wireVote(account, commit, wanted);
    return;
  }

  // A new asset costs the bond plus the least funding requestIndexing accepts; the
  // bond comes back through revokeAsset, the funding never does. Both numbers are
  // the registry's own, read by the daemon, and shown before the button rather than
  // discovered in the wallet prompt.
  const bondWei = BigInt(reg.asset_bond_wei || 0);
  const fundWei = BigInt(reg.min_funding_wei || 0);
  const cost = (bondWei > 0n ? `${H.weiToEth(bondWei)} ETH bond + ` : '')
    + `${H.weiToEth(fundWei)} ETH funding`;
  const paid = !!reg.address;

  body.innerHTML = `
    <p class="hint" style="margin:0 0 14px; max-width:680px; text-wrap:pretty">Nobody has registered or promoted
      these, so no per-account index exists for them and the next reader starts from nothing. ${paid
        ? 'You can change that from your own wallet — whatever you pay above the bond becomes that asset\'s funding, and funding buys blocks of coverage for that asset only.'
        : 'This deployment names no registry, so there is nowhere to make that permanent from here.'}</p>
    ${rows.map(h => `
    <div class="unkept-row" data-token="${H.esc(h.address)}">
      <div class="who">
        <div class="sym">${H.esc(h.symbol || h.name || H.short(h.address, 4))}<span class="std">${H.esc(STD_LABEL[h.standard] || h.standard || 'ERC-20')}</span></div>
        <div style="font:400 11px/1.5 var(--mono); color:var(--ink-65)" data-state>${H.esc(H.short(h.address, 4))} · ${H.esc(whyUnkept(h))}</div>
      </div>
      ${paid ? `<div class="act">
        <span class="cost">${H.esc(cost)}</span>
        <button class="btn btn-primary btn-sm" data-keep="${H.esc(h.address)}"
                data-kind="${KIND_BY_STANDARD[h.standard] || 20}">Pay to index it</button>
      </div>` : ''}
    </div>`).join('')}
    <div class="err" id="preserve-err" hidden></div>
    <div class="unkept-foot">Whether an asset is kept comes from what the index actually returned, never from the
      membership filter. A filter says where to look and is allowed to be wrong about one pair in 256; this decides
      whether to spend money, so it does not get to guess.${paid
        ? ' The deposit is not refundable and the transaction is yours, from your own wallet — the daemon only says where the registry on chain ' + H.esc(String(reg.chain_id ?? '—')) + ' is and what it costs. Paying puts a contract in the index; it does not put it at the top of anyone\'s list.'
        : ''}</div>${voteCard}`;

  for (const btn of body.querySelectorAll('[data-keep]')) {
    btn.addEventListener('click', () => keep(btn, bondWei + fundWei).catch(e => {
      const el = $('preserve-err');
      el.hidden = false;
      el.textContent = e.message || String(e);
    }));
  }
  wireVote(account, commit, wanted);
}

const STD_LABEL = { erc20: 'ERC-20', erc721: 'ERC-721', erc1155: 'ERC-1155' };

// Why the index has nothing for a contract this account holds. Three different
// reasons, and a reader deciding whether to pay should know which one it is.
function whyUnkept(h) {
  if (h.standard === 'erc1155') return 'per-id balances · the index tracks contracts, not ids';
  const c = (H.candidates() || []).find(x => x.address && x.address.toLowerCase() === h.address.toLowerCase());
  if (c && c.verdict === 'spam') return 'marked spam by the curator · discovery still counts it';
  if (c) return `discovery found it · ${fmtInt(c.event_count || 0)} events · nobody registered it, no verdict yet`;
  return 'read live at head · no hint registered, not seen by discovery';
}

function wireVote(account, commit, wanted) {
  const onchain = $('vote-chain');
  if (onchain && wanted.length) {
    onchain.addEventListener('click', async () => {
      const status = $('vote-chain-status');
      onchain.disabled = true;
      status.textContent = 'confirm in your wallet…';
      try {
        const tx = await H.voteOnChain(commit.chainId, wanted);
        status.innerHTML = `sent · <code>${H.esc(String(tx).slice(0, 12))}…</code> · the mirror picks it up within a minute of it mining`;
        onchain.textContent = 'voted on chain';
      } catch (e) {
        status.textContent = e.message || String(e);
        onchain.disabled = false;
      }
    });
  }
  const btn = $('vote-send');
  if (!btn || !wanted.length) return;
  btn.addEventListener('click', async () => {
    const status = $('vote-status');
    btn.disabled = true;
    status.textContent = 'sending…';
    try {
      const r = await vote(account, commit.chainId, wanted);
      const counts = Object.values(r.voters || {});
      const top = counts.length ? Math.max(...counts) : 0;
      status.innerHTML = `recorded · ${H.esc(String(r.recorded))} new · the most-wanted of these now has
        ${H.esc(String(top))} voter${top === 1 ? '' : 's'}${r.min_voters
          ? ` · ${H.esc(String(r.min_voters))} promote it on this deployment`
          : ' · this deployment promotes by hand, votes order its queue'}`;
      btn.textContent = 'voted';
    } catch (e) {
      status.textContent = e.message || String(e);
      btn.disabled = false;
    }
  });
}

async function keep(btn, minWei) {
  $('preserve-err').hidden = true;
  const token = btn.dataset.keep;
  const kind = btn.dataset.kind;
  const label = btn.textContent;
  btn.disabled = true;
  btn.textContent = 'confirm in wallet…';
  try {
    // From the head this node can actually serve, not from genesis. A fromBlock below
    // the history floor buys range the light client cannot answer for, and the deposit
    // does not come back. requestIndexing re-checks it; this is so the common case
    // does not have to fail first.
    const head = H.head() || 0;
    const from = Math.max(0, head - 4000);
    const tx = await H.requestIndexing(token, kind, String(from), minWei);
    btn.textContent = 'submitted';
    const row = btn.closest('[data-token]');
    const state = row.querySelector('[data-state]');
    if (state) state.innerHTML = `registered · <code>${H.esc(String(tx).slice(0, 12))}…</code>`;
  } catch (e) {
    btn.disabled = false;
    btn.textContent = label;
    throw e;
  }
}


// ---------------------------------------------------------------------------
// Voting
//
// A vote is the reader's one write, and it is deliberately small: a POST naming the
// account and the contracts, no wallet, no gas. It is a priority signal for what the
// daemon indexes next — the queue orders by it, and with min_voters set a contract
// enough accounts asked for is promoted on its own. It changes nothing about what is
// true: a spam verdict still drops a contract from the promotable set and every
// balance is still read from the chain.
//
// The list itself is never published anywhere. An earlier version wrote it to an ENS
// text record, which cost about 31,000 gas per contract on mainnet and stored, per
// wallet, what a counter per contract stores once for everyone. The registry keeps
// that counter on chain (HintRegistry.vote); this is the daemon-side half.
// ---------------------------------------------------------------------------

async function vote(account, chainId, assets) {
  const res = await fetch('/v1/demand', {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ chain_id: chainId, account, assets }),
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error ? `${body.error}${body.detail ? ': ' + body.detail : ''}` : `${res.status} ${res.statusText}`);
  return body;
}

// ---------------------------------------------------------------------------
// Spending one
//
// Everything above remembers what an account held. This is the half that reads it
// back, from the copy this browser kept last visit — free, instant, and gone with
// the browser profile. Nothing is read from ENS any more: the list is no longer
// published there, and a vote cannot be read back into a wallet, which is the point
// of a vote.
//
// What the memory is allowed to do is the part worth being exact about, because the
// index filter got this wrong once and hid 64 of an account's 65 holdings:
//
//   - It ADDS contracts to the list of what gets asked about — the contracts this
//     account held last visit, whether or not the index has ever heard of them.
//   - It ORDERS the candidates the index offered, so a wallet with more of them than
//     the per-lookup cap spends that cap on contracts this account has held.
//   - It REMOVES nothing, ever. Absence means "not held last visit", which is not a
//     statement about now, and every row is checked against the chain like every
//     other.
// ---------------------------------------------------------------------------

// seedFor gathers what this browser already knows about this account. Fail-open: a
// full or missing localStorage costs a slower answer, never the answer itself.
export async function seedFor(account) {
  const sources = [], notes = [];
  const local = loadSeen(account);
  if (local) {
    sources.push({ where: 'this browser', chainId: local.chainId, set: new Set(local.contracts), count: local.contracts.length });
  }
  return {
    sources, notes,
    empty: sources.length === 0,
    has(chainId, address) {
      const k = String(address || '').toLowerCase();
      return sources.some(s => String(s.chainId) === String(chainId) && s.set.has(k));
    },
  };
}

// vouchedBy is every contract the commitment names for this chain: the union of the
// sources, which the caller then asks the chain about whether or not the index ever
// offered them.
export async function vouchedBy(seed, chainId) {
  const out = new Set();
  if (!seed || seed.empty) return { tokens: [] };
  for (const s of seed.sources) {
    if (String(s.chainId) !== String(chainId)) continue;
    for (const a of s.set) out.add(a);
  }
  return { tokens: [...out] };
}

// mark returns the lowercased subset of `addrs` the commitment names.
//
// The caller sorts by it and then applies a cap, which is the one place a
// commitment decides what does *not* get asked about — and it decides it inside a
// budget that already existed and was previously spent in discovery order. Nothing
// is dropped that the cap would not have dropped anyway; what changes is which side
// of it the contracts this account has actually held land on.
export async function mark(seed, chainId, addrs) {
  const hit = new Set();
  if (!seed || seed.empty) return hit;
  for (const a of addrs) if (seed.has(chainId, a)) hit.add(a.toLowerCase());
  return hit;
}
