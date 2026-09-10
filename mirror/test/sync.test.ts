/**
 * `syncLatest`, the function a publisher actually runs on a timer.
 *
 * Everything it composes is tested elsewhere; what is tested here is the wiring —
 * that the epoch id it stores is the on-chain one, that it finds the table through
 * the uri on the chain, that a second run does nothing, and that it does not write
 * rows for a document the registry does not vouch for.
 *
 * No network: the `eth_call` answers from the ABI blobs go-ethereum packed, and the
 * fetch is injected.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it, vi } from "vitest";

import { syncLatest } from "../src/ingest.js";
import { normalizeHex, type Hex } from "../src/merkle.js";
import { encodeGetEpoch, encodeLatestFinalizedEpoch, type EthCall } from "../src/registry.js";
import { rootsOf, type Coverage, type Leaf } from "../src/snapshot.js";
import { MemoryStore } from "../src/store.js";
import { verifyAsOf } from "../src/verify.js";

interface RegistryFixture {
  registry: Hex;
  latest_finalized_epoch: {
    chain_id: string;
    returndata: Hex;
    epoch_id: string;
    epoch: { root: Hex; coverage_root: Hex; uri: string };
    not_found: { returndata: Hex };
  };
  get_epoch: { returndata: Hex };
}

const fixture = JSON.parse(
  readFileSync(fileURLToPath(new URL("../testdata/registry.json", import.meta.url)), "utf8"),
) as RegistryFixture;

const REGISTRY = fixture.registry;
const CHAIN = BigInt(fixture.latest_finalized_epoch.chain_id);
const EPOCH = BigInt(fixture.latest_finalized_epoch.epoch_id);
const URI = fixture.latest_finalized_epoch.epoch.uri;

const account = (n: number): Hex => `0x${n.toString(16).padStart(40, "0")}`;
const asset = (n: number): Hex => `0x${n.toString(16).padStart(40, "0")}`;

/**
 * The fixture's epoch commits roots over data nobody has: it was packed with
 * `0xabab…` as the root. So the fixture is used for everything except the roots,
 * and the two root words are rewritten to match a table this test does have.
 */
const LEAVES: Leaf[] = [
  { account: account(1), assets: [asset(0xa1)] },
  { account: account(2), assets: [asset(0xa1), asset(0xb2)] },
  { account: account(3), assets: [asset(0xb2)] },
];
const COVERAGE: Coverage[] = [
  { asset: asset(0xa1), fromBlock: 100n, toBlock: 900n },
  { asset: asset(0xb2), fromBlock: 150n, toBlock: 900n },
];
const ROOTS = rootsOf(CHAIN, COVERAGE, LEAVES);

function document(leaves: Leaf[] = LEAVES): string {
  const roots = rootsOf(CHAIN, COVERAGE, leaves);
  const lines = [
    JSON.stringify({
      format: "evmscan-snapshot/1",
      chain_id: Number(CHAIN),
      from_block: 1,
      to_block: 900,
      root: roots.root,
      coverage_root: roots.coverageRoot,
      leaf_count: leaves.length,
      asset_count: COVERAGE.length,
    }),
    ...COVERAGE.map((c) =>
      JSON.stringify({
        t: "coverage",
        asset: c.asset,
        from_block: Number(c.fromBlock),
        to_block: Number(c.toBlock),
      }),
    ),
    ...leaves.map((l) => JSON.stringify({ t: "leaf", account: l.account, assets: l.assets })),
  ];
  return lines.join("\n");
}

/**
 * Replaces the two root words inside a packed Epoch tuple.
 *
 * `offsetAt` is the word holding the offset to the tuple: word 2 for
 * `latestFinalizedEpoch`'s three-value return, word 0 for `getEpoch`'s bare tuple.
 * `root` and `coverageRoot` are components 3 and 4 of the tuple's head.
 */
function withRoots(returndata: Hex, offsetAt: number, root: Hex, coverageRoot: Hex): Hex {
  const words = normalizeHex(returndata).slice(2).match(/.{64}/g)!;
  const base = Number(BigInt(`0x${words[offsetAt]!}`) / 32n);
  words[base + 3] = normalizeHex(root, 32).slice(2);
  words[base + 4] = normalizeHex(coverageRoot, 32).slice(2);
  return `0x${words.join("")}`;
}

const latestReturn = withRoots(
  fixture.latest_finalized_epoch.returndata,
  2,
  ROOTS.root,
  ROOTS.coverageRoot,
);
const getReturn = withRoots(fixture.get_epoch.returndata, 0, ROOTS.root, ROOTS.coverageRoot);

/** An eth_call over the fixture blobs. Records what it was asked. */
function chain(overrides: { latest?: Hex } = {}): { call: EthCall; calls: Hex[] } {
  const calls: Hex[] = [];
  const call: EthCall = async (to, data) => {
    expect(to).toBe(normalizeHex(REGISTRY, 20));
    calls.push(data);
    if (data === encodeLatestFinalizedEpoch(CHAIN)) return overrides.latest ?? latestReturn;
    if (data === encodeGetEpoch(EPOCH)) return getReturn;
    throw new Error(`unexpected calldata ${data}`);
  };
  return { call, calls };
}

describe("syncLatest", () => {
  it("reads the chain, fetches the table it names, and stores the epoch", async () => {
    const store = new MemoryStore();
    const { call, calls } = chain();
    const fetch = vi.fn(async (uri: string) => {
      expect(uri).toBe(URI); // the uri came off the chain, not from configuration
      return document();
    });

    const result = await syncLatest(store, { registry: REGISTRY, chainId: CHAIN, call, fetch });

    expect(result.wrote).toBe(true);
    expect(result.epochId).toBe(EPOCH);
    expect(result.leaves).toBe(3);
    expect(fetch).toHaveBeenCalledOnce();
    expect(calls).toEqual([encodeLatestFinalizedEpoch(CHAIN), encodeGetEpoch(EPOCH)]);

    // And the mirror it left behind verifies against the commitment it just read.
    const verified = verifyAsOf(await store.rows(), await store.coverage(EPOCH), result.commitment);
    expect(verified.ok, verified.reason).toBe(true);
  });

  it("keys rows by the on-chain epoch id, which is what a wallet looks up", async () => {
    const store = new MemoryStore();
    const { call } = chain();
    await syncLatest(store, {
      registry: REGISTRY,
      chainId: CHAIN,
      call,
      fetch: async () => document(),
    });

    expect(await store.ingestedEpochs()).toEqual([EPOCH]);
    expect((await store.rows()).every((r) => r.sinceEpoch === EPOCH)).toBe(true);
  });

  it("does nothing, and fetches nothing, when the epoch is already in", async () => {
    const store = new MemoryStore();
    const { call } = chain();
    const fetch = vi.fn(async () => document());

    await syncLatest(store, { registry: REGISTRY, chainId: CHAIN, call, fetch });
    const again = await syncLatest(store, { registry: REGISTRY, chainId: CHAIN, call, fetch });

    expect(again.wrote).toBe(false);
    expect(again.epochId).toBe(EPOCH);
    expect(fetch).toHaveBeenCalledOnce(); // not twice: no table was downloaded
  });

  it("takes an explicit uri for an epoch published before uri existed", async () => {
    const store = new MemoryStore();
    const { call, calls } = chain();
    await syncLatest(store, {
      registry: REGISTRY,
      chainId: CHAIN,
      call,
      fetch: async (uri) => {
        expect(uri).toBe("https://mirror.example/snapshot");
        return document();
      },
      snapshotUri: "https://mirror.example/snapshot",
    });

    // getEpoch was never called: the caller said where to look.
    expect(calls).toEqual([encodeLatestFinalizedEpoch(CHAIN)]);
  });

  it("writes nothing when the served table is not what was committed", async () => {
    const store = new MemoryStore();
    const { call } = chain();
    const tampered = document([
      ...LEAVES.slice(0, 2),
      { account: account(3), assets: [asset(0xa1), asset(0xb2)] }, // an extra asset
    ]);

    await expect(
      syncLatest(store, { registry: REGISTRY, chainId: CHAIN, call, fetch: async () => tampered }),
    ).rejects.toThrow(/does not match epoch/);

    expect(await store.rows()).toEqual([]);
    expect(await store.ingestedEpochs()).toEqual([]);
  });

  it("says there is nothing to sync rather than storing a zero root", async () => {
    const store = new MemoryStore();
    const { call } = chain({ latest: fixture.latest_finalized_epoch.not_found.returndata });
    await expect(
      syncLatest(store, {
        registry: REGISTRY,
        chainId: CHAIN,
        call,
        fetch: async () => document(),
      }),
    ).rejects.toThrow(/no finalized epoch/);
    expect(await store.rows()).toEqual([]);
  });
});
