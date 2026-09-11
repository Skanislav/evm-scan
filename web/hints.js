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
