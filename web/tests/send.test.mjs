import { test } from 'node:test';
import assert from 'node:assert/strict';
import * as v from 'viem';
import { amountUnits, address, build, ROUTES, destinations, routeFor, BURN, checkWallet, receipt, sameTransfer, sourceMessage } from '../send.mjs';
const recipient = '0x1111111111111111111111111111111111111111';
const source = ROUTES[0];
const input = { chainId: 1, token: source.token, recipient, amount: '1.000001', decimals: 6 };
test('strict precision: never round money or accept scientific notation', () => {
  assert.equal(amountUnits('1.000001', 6), 1000001n);
  for (const n of ['1.0000001', '-1', '1e6', 'NaN', '0', '01', '.5']) assert.throws(() => amountUnits(n, 6));
  assert.throws(() => amountUnits((2n ** 256n).toString(), 0));
  assert.equal(amountUnits('0', 6, true), 0n);
  assert.throws(() => address(v, '0x' + '00'.repeat(20)));
});
test('transfer encodes recipient and integer amount, zero native value', () => {
  const tx = build(v, input);
  assert.equal(tx.to, source.token);
  assert.equal(tx.value, '0x0');
  assert.equal(tx.data.slice(0, 10), '0xa9059cbb');
  assert.deepEqual(v.decodeFunctionData({ abi: v.erc20Abi, data: tx.data }).args, [recipient, 1000001n]);
});
test('CCTP uses domains, padded recipient, explicit fee and finalized threshold', () => {
  const tx = build(v, { ...input, destination: 8453, maxFee: '0.001' });
  assert.equal(tx.to, source.messenger);
  assert.deepEqual(v.decodeFunctionData({ abi: v.parseAbi(BURN), data: tx.data }).args,
    [1000001n, 6, v.pad(recipient), source.token, v.zeroHash, 1000n, 2000]);
  for (const changes of [{ destination: 1 }, { destination: 84532 }, { destination: 137 }, { destination: 8453, token: recipient }, { destination: 8453, maxFee: '2' }]) assert.throws(() => build(v, { ...input, ...changes }));
  assert.equal(routeFor(1, recipient), undefined);
  assert.equal(destinations(ROUTES[4]).length, 1);
});
test('wallet identity and chain checked before sending', async () => {
  const provider = { request: async ({ method }) => method === 'eth_accounts' ? [recipient] : '0x1' };
  await checkWallet(provider, recipient, 1);
  await assert.rejects(checkWallet(provider, source.token, 1), /account changed/);
  await assert.rejects(checkWallet(provider, recipient, 8453), /network changed/);
});
test('pending, reverted and wrong-network receipts are never success', async () => {
  const provider = status => ({ request: async ({ method }) => method === 'eth_chainId' ? '0x1' : status === null ? null : { status } });
  await receipt(provider('0x1'), v.zeroHash, 1, 1);
  await assert.rejects(receipt(provider('0x0'), v.zeroHash, 1, 1), { reverted: true });
  await assert.rejects(receipt(provider(null), v.zeroHash, 1, 1, async () => {}), /Still pending/);
  await assert.rejects(receipt(provider('0x1'), v.zeroHash, 10, 1), /Switch back/);
});
test('Iris may assign nonce, finality and fee, but may not change recipient, amount, route or fee cap', () => {
  const original = '0x' + '00'.repeat(376);
  const change = offset => original.slice(0, 2 + offset * 2) + 'ff' + original.slice(4 + offset * 2);
  for (const offset of [12, 144, 312, 344]) assert.equal(sameTransfer(original, change(offset)), true);
  for (const offset of [4, 8, 44, 184, 216, 248, 280]) assert.equal(sameTransfer(original, change(offset)), false);
  assert.equal(sameTransfer(original, '0x'), false);
  const logs = [{ address: source.transmitter, topics: [v.keccak256(v.toHex('MessageSent(bytes)'))], data: v.encodeAbiParameters([{ type: 'bytes' }], [original]) }];
  assert.equal(sourceMessage(v, logs, source), original);
  assert.throws(() => sourceMessage(v, [{ ...logs[0], address: recipient }], source));
  assert.throws(() => sourceMessage(v, [...logs, ...logs], source));
});
