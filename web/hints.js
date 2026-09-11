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

  const assets = H.assets() || [];
  body.innerHTML = `
    <p class="prose" style="margin:6px 0 16px">
      The daemon publishes one membership filter over every <em>(account, contract)</em> pair it has
      indexed. It is a static file, the same bytes for every visitor, so downloading it says nothing
      about who downloaded it — and the test runs in this tab. Nothing here tells the daemon which
      address you asked about.
    </p>
    <div class="kvgrid" id="private-facts" style="gap:28px; margin-bottom:18px"></div>
    <p class="hint" style="max-width:74ch; margin-bottom:6px">
      <strong>What this can and cannot say.</strong> The index covers
      ${assets.length ? `the ${H.esc(String(assets.length))} contract${assets.length === 1 ? '' : 's'} this deployment has promoted`
                      : 'only the contracts this deployment has promoted'},
      not the chain — so a contract missing below is one nobody here indexes, which is a different
      thing from a balance of zero. The file is true as of block
      ${H.esc(fmtInt(m.to_block || 0))}; anything newer than that is not in it yet. And roughly one
      hit in 256 is the filter guessing, which is why every hit is confirmed by reading the balance
      on chain.
    </p>
    <div class="row" style="gap:10px; margin:16px 0 0; flex-wrap:wrap">
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
      <div style="font-size:26px; font-variant-numeric:tabular-nums">${H.esc(v)}</div>
      <div class="kicker" style="margin-top:4px">${H.esc(k)}</div>
      <div class="hint" style="margin-top:2px">${H.esc(note)}</div>
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
      throw new Error(`this filter is for chain ${f.chainId} and the page is now on chain ${H.chainId()}; reopen the panel`);
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

function renderResult(out, r) {
  const { account, resolved, assets, hits, manifest } = r;
  
  out.innerHTML = `
    <div class="kicker">the index, as of block ${H.esc(fmtInt(manifest.to_block || 0))}</div>
    <p class="prose" style="margin:8px 0 4px">
      <strong>${H.esc(String(hits.length))}</strong> of ${H.esc(String(assets.length))} indexed
      contract${assets.length === 1 ? '' : 's'} ${hits.length === 1 ? 'has' : 'have'} a row for
      <span class="addr">${H.esc(resolved.name || account)}</span>.
    </p>
    <p class="hint" style="margin:0 0 14px">
      Worked out from a file this browser already had. The daemon served the file and learned
      nothing about the address it was tested against.${nameNote(resolved)}
    </p>
    <div class="scroll"><div id="private-rows"></div></div>`;

  const rows = $('private-rows');
  if (!hits.length) {
    rows.innerHTML = `<p class="empty">No indexed contract has a row for this account. That means
      none of the contracts <em>this deployment indexes</em> has seen it — it says nothing about
      what the account holds elsewhere.</p>`;
    return;
  }
  rows.innerHTML = hits.map(a => `
    <div class="tablerow cols-hold" data-addr="${H.esc(a.address)}"
         style="grid-template-columns: 2fr 1.4fr 1fr">
      <span>
        <strong>${H.esc(a.symbol || '—')}</strong>
        <span class="hint" style="margin-left:8px">${H.esc(a.name || '')}</span>
        <div class="addr">${H.esc(a.address)}</div>
      </span>
      <span class="num" data-balance>…</span>
      <span class="num"><span class="tag">in the index</span></span>
    </div>`).join('');
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

export function openWatchlist() {
  const holdings = (window.evmscanHoldings ? window.evmscanHoldings() : []) || [];
  // Only fungible contracts. A token list is a list of fungible tokens and a KindToken
  // filter is keyed by one address, so NFT rows would go in and never be asked about.
  const tokens = holdings
    .filter(h => h.address && (!h.standard || h.standard === 'erc20'))
    .map(h => ({ address: h.address, symbol: h.symbol || '' }));

  watchPanel().innerHTML = `
    <p class="prose" style="margin:0 0 14px">
      A watchlist is a filter over contracts you care about, with every key blinded under a secret
      only you hold. It can be <em>tested</em> but never <em>read out</em>, so the file is safe
      somewhere that has no business knowing what is in it — this daemon, a gist, IPFS. You read it
      back by walking a public token list and testing each entry; anyone else holds the same file,
      walks the same list, and learns nothing.
    </p>
    <div class="row" style="gap:10px; margin-bottom:16px; flex-wrap:wrap">
      <button class="btn btn-secondary btn-sm" id="watch-tab-build">Build one</button>
      <button class="btn btn-secondary btn-sm" id="watch-tab-open">Open one</button>
    </div>
    <div id="watch-build"></div>
    <div id="watch-read" hidden></div>
    <div class="err" id="watch-err" hidden></div>`;

  $('watch-tab-build').addEventListener('click', () => {
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
    el.innerHTML = `<p class="empty">Look up an account first — its holdings are what a watchlist
      is built from.</p>`;
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
    for (const h of (window.evmscanHoldings ? window.evmscanHoldings() : []) || []) {
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
// Two things, and they answer the same question from opposite ends: a balance read
// at head is true for one moment and belongs to nobody once the tab closes.
//
//   - The set is remembered here, as a bloom, automatically. No file, no prompt. On
//     the next visit it is the fast path: the contracts this account has actually
//     held, asked about immediately, instead of walking a token list to rediscover
//     them. It is deliberately NOT blinded. The balances are public on chain, so a
//     hint that only saves someone the work they could already do leaks nothing —
//     and a secret that has to be re-derived on every page load to read a cache is a
//     secret that makes the cache slower than not having one. Blinding stays for the
//     watchlist above, which is a set the reader chose rather than one the chain
//     already shows.
//
//   - The holdings the index does not keep get offered the on-chain action, which is
//     the only thing here that outlives the browser: requestIndexing funds the asset,
//     the publisher commits it to a root, and HintRegistry answers for it from then
//     on — through the ENS resolver, to any client, with this daemon out of the path.
// ---------------------------------------------------------------------------

const KIND_INTEROP = 3;

// A bloom sized for a wallet.
//
// The reader for this lives in index.html and the writer is here, which is the one
// direction that is safe to implement twice: a builder that is wrong produces a file
// whose own reader rejects it, loudly, rather than one that quietly answers no. The
// probe has to match internal/hintfilter/bloom.go exactly all the same —
// testdata/public-bloom-interop.xorf is what holds the three of them together, and
// testdata/browser-slot-interop.xorf pins this direction.
//
// 1024 bits, matching hintfilter.HintBits. Not 256 — one storage slot is the obvious
// size and the wrong one: measured, it is 9.1% false positives at fifty tokens and
// 13.7% at sixty-five, where 128 bytes is 0.05% and 0.12%. The slot was never
// actually cheaper in any way that matters, either; on Base the difference between
// writing one word and four is a tenth of a cent.
//
// Only the bloom is built here. The fuse filter needs a peeling loop with retries and
// a second implementation of that in a second language is a bad trade at this size;
// a bloom is an OR.
const HINT_BITS = 1024;

function bloomK(m, n) {
  if (n <= 0) return 1;
  return Math.min(24, Math.max(1, Math.round((m / n) * Math.LN2)));
}

// Kirsch-Mitzenmacher, in the same order Go does it: uint32 wrap first, then mod m.
function bloomProbe(key, i, m) {
  const h1 = Number(key & 0xffffffffn);
  const h2 = Number((key >> 32n) & 0xffffffffn) | 1;
  return ((h1 + Math.imul(i, h2)) >>> 0) % m;
}

function bloomAdd(bitmap, key, k, m) {
  for (let i = 0; i < k; i++) {
    const p = bloomProbe(key, i, m);
    bitmap[p >>> 3] |= 0x80 >> (p & 7);
  }
}

// buildSlotHint returns { bytes, bitmap, k, hex } for a set of (chainId, token)
// pairs. `bytes` is the whole .xorf; `hex` is the bare bitmap, which is what gets
// published — whatever stores it already knows its own m and k.
async function buildSlotHint(pairs) {
  const sub = await H.interopSubkey(new Uint8Array(0));
  const keys = [];
  for (const p of pairs) keys.push(await H.interopKey(sub, p.chainId, p.address));
  const uniq = [...new Set(keys.map(String))].map(BigInt);

  const m = HINT_BITS;
  const k = bloomK(m, uniq.length);
  const bitmap = new Uint8Array(m / 8);
  for (const key of uniq) bloomAdd(bitmap, key, k, m);

  // The .xorf wrapper, matching internal/hintfilter/codec.go. chainId 0 because the
  // chain is inside every preimage; epochId -1 because nothing vouches for this.
  const out = new Uint8Array(34 + 8 + 5 + bitmap.length);
  const dv = new DataView(out.buffer);
  out.set(new TextEncoder().encode('XORF'), 0);
  out[4] = 1;
  out[5] = 0;            // not blinded
  out[6] = 3;            // StructureBloom
  dv.setBigUint64(7, 0n);
  out[15] = KIND_INTEROP;
  dv.setBigInt64(16, -1n);
  dv.setBigUint64(24, 0n);
  dv.setUint16(32, 0);   // no descriptor
  dv.setBigUint64(34, BigInt(uniq.length));
  dv.setUint32(42, m);
  out[46] = k;
  out.set(bitmap, 47);

  const hex = '0x' + [...bitmap].map(b => b.toString(16).padStart(2, '0')).join('');
  return { bytes: out, bitmap, k, m, hex, count: uniq.length };
}

const CACHE_PREFIX = 'evmscan.seen.';
const cacheKey = (account) => `${CACHE_PREFIX}${account.toLowerCase()}`;

// Stored as the filter's own bytes, base64, so what sits in localStorage is the same
// artifact that gets published. One representation, not two that can disagree.
function storeSeen(account, bytes) {
  try {
    localStorage.setItem(cacheKey(account), H.bytesToB64url(bytes));
  } catch { /* private window, or full; a cache that cannot be written is not an error */ }
}

export function loadSeen(account) {
  try {
    const s = localStorage.getItem(cacheKey(account));
    if (!s) return null;
    const bytes = H.b64urlToBytes(s);
    return { bytes, filter: H.decode(bytes.buffer) };
  } catch { return null; }
}

export async function afterLookup(account, holdings, indexed) {
  const chainId = H.chainId();
  const pairs = (holdings || [])
    .filter(h => h.address && (!h.standard || h.standard === 'erc20'))
    .map(h => ({ chainId, address: h.address }));

  // Rebuilt from scratch rather than merged. A filter cannot be enumerated, so the
  // old one cannot be read back to add to it — and the holdings on screen are the
  // better answer anyway. (Merging is possible for a bloom, by OR-ing; it is not
  // done here because it would keep sold tokens forever, and this set is cheap to
  // rebuild.)
  let hint = null;
  if (pairs.length) {
    try {
      hint = await buildSlotHint(pairs);
      storeSeen(account, hint.bytes);
    } catch { /* the cache is an optimisation; losing it costs a slower next visit */ }
  }

  renderPreserve(account, holdings, indexed, hint);
}

// ---------------------------------------------------------------------------
// The on-chain action
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

function renderPreserve(account, holdings, indexed, hint) {
  const section = $('preserve');
  const body = $('preserve-body');
  if (!section || !body) return;

  const rows = unkept(holdings, indexed);
  const reg = H.registry() || {};
  section.hidden = false;

  const hintLine = hint ? `
    <p class="hint" style="margin:14px 0 0; max-width:74ch">
      This browser now holds a ${H.esc(String(hint.m / 8))}-byte hint over the
      ${H.esc(String(hint.count))} contract${hint.count === 1 ? '' : 's'} above, keyed by
      <a href="https://eips.ethereum.org/EIPS/eip-7930">ERC-7930</a> so one filter covers every
      chain. Next visit asks about these first instead of walking a token list.
      It is not secret — these balances are public on chain, so a hint that saves someone work
      they could already do gives away nothing.
      <br><code style="word-break:break-all">${H.esc(abbrev(hint.hex))}</code>
      <button class="linkbtn" id="preserve-copy">copy all ${H.esc(String(hint.m / 8))} bytes</button>
    </p>` : '';

  if (!rows.length) {
    $('preserve-count').textContent = '';
    body.innerHTML = `<p class="hint" style="max-width:74ch">Everything above is already an indexed
      asset, so it is committed to a root and served from the registry — this deployment could stop
      running and the answer would still be there.</p>${hintLine}`;
    wireCopy(hint);
    return;
  }

  if (!reg.address) {
    $('preserve-count').textContent = '';
    body.innerHTML = `<p class="hint" style="max-width:74ch">${H.esc(String(rows.length))} of these were
      found by reading the chain just now and are not in the index. This deployment names no registry,
      so there is nowhere to make that permanent from here.</p>${hintLine}`;
    wireCopy(hint);
    return;
  }

  const minWei = BigInt(reg.min_funding_wei || 0);
  $('preserve-count').textContent = `${rows.length} read live, not kept by the index`;
  body.innerHTML = `
    <p class="prose" style="margin:0 0 6px; max-width:74ch">
      These came from reading the chain at head a moment ago. Nothing stores them: close the tab and
      the only way back is to read the chain again. Paying for one registers it in
      <code>HintRegistry</code> on chain ${H.esc(String(reg.chain_id ?? '—'))}, which funds the backfill
      and puts it in the next committed root — after which the registry answers for it directly, to any
      ENS client, with this daemon out of the path.
    </p>
    <p class="hint" style="margin:0 0 14px; max-width:74ch">
      The deposit is not refundable and the transaction is yours, from your own wallet — the daemon only
      says where the registry is and what it costs. Minimum ${H.esc(H.weiToEth(reg.min_funding_wei))} ETH
      each, which buys a fixed number of blocks of coverage rather than a subscription. Paying puts a
      contract in the index; it does not put it at the top of anyone's list.
    </p>
    <div class="scroll"><div id="preserve-rows"></div></div>
    <div class="err" id="preserve-err" hidden></div>${hintLine}`;

  $('preserve-rows').innerHTML = rows.map(h => `
    <div class="tablerow" data-token="${H.esc(h.address)}" style="grid-template-columns: 2fr 1.2fr 1fr">
      <span>
        <strong>${H.esc(h.symbol || '—')}</strong>
        <span class="hint" style="margin-left:8px">${H.esc(h.name || '')}</span>
        <div class="addr">${H.esc(h.address)}</div>
      </span>
      <span class="hint num" data-state>${H.esc(h.standard || 'erc20')}</span>
      <span class="num">
        <button class="btn btn-secondary btn-sm" data-keep="${H.esc(h.address)}"
                data-kind="${KIND_BY_STANDARD[h.standard] || 20}">Keep it</button>
      </span>
    </div>`).join('');

  for (const btn of body.querySelectorAll('[data-keep]')) {
    btn.addEventListener('click', () => keep(btn, minWei).catch(e => {
      const el = $('preserve-err');
      el.hidden = false;
      el.textContent = e.message || String(e);
    }));
  }
  wireCopy(hint);
}

// 128 bytes is 258 hex characters, which is a wall rather than a value. Show enough
// of each end to recognise it and to tell two apart; the button copies the whole
// thing, which is the only form anyone actually uses.
const abbrev = (hex) => hex.length <= 42 ? hex : `${hex.slice(0, 22)}…${hex.slice(-18)}`;

function wireCopy(hint) {
  const btn = $('preserve-copy');
  if (!btn || !hint) return;
  btn.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(hint.hex);
      btn.textContent = 'copied';
    } catch {
      btn.textContent = 'could not copy — select it by hand';
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
