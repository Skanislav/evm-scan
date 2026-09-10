/**
 * The registry decoder, against blobs go-ethereum packed from the compiled ABI.
 *
 * Hand-rolled ABI decoding is only defensible if it is pinned to encodings the
 * contract actually produces, which is what ../testdata/registry.json is. The
 * Epoch struct is a dynamic tuple, so the failure mode being guarded against is not
 * an exception — it is a wrong root read from the right-looking place.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

import { normalizeHex, type Hex } from "../src/merkle.js";
import {
  decodeGetEpoch,
  decodeLatestFinalizedEpoch,
  encodeGetEpoch,
  encodeLatestFinalizedEpoch,
  EpochStatus,
  NoFinalizedEpochError,
  readCommitment,
  readSnapshotUri,
  selector,
  type EthCall,
} from "../src/registry.js";

interface EpochView {
  chain_id: string;
  from_block: string;
  to_block: string;
  root: Hex;
  coverage_root: Hex;
  uri: string;
  publisher: Hex;
  challenger: Hex;
  bond: string;
  published_at: string;
  challenge_deadline: string;
  status: number;
  assertion_id: Hex;
}

interface RegistryFixture {
  registry: Hex;
  latest_finalized_epoch: {
    chain_id: string;
    calldata: Hex;
    returndata: Hex;
    found: boolean;
    epoch_id: string;
    epoch: EpochView;
    not_found: { why: string; returndata: Hex; found: boolean };
  };
  get_epoch: { epoch_id: string; calldata: Hex; returndata: Hex; epoch: EpochView };
}

const fixture = JSON.parse(
  readFileSync(fileURLToPath(new URL("../testdata/registry.json", import.meta.url)), "utf8"),
) as RegistryFixture;

const latest = fixture.latest_finalized_epoch;
const chainId = BigInt(latest.chain_id);

/** An eth_call that answers from the fixture, keyed by the calldata it is given. */
const fakeCall: EthCall = async (to, data) => {
  expect(to).toBe(normalizeHex(fixture.registry, 20));
  if (data === normalizeHex(latest.calldata)) return latest.returndata;
  if (data === normalizeHex(fixture.get_epoch.calldata)) return fixture.get_epoch.returndata;
  throw new Error(`unexpected calldata ${data}`);
};

describe("call encoding", () => {
  it("derives the selector from the signature", () => {
    // 0x6c52b092 is what go-ethereum put in front of the fixture's calldata.
    expect(selector("latestFinalizedEpoch(uint64)")).toBe(latest.calldata.slice(0, 10));
    expect(selector("getEpoch(uint256)")).toBe(fixture.get_epoch.calldata.slice(0, 10));
  });

  it("encodes latestFinalizedEpoch exactly as the ABI does", () => {
    expect(encodeLatestFinalizedEpoch(chainId)).toBe(normalizeHex(latest.calldata));
  });

  it("encodes getEpoch exactly as the ABI does", () => {
    expect(encodeGetEpoch(BigInt(fixture.get_epoch.epoch_id))).toBe(
      normalizeHex(fixture.get_epoch.calldata),
    );
  });
});

describe("return decoding", () => {
  it("reads every field of the Epoch tuple", () => {
    const { found, epochId, epoch } = decodeLatestFinalizedEpoch(latest.returndata);
    expect(found).toBe(true);
    expect(epochId).toBe(BigInt(latest.epoch_id));

    const want = latest.epoch;
    expect(epoch).not.toBeNull();
    expect(epoch!.chainId).toBe(BigInt(want.chain_id));
    expect(epoch!.fromBlock).toBe(BigInt(want.from_block));
    expect(epoch!.toBlock).toBe(BigInt(want.to_block));
    expect(epoch!.root).toBe(normalizeHex(want.root, 32));
    expect(epoch!.coverageRoot).toBe(normalizeHex(want.coverage_root, 32));
    expect(epoch!.publisher).toBe(normalizeHex(want.publisher, 20));
    expect(epoch!.challenger).toBe(normalizeHex(want.challenger, 20));
    expect(epoch!.bond).toBe(BigInt(want.bond));
    expect(epoch!.publishedAt).toBe(BigInt(want.published_at));
    expect(epoch!.challengeDeadline).toBe(BigInt(want.challenge_deadline));
    expect(epoch!.status).toBe(want.status);
    expect(epoch!.assertionId).toBe(normalizeHex(want.assertion_id, 32));
  });

  it("reads the uri through its offset, not from a fixed position", () => {
    // The uri is why the tuple is dynamic. A decoder that mistook the tuple's own
    // offset base for the start of the returndata reads garbage here, or throws.
    const { epoch } = decodeLatestFinalizedEpoch(latest.returndata);
    expect(epoch!.uri).toBe(latest.epoch.uri);
    expect(epoch!.uri).toMatch(/^https:\/\//);
    expect(decodeGetEpoch(fixture.get_epoch.returndata).uri).toBe(fixture.get_epoch.epoch.uri);
  });

  it("decodes the same struct from getEpoch, whose offsets differ", () => {
    // getEpoch returns the tuple alone, so every offset inside it sits at a
    // different absolute position than in latestFinalizedEpoch's three-value return.
    const fromGet = decodeGetEpoch(fixture.get_epoch.returndata);
    const fromLatest = decodeLatestFinalizedEpoch(latest.returndata).epoch!;
    expect(fromGet).toEqual(fromLatest);
  });

  it("does not read a root out of a not-found answer", () => {
    const { found, epoch } = decodeLatestFinalizedEpoch(latest.not_found.returndata);
    expect(found).toBe(false);
    expect(epoch).toBeNull();
  });

  it("refuses returndata that is not a whole number of words", () => {
    expect(() => decodeLatestFinalizedEpoch("0x1234")).toThrow(/whole number of words/);
  });

  it("refuses returndata that is too short for the struct it points at", () => {
    const truncated = latest.returndata.slice(0, 2 + 64 * 6);
    expect(() => decodeLatestFinalizedEpoch(truncated)).toThrow(/words/);
  });
});

describe("readCommitment", () => {
  it("returns what a mirror is checked against", async () => {
    const commitment = await readCommitment(fakeCall, fixture.registry, chainId);
    expect(commitment.epochId).toBe(BigInt(latest.epoch_id));
    expect(commitment.chainId).toBe(chainId);
    expect(commitment.root).toBe(normalizeHex(latest.epoch.root, 32));
    expect(commitment.coverageRoot).toBe(normalizeHex(latest.epoch.coverage_root, 32));
  });

  it("insists the epoch is finalized, not merely proposed", async () => {
    expect(latest.epoch.status).toBe(EpochStatus.Finalized);
    const proposed: EthCall = async () => flipStatus(latest.returndata, EpochStatus.Proposed);
    await expect(readCommitment(proposed, fixture.registry, chainId)).rejects.toThrow(
      /not finalized/,
    );
  });

  it("refuses an epoch about a different chain than the one asked for", async () => {
    await expect(readCommitment(fakeCall, fixture.registry, chainId + 1n)).rejects.toThrow(
      /unexpected calldata/, // the fake only answers the chain in the fixture
    );
    const wrongChain: EthCall = async () => latest.returndata;
    await expect(readCommitment(wrongChain, fixture.registry, 1n)).rejects.toThrow(
      /got an epoch about chain 8453/,
    );
  });

  it("says there is nothing to verify against rather than returning a zero root", async () => {
    const empty: EthCall = async () => latest.not_found.returndata;
    await expect(readCommitment(empty, fixture.registry, chainId)).rejects.toThrow(
      NoFinalizedEpochError,
    );
  });

  it("finds the snapshot uri a bootstrap needs", async () => {
    const uri = await readSnapshotUri(fakeCall, fixture.registry, BigInt(fixture.get_epoch.epoch_id));
    expect(uri).toBe(fixture.get_epoch.epoch.uri);
  });
});

/** Rewrites the status word of a packed Epoch, to make a proposed-epoch answer. */
function flipStatus(returndata: Hex, status: EpochStatus): Hex {
  const words = normalizeHex(returndata).slice(2).match(/.{64}/g)!;
  // word 2 is the offset to the tuple (0x60 = word 3); status is its 12th component.
  const base = Number(BigInt(`0x${words[2]!}`) / 32n);
  words[base + 11] = status.toString(16).padStart(64, "0");
  return `0x${words.join("")}`;
}
