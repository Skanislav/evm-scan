// Native USDC only. Sources and route maintenance: docs/SEND.md.
export const ROUTES = [
  [1, 'Ethereum', 0, '0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48'],
  [10, 'OP Mainnet', 2, '0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85'],
  [42161, 'Arbitrum', 3, '0xaf88d065e77c8cC2239327C5EDb3A432268e5831'],
  [8453, 'Base', 6, '0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913'],
  [11155111, 'Ethereum Sepolia', 0, '0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238', true],
  [84532, 'Base Sepolia', 6, '0x036CbD53842c5426634e7929541eC2318f3dCF7e', true],
].map(([id, name, domain, token, testnet = false]) => Object.freeze({ id, name, domain, token, testnet,
  messenger: testnet ? '0x8FE6B999Dc680CcFDD5Bf7EB0974218be2542DAA' : '0x28b5a0e9C621a5BadaA536219b3a228C8168cf5d',
  transmitter: testnet ? '0xE737e5cEBEEBa77EFE34D4aa090756590b1CE275' : '0x81D40F21F12A8F0E3252Bccb954D722d4c464B64',
  iris: testnet ? 'https://iris-api-sandbox.circle.com' : 'https://iris-api.circle.com',
}));
export const routeFor = (chain, token) => ROUTES.find(r => r.id === Number(chain) && r.token.toLowerCase() === token.toLowerCase());
export const destinations = source => ROUTES.filter(r => r.testnet === source.testnet && r.id !== source.id);
export const BURN = ['function depositForBurn(uint256 amount,uint32 destinationDomain,bytes32 mintRecipient,address burnToken,bytes32 destinationCaller,uint256 maxFee,uint32 minFinalityThreshold)'];
export const RECEIVE = ['function receiveMessage(bytes message,bytes attestation) returns (bool)'];
const ZERO = '0x' + '00'.repeat(32);
export function amountUnits(input, decimals, allowZero = false) {
  if (!Number.isInteger(decimals) || decimals < 0 || decimals > 255) throw Error('Token decimals unavailable');
  if (!/^(0|[1-9]\d*)(\.\d+)?$/.test(input)) throw Error('Enter a plain decimal amount');
  const [whole, fraction = ''] = input.split('.');
  if (fraction.length > decimals) throw Error(`Use at most ${decimals} decimal places`);
  const n = BigInt(whole + fraction.padEnd(decimals, '0'));
  if ((!allowZero && n === 0n) || n >= 2n ** 256n) throw Error('Amount is out of range');
  return n;
}
export function address(v, value) {
  value = value.trim();
  if (!v.isAddress(value, { strict: true }) || /^0x0{40}$/i.test(value)) throw Error('Enter a valid, nonzero recipient address');
  return v.getAddress(value);
}
export function build(v, { chainId, token, recipient, amount, decimals, destination, maxFee = '0' }) {
  const to = address(v, token), recipientAddress = address(v, recipient);
  const units = amountUnits(amount, decimals);
  if (!Number.isSafeInteger(chainId) || chainId <= 0) throw Error('Invalid source network');
  if (!destination) return { chainId, to, value: '0x0', data: v.encodeFunctionData({ abi: v.erc20Abi, functionName: 'transfer', args: [recipientAddress, units] }) };
  const source = routeFor(chainId, token), dest = ROUTES.find(r => r.id === destination);
  if (!source || !dest || !destinations(source).includes(dest) || decimals !== 6) throw Error('Unsupported CCTP route: native USDC only');
  const fee = amountUnits(maxFee, 6, true);
  if (fee >= units || units > 10_000_000n * 10n ** 6n) throw Error('CCTP amount must exceed its fee cap and be at most 10 million USDC');
  return { chainId, to: source.messenger, value: '0x0', data: v.encodeFunctionData({ abi: v.parseAbi(BURN), functionName: 'depositForBurn', args: [units, dest.domain, v.pad(recipientAddress, { size: 32 }), to, ZERO, fee, 2000] }) };
}
export async function checkWallet(provider, account, chainId) {
  const accounts = await provider.request({ method: 'eth_accounts' });
  if (accounts[0]?.toLowerCase() !== account.toLowerCase()) throw Error('Wallet account changed. Reconnect the account being sent from.');
  if (BigInt(await provider.request({ method: 'eth_chainId' })) !== BigInt(chainId)) throw Error('Wallet network changed. Switch to the transaction network.');
}
// Receipt timeout retains the hash; it must never be interpreted as permission to resend.
export async function receipt(provider, hash, chainId, attempts = 150, pause = ms => new Promise(r => setTimeout(r, ms))) {
  for (let i = 0; i < attempts; i++) {
    if (BigInt(await provider.request({ method: 'eth_chainId' })) !== BigInt(chainId)) throw Error('Switch back to the transaction network and check its receipt.');
    const r = await provider.request({ method: 'eth_getTransactionReceipt', params: [hash] });
    if (r) {
      if (BigInt(r.status) !== 1n) throw Object.assign(Error('Transaction reverted on chain'), { reverted: true });
      return r;
    }
    await pause(2000);
  }
  throw Error('Still pending. Keep this hash and check the receipt before sending again.');
}
// Bind Iris bytes to the exact MessageSent emitted by the source transmitter.
export function sourceMessage(v, logs, source) {
  const abi = v.parseAbi(['event MessageSent(bytes message)']);
  const messages = logs.filter(l => l.address.toLowerCase() === source.transmitter.toLowerCase()).flatMap(l => {
    try { return [v.decodeEventLog({ abi, data: l.data, topics: l.topics }).args.message]; } catch { return []; }
  });
  if (messages.length !== 1) throw Error('Expected exactly one CCTP message in the burn receipt');
  return messages[0];
}
// Iris assigns the nonce and updates executed finality, fee and expiry; transfer identity stays fixed.
export function sameTransfer(original, attested) {
  if (!/^0x[0-9a-f]+$/i.test(attested) || attested.length !== original.length || original.length < 754) return false;
  const part = (s, a, b) => s.slice(2 + a * 2, 2 + b * 2).toLowerCase();
  return part(original, 0, 12) === part(attested, 0, 12) &&
    part(original, 44, 144) === part(attested, 44, 144) &&
    part(original, 148, 312) === part(attested, 148, 312) &&
    part(original, 376, (original.length - 2) / 2) === part(attested, 376, (attested.length - 2) / 2);
}

export function mount({ root, viem, resolveAccount, currentAccount }) {
  let selected, account, v, plan, busy = false, pending;
  const $ = id => root.querySelector(`[data-send="${id}"]`);
  root.innerHTML = `<details><summary>Send tokens · resume a USDC bridge</summary>
    <p data-send="asset">Choose Send beside a token holding.</p>
    <fieldset data-send="fields" disabled>
      <label>Recipient address or ENS name <input data-send="recipient" autocomplete="off" placeholder="0x… or name.eth"></label>
      <label>Amount <input data-send="amount" inputmode="decimal" placeholder="0.00"></label>
      <label>Destination <select data-send="destination"></select></label>
      <label data-send="fee-wrap" hidden>Maximum CCTP fee (USDC) <input data-send="fee" value="0" inputmode="decimal"></label>
      <p data-send="bridge-note" hidden>CCTP burns native USDC, then mints it after Circle attests. Standard transfers usually take 15–19 minutes. You pay gas on both networks and confirm each transaction. Circle can pause transfers and freeze USDC; its API sees your IP and burn hash. <a href="https://developers.circle.com/cctp/concepts/fees" target="_blank" rel="noopener noreferrer">Check current CCTP fees</a> before choosing a fee cap; a cap below the required fee makes the burn revert.</p>
    </fieldset>
    <button class="btn" data-send="review" disabled>Connect wallet & review</button>
    <pre data-send="preview" style="white-space:pre-wrap;overflow-wrap:anywhere"></pre>
    <button class="btn btn-primary" data-send="execute" hidden>Confirm in wallet</button>
    <p data-send="status" role="status" style="overflow-wrap:anywhere"></p>
    <details><summary>Resume CCTP from a burn transaction</summary>
      <label>Source network <select data-send="source"></select></label>
      <label>Burn transaction hash <input data-send="hash" placeholder="0x…"></label>
      <button class="btn" data-send="resume">Check attestation</button>
      <p class="hint">The last burn is saved in this browser. Keep its hash to recover from another browser. Minting is permissionless; the recipient is fixed by the burn.</p>
    </details></details>`;
  for (const r of ROUTES) $('source').add(new Option(r.name, r.id));
  try { const j = JSON.parse(localStorage.getItem('evmscan.cctp.last')); if (j) { $('source').value = j.source; $('hash').value = j.hash; } } catch { /* manual recovery remains available */ }
  const say = s => { $('status').textContent = s; };
  const clear = () => { plan = null; $('execute').hidden = true; $('preview').textContent = ''; };
  const run = async fn => {
    if (busy) return;
    busy = true;
    root.querySelectorAll('button,input,select,fieldset').forEach(e => { e.disabled = true; });
    try { await fn(); } catch (e) { say((e.code === 4001 ? 'Cancelled in wallet.' : e.shortMessage || e.message || 'Wallet request failed') + (pending ? ` Submitted hash: ${pending.hash}. Use Check submitted transaction; do not resend.` : '')); }
    finally {
      busy = false;
      root.querySelectorAll('button,input,select,fieldset').forEach(e => { e.disabled = false; });
      $('fields').disabled = !selected || !!pending;
      $('review').disabled = !selected || !!pending;
    }
  };
  async function connect(chainId, expected) {
    if (!window.ethereum) throw Error('No injected wallet found');
    const [a] = await window.ethereum.request({ method: 'eth_requestAccounts' });
    if (!a || (expected && a.toLowerCase() !== expected.toLowerCase())) throw Error('Connect the wallet that owns this holding');
    if (BigInt(await window.ethereum.request({ method: 'eth_chainId' })) !== BigInt(chainId)) {
      say(`Switch your wallet to network ${chainId}…`);
      await window.ethereum.request({ method: 'wallet_switchEthereumChain', params: [{ chainId: '0x' + chainId.toString(16) }] });
    }
    await checkWallet(window.ethereum, a, chainId);
    return a;
  }
  async function simulate(tx, from) {
    await checkWallet(window.ethereum, from, tx.chainId);
    const request = { from, to: tx.to, data: tx.data, value: tx.value };
    const result = await window.ethereum.request({ method: 'eth_call', params: [request, 'latest'] });
    if (result && /^0x0{64}$/.test(result)) throw Error('Token returned false; transfer refused');
    const gas = await window.ethereum.request({ method: 'eth_estimateGas', params: [request] });
    const gasPrice = await window.ethereum.request({ method: 'eth_gasPrice' });
    return `Estimated gas: ${BigInt(gas)} units, approximately ${v.formatEther(BigInt(gas) * BigInt(gasPrice))} native coin. Wallet shows the final fee.`;
  }
  function showPlan(tx, kind, from, note, extra = {}) {
    plan = { tx, kind, from, ...extra };
    $('preview').textContent = note + '\n\n' + JSON.stringify(tx, null, 2);
    $('execute').textContent = kind === 'approve' ? 'Approve exact amount in wallet' : kind === 'mint' ? 'Mint USDC in wallet' : kind === 'burn' ? 'Burn USDC in wallet' : 'Send in wallet';
    $('execute').hidden = false;
  }
  $('fields').addEventListener('input', clear);
  $('destination').addEventListener('change', () => {
    clear(); $('fee-wrap').hidden = $('bridge-note').hidden = !$('destination').value;
  });
  $('review').onclick = () => run(async () => {
    clear(); v = await viem();
    const s = { ...selected }, owner = account;
    if (currentAccount().toLowerCase() !== owner.toLowerCase()) throw Error('Account on screen changed. Choose the token again.');
    const from = await connect(s.chainId, owner);
    const resolved = await resolveAccount($('recipient').value.trim());
    const recipient = address(v, resolved.address);
    const amount = $('amount').value.trim(), destination = Number($('destination').value) || undefined;
    const maxFee = $('fee').value.trim();
    const decimals = Number(await v.createPublicClient({ transport: v.custom(window.ethereum) }).readContract({ address: s.address, abi: v.erc20Abi, functionName: 'decimals' }));
    const units = amountUnits(amount, decimals);
    const client = v.createPublicClient({ transport: v.custom(window.ethereum) });
    const balance = await client.readContract({ address: s.address, abi: v.erc20Abi, functionName: 'balanceOf', args: [from] });
    if (units > balance) throw Error('Amount exceeds your current balance');
    const tx = build(v, { chainId: s.chainId, token: s.address, recipient, amount, decimals, destination, maxFee });
    let kind = destination ? 'burn' : 'transfer';
    const source = routeFor(s.chainId, s.address);
    if (destination) {
      const allowance = await client.readContract({ address: s.address, abi: v.erc20Abi, functionName: 'allowance', args: [from, source.messenger] });
      if (allowance < units) {
        tx.to = s.address; tx.data = v.encodeFunctionData({ abi: v.erc20Abi, functionName: 'approve', args: [source.messenger, units] }); kind = 'approve';
      }
    }
    const gas = await simulate(tx, from);
    const usd = s.value_usd && BigInt(s.balance) > 0n ? ` (~$${(Number(s.value_usd) * Number(units) / Number(s.balance)).toFixed(2)})` : ' (USD estimate unavailable)';
    showPlan(tx, kind, from, `${kind === 'approve' ? 'Approve spending for this transfer; review again after approval.\n' : ''}${amount} ${s.symbol || 'tokens'}${usd}\nFrom: ${from}\nToken: ${s.address}\nRecipient: ${recipient}\nNetwork: ${s.chainName || s.chainId}${destination ? ' → ' + ROUTES.find(r => r.id === destination).name + '\nMaximum fee: ' + maxFee + ' USDC; minimum received: ' + v.formatUnits(units - amountUnits(maxFee, 6, true), 6) + ' USDC' : ''}\n${gas}`, { source });
    say('Review the recipient, network, amount and calldata before confirming.');
  });
  async function settle() {
    const p = pending;
    let r;
    try { r = await receipt(window.ethereum, p.hash, p.tx.chainId); }
    catch (e) { if (e.reverted) { pending = null; clear(); } throw e; }
    pending = null; $('execute').hidden = true;
    if (p.kind === 'burn') say(`Burn confirmed: ${p.hash}. Check attestation below to finish on the destination network.`);
    else say(`${p.kind === 'mint' ? 'USDC minted' : p.kind === 'approve' ? 'Approval confirmed — review the transfer next' : 'Transfer confirmed — refresh holdings to read the new balance'}: ${p.hash}`);
    return r;
  }
  $('execute').onclick = () => run(async () => {
    if (pending) { await connect(pending.tx.chainId, pending.from); await settle(); return; }
    const p = plan; if (!p) throw Error('Review the transfer first');
    if (p.kind !== 'mint' && currentAccount().toLowerCase() !== account.toLowerCase()) { clear(); throw Error('Account on screen changed. Review again.'); }
    await simulate(p.tx, p.from);
    await checkWallet(window.ethereum, p.from, p.tx.chainId);
    say('Confirm in your wallet…');
    const hash = await window.ethereum.request({ method: 'eth_sendTransaction', params: [{ from: p.from, to: p.tx.to, data: p.tx.data, value: p.tx.value, chainId: '0x' + p.tx.chainId.toString(16) }] });
    pending = { ...p, hash }; plan = null;
    $('execute').textContent = 'Check submitted transaction';
    say(`Submitted: ${hash}. Waiting for confirmation…`);
    if (p.kind === 'burn') {
      $('source').value = p.source.id; $('hash').value = hash;
      try { localStorage.setItem('evmscan.cctp.last', JSON.stringify({ source: p.source.id, hash })); } catch { say(`Save your burn hash for recovery: ${hash}`); }
    }
    await settle();
  });
  $('resume').onclick = () => run(async () => {
    if (pending) throw Error('Check the submitted transaction above first');
    clear(); v = await viem();
    const source = ROUTES.find(r => r.id === Number($('source').value)), hash = $('hash').value.trim();
    if (!/^0x[0-9a-f]{64}$/i.test(hash)) throw Error('Enter a burn transaction hash');
    await connect(source.id);
    const r = await receipt(window.ethereum, hash, source.id, 1);
    const original = sourceMessage(v, r.logs, source);
    const wordAddress = offset => ('0x' + original.slice(2 + offset * 2 + 24, 2 + (offset + 32) * 2)).toLowerCase();
    if (original.length !== 754 || wordAddress(44) !== source.messenger.toLowerCase() ||
        wordAddress(76) !== source.messenger.toLowerCase() || wordAddress(152) !== source.token.toLowerCase())
      throw Error('This receipt is not a supported native USDC burn');
    say('Checking Circle attestation…');
    const response = await fetch(`${source.iris}/v2/messages/${source.domain}?transactionHash=${hash}`, { signal: AbortSignal.timeout(15000) });
    if (response.status === 404) throw Error('Attestation not ready. Check again later; do not burn again.');
    if (!response.ok) throw Error(`Circle API unavailable (${response.status}). Your burn hash can be retried later.`);
    const data = await response.json();
    const m = data.messages?.find(m => m.status === 'complete' && sameTransfer(original, m.message));
    if (!m || !/^0x[0-9a-f]+$/i.test(m.attestation)) throw Error('Attestation not ready or does not match this burn. Check again later.');
    const domain = Number(BigInt('0x' + original.slice(18, 26)));
    const dest = destinations(source).find(r => r.domain === domain);
    if (!dest) throw Error('Destination is not supported by this UI');
    const from = await connect(dest.id);
    const tx = { chainId: dest.id, to: dest.transmitter, value: '0x0', data: v.encodeFunctionData({ abi: v.parseAbi(RECEIVE), functionName: 'receiveMessage', args: [m.message, m.attestation] }) };
    const gas = await simulate(tx, from);
    const recipient = v.getAddress('0x' + original.slice(2 + 184 * 2 + 24, 2 + 216 * 2));
    const units = BigInt('0x' + original.slice(2 + 216 * 2, 2 + 248 * 2));
    showPlan(tx, 'mint', from, `Complete CCTP on ${dest.name}\nRecipient: ${recipient}\nBurned: ${v.formatUnits(units, 6)} USDC (less CCTP fee)\nSource transaction: ${hash}\n${gas}`);
    say('Attestation ready. Confirm destination mint in your wallet.');
  });
  window.ethereum?.on?.('accountsChanged', () => { if (!busy && !pending) clear(); });
  window.ethereum?.on?.('chainChanged', () => { if (!busy && !pending) clear(); });
  return { open(holding, owner) {
    root.querySelector('details').open = true; root.scrollIntoView({ behavior: 'smooth', block: 'center' });
    if (busy || pending) { say('Finish checking the current transaction first.'); return; }
    selected = { ...holding }; account = owner; clear();
    $('fields').disabled = $('review').disabled = false;
    $('asset').textContent = `${holding.symbol || 'Token'} · ${holding.chainName || holding.chainId} · ${holding.address}`;
    $('recipient').value = owner; $('amount').value = ''; $('fee').value = '0';
    $('destination').replaceChildren(new Option('Same network', ''));
    const source = routeFor(holding.chainId, holding.address);
    if (source) for (const r of destinations(source)) $('destination').add(new Option(`${r.name} · CCTP USDC`, r.id));
    $('fee-wrap').hidden = $('bridge-note').hidden = true;
    say(source ? 'Send directly or select a CCTP destination.' : 'ERC-20 transfer. CCTP is available only for supported native USDC contracts.');
  } };
}
