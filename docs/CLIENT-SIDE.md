# Moving work to the client

Three kinds of work could leave the daemon, and they are not equally safe. The
dividing line is whether a client's answer ends up in the database — and therefore
inside a merkle root the publisher has bonded.

## Tier 1 — reads: done

`AssetLens` and `PriceLens` are deployless. `eth_call` with no `to` executes creation
code and returns whatever the constructor returns, so the entire query is:

```
eth_call({ data: creation || abi.encode(request) }, "latest")
```

Nothing in that is privileged. The computation happens inside the EVM, not in the
daemon, so a browser holding the bytecode makes the identical call against whatever
RPC its reader trusts and gets the identical answer.

`GET /v1/lens` serves the calling convention — creation code, the request parameters
to encode, the reply shape to decode, and the two consensus ceilings to batch under
(EIP-170's 24,576-byte reply, EIP-3860's 49,152-byte payload). The shapes come from
the compiled artifacts rather than being transcribed, because a hand-copied ABI is
the classic way for a client and a server to drift apart in silence.

The UI uses it when a reader names their own RPC, and falls back to the daemon
otherwise. Measured from a browser against a public endpoint: **215 ms** for a
three-token read, against roughly 33 s for a cold price read through the daemon's
light client.

That gap is the argument. A lens call through Helios makes it re-verify every
storage slot the lens touches with `eth_getProof`, so one balance read fans out into
many billed upstream requests. Moving it to the reader's own endpoint removes the
spikiest part of the bill, and the answer is no less trustworthy — it is their node,
and the bytecode came from an endpoint they can diff against this repo.

**Why this is safe:** nothing is written. A reader who supplies a lying RPC lies
only to themselves.

Names take the same road. A typed `vitalik.eth` becomes an address in the browser:
one `eth_call` to ENS's Universal Resolver — the same contract at the same address on
mainnet and on Sepolia's ENSv2 — on the RPC the reader named for the lens, else a
public one for the active chain. The daemon has no endpoint that takes a name and never
sees one; the mirror does the same through its injected `eth_call` (`resolveName` in
`mirror/src/names.ts`). Only a name behind a CCIP-Read gateway falls back to the mainnet
library, and the result line says so. Reverse records follow the same path, and stand in
for an address only once the forward record agrees.

## Tier 2 — hints: mostly already here

A client can say *what to look at* without saying *what is true*. That is what
`requestIndexing` and candidate promotion already are: the registrant supplies an
address, and the daemon decides what to believe by reading logs itself.

The saving available here is real but indirect. Discovery is the standing cost — an
unfiltered sweep across every block at the head, forever — and a filtered
`eth_getLogs` for one named address is far cheaper. Enough client hints would let
discovery's cadence come down. Nothing about the trust model changes, because the
daemon still does the reading.

## Tier 3 — client-computed index rows: not without proofs

This is the one that would genuinely offload indexing, and it is currently
forbidden. From CLAUDE.md:

> The `eth_subscribe` stream is a wake-up signal only; all logs enter the index
> through `eth_getLogs`. Do not add a second ingestion path.

That invariant is not stylistic. `interactions` is folded into a merkle root, the
root is published with a bond, and the challenge window makes the publisher liable
for it. A client that can write rows can poison the root — and it is the publisher's
bond that is slashed, not theirs. Re-reading the logs to check a submission costs
exactly the RPC spend the submission was meant to save.

### What would make it work

A submission carries its own evidence, and the daemon checks the evidence instead of
the claim:

1. The client submits the logs it decoded, plus the **block header** they came from
   and a **receipts-root Merkle proof** for each receipt containing them.
2. The daemon hashes the receipt into the proof and checks the result equals the
   header's `receiptsRoot`. That is a hash check, not a round trip.
3. The daemon checks the header is canonical. Inside Helios's verified window this
   is nearly free — it already tracks the beacon-verified header chain.

Cost per submission drops from an `eth_getLogs` to some keccak. That is the whole
prize.

### What it does not solve

- **Beyond the window.** Helios verifies roughly 8,191 blocks (EIP-2935's ring
  buffer, about 27 hours on mainnet). Older headers cannot be checked cheaply, so
  client-supplied history past that needs a different anchor — a checkpoint the
  operator already trusts, or an accumulator committed on chain.
- **Omission.** A proof shows a log *happened*; nothing shows a client didn't leave
  logs out. A submitter who reports nine of ten transfers produces a root that is
  wrong and fully proven. Coverage has to come from elsewhere: the daemon sampling
  ranges itself, or several independent submitters being compared, or funding tied
  to a range someone will challenge.
- **Who pays for a bad root.** Even with proofs, the publisher signs the commitment.
  Either submitters post their own bond, or the publisher accepts the liability
  knowingly.

Omission is the hard one, and it is a design problem rather than an implementation
one. Proof-carrying submissions without an answer to it buy less than they appear
to: they make lying about a log impossible and staying quiet about one free.

### Recommendation

Do tier 1 (done) and lean on tier 2 before attempting tier 3. If tier 3 goes ahead,
scope it first to the verified window and to assets whose funding gives someone a
motive to challenge — that keeps the omission problem bounded to a range somebody is
already paying to have watched.
