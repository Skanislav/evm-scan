// Injected-wallet integration. No RPC or real transactions.
import { chromium } from '@playwright/test';
import { build } from 'esbuild';
import assert from 'node:assert/strict';
const bundle = await build({ stdin: { contents: `import * as v from 'viem'; import {mount, ROUTES} from '../send.mjs'; window.setup = () => {
const owner = '0x1111111111111111111111111111111111111111';
window.sent=[]; window.reject=false; window.confirmed=false; window.chain='0x1'; window.allowance=0n;
const word = x=>v.pad(x,{size:32});
const message = v.concat(['0x000000010000000000000006', v.zeroHash, word(ROUTES[0].messenger), word(ROUTES[0].messenger), v.zeroHash, '0x000007d000000000', '0x00000001', word(ROUTES[0].token), word(owner), v.toHex(1000000n,{size:32}), word(owner), v.zeroHash, v.zeroHash, v.zeroHash]);
window.fetch=async()=>({ok:true,json:async()=>({messages:[{status:'complete',message,attestation:'0x1234'}]})});
window.ethereum={on(){}, async request({method,params}) {
 if(method==='eth_requestAccounts'||method==='eth_accounts')return [owner];
 if(method==='eth_chainId')return window.chain;
 if(method==='wallet_switchEthereumChain'){window.chain=params[0].chainId;return null;}
 if(method==='eth_call') {
  if(params[0].data.startsWith('0xdd62ed3e')) return v.encodeAbiParameters([{type:'uint256'}],[window.allowance]);
  if(params[0].data.startsWith('0x313ce567')) return v.encodeAbiParameters([{type:'uint8'}],[6]);
  if(params[0].data.startsWith('0x70a08231')) return v.encodeAbiParameters([{type:'uint256'}],[100000000n]);
  return v.encodeAbiParameters([{type:'bool'}],[true]);
 }
 if(method==='eth_estimateGas')return '0xc350';
 if(method==='eth_gasPrice')return '0x1';
 if(method==='eth_sendTransaction'){if(window.reject)throw {code:4001}; window.sent.push(params[0]); if(params[0].data.startsWith('0x095ea7b3'))window.allowance=1000000n; return v.zeroHash;}
 if(method==='eth_getTransactionReceipt')return window.confirmed?{status:'0x1',logs:[{address:ROUTES[0].transmitter,topics:[v.keccak256(v.toHex('MessageSent(bytes)'))],data:v.encodeAbiParameters([{type:'bytes'}],[message])}]}:null;
 throw Error(method);
}};
const ui=mount({root:document.querySelector('#root'),viem:async()=>v,resolveAccount:async address=>({address}),currentAccount:()=>owner});
ui.open({address:ROUTES[0].token,chainId:1,chainName:'Ethereum',decimals:6,symbol:'USDC',balance:'100000000',value_usd:'100'},owner);
};`, resolveDir: new URL('.', import.meta.url).pathname }, bundle: true, write: false, format: 'iife', platform: 'browser' });
const browser = await chromium.launch({headless:true, ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE ? {executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE} : {})});
try {
 const page = await browser.newPage();
 await page.setContent('<div id="root"></div>');
 await page.addScriptTag({content:bundle.outputFiles[0].text});
 await page.evaluate(()=>window.setup());
 const field = name=>page.locator(`[data-send="${name}"]`);
 await field('amount').fill('1.0000001'); await field('review').click();
 await field('status').filter({hasText:'at most 6'}).waitFor();
 assert.equal(await field('execute').isHidden(),true);
 await field('amount').fill('1'); await field('review').click();
 await field('execute').waitFor({state:'visible'});
 assert.match(await field('preview').textContent(), /0xa9059cbb/);
 await page.evaluate(()=>window.reject=true); await field('execute').click();
 await field('status').filter({hasText:'Cancelled'}).waitFor();
 assert.equal(await field('execute').isEnabled(),true);
 await page.evaluate(()=>window.reject=false); await field('execute').click();
 await field('status').filter({hasText:'Submitted'}).waitFor();
 assert.equal(await field('execute').isDisabled(),true);
 assert.equal(await field('amount').isDisabled(),true);
 assert.equal(await page.evaluate(()=>window.sent.length),1);
 await page.evaluate(()=>window.confirmed=true);
 await field('status').filter({hasText:'Transfer confirmed'}).waitFor();
 assert.equal(await field('execute').isHidden(),true);
 await field('destination').selectOption('8453');
 await field('review').click();
 await field('execute').filter({hasText:'Approve exact'}).waitFor();
 await field('execute').click();
 await field('status').filter({hasText:'Approval confirmed'}).waitFor();
 await field('review').click();
 await field('execute').filter({hasText:'Burn USDC'}).waitFor();
 await field('execute').click();
 await field('status').filter({hasText:'Burn confirmed'}).waitFor();
 await page.locator('summary').filter({hasText:'Resume CCTP from'}).click();
 await field('resume').click();
 await field('execute').filter({hasText:'Mint USDC'}).waitFor();
 assert.match(await field('preview').textContent(),/Complete CCTP on Base/);
 await field('execute').click();
 await field('status').filter({hasText:'USDC minted'}).waitFor();
 assert.equal(await page.evaluate(()=>window.sent.length),4);
 assert.equal(await page.evaluate(()=>window.sent[3].chainId),'0x2105');
 console.log('CCTP browser checks passed: exact approval, burn, resume, attestation, network switch, mint.');
 console.log('Send browser checks passed: precision, preview, rejection, pending lock, confirmation.');
} finally {await browser.close();}
