# Tokenomics: paying for indexing without a sponsor

Status: **design v0, minimum built.** Decided 2026-09-05: unit of account is ETH, and
only the minimum ships for the hackathon. What is in `contracts/src/HintRegistry.sol`
and the daemon today:

- per-asset funding (§3, without the bounty/maintenance split or period buckets: one
  balance per asset, paid at `rewardPerBlock` per newly covered block);
- coverage roots on epochs and per-asset claims after finalization (§4.2);
- the gas floor on the daemon side: the registry quotes the coverage, the publisher
  refuses to post below `min_expected_reward_wei` (§6.1's intent, without on-chain
  reimbursement).

Everything else here, staking, attestations, slashing, fraud proofs, the paymaster, a
token, is design only. §10 says what order it would land in.

## 0. What we are designing for

Four parties, one product:

| Party | Wants | Pays / earns |
| --- | --- | --- |
| **Requester** (token issuer, dapp, wallet vendor) | its contract discoverable in every wallet, from its deploy block, kept current | pays |
| **Publisher** (runs a node + `evmscand`) | to be paid for history it scanned and for keeping the head indexed, without locking capital or fronting unreimbursed gas | earns |
| **Challenger** (any node operator, usually another publisher) | to be paid for catching a wrong root | earns on success, pays on failure |
| **Consumer** (wallet, user) | `account → contracts`, verifiable, fast | free for now; see §7 |

Two constraints carried over from the README, both load-bearing:

- **Output is deterministic.** Given a chain, a block range and a fixed asset set, every
  honest node computes the same `account → assets` table and therefore the same root.
  This is the property the whole design leans on: agreement is cheap to check on-chain,
  so we reward agreement and only fall back to adjudication on disagreement.
- **Nothing here is a source of truth.** A root is a hint. That is why optimistic
  finality with a bonded challenge is enough, and why we never need the registry to
  hold the data itself.

## 1. What is wrong with v1

The contract on this branch already has a reward pool, and it works end-to-end on a dev
chain. It has three problems that any serious deployment would hit.

1. **The pool is drainable.** `publishIndex` accepts any root from anyone. After the
   window, `finalizeIndex` pays `publisherReward` from the pool, no questions asked.
   With `publisherBond = 0` (the Railway config) it costs an attacker one transaction per
   reward. With a non-zero bond it still costs nothing but gas, because the bond comes
   back unless the arbiter is paged. Rewards are not tied to work done.
2. **Reward is per epoch, not per work.** A flat `publisherReward` rewards posting many
   small epochs and pays the same for a one-asset chain as for a thousand-asset chain.
   The requester's money goes into one chain-wide bucket, so paying for asset X does not
   make X any more likely to be indexed.
3. **Gas is fronted with no floor.** The publisher pays `publishIndex` and
   `finalizeIndex` gas up front and is reimbursed a fixed amount later. Whether that
   covers gas depends on the base fee that day, and the publisher needs seed capital and a
   funded EOA to start at all.

The rest of the document fixes these in order. Everything remains ETH-denominated; §8
is about whether a native token earns its place.

## 2. Unit of account: ETH, with a hook for a token

Recommendation: **no native token in this phase.** Requesters want to pay in the thing
they already hold, publishers want to be paid in the thing that pays their gas, and a
token whose only job is "be the fee" adds sell pressure and a speculative surface without
adding a mechanism.

The contract holds balances through a single `paymentToken` seam: `address(0)` means
ETH, anything else is an ERC-20. Every `payable` becomes a pull from that token. This
keeps the door open for §8 without touching the mechanism.

## 3. Requesters: per-asset funding instead of a chain pool

`requestIndexing(chainId, token, kind, fromBlock)` keeps its signature. The payment is
split three ways and **stays attached to the asset**:

```
payment = assetBond            refundable on revoke, prices spam
        + backfillBounty       one-shot: pays whoever first covers [fromBlock, head]
        + maintenance          streamed: pays per block of head coverage until exhausted
```

- **`assetBond`** is unchanged. It is still a spam price, not a legitimacy signal.
- **`backfillBounty`** is claimed once, by the publisher of the first finalized epoch whose
  declared coverage for this asset reaches `fromBlock` (or the chain's declared floor,
  see §4). It is the price of the expensive part: the walk back through history.
- **`maintenance`** is a prepaid meter. The requester chooses a `ratePerBlock` (or takes
  the default); each finalized epoch that covers the asset draws
  `ratePerBlock × blocksCovered` until the balance is zero. Anyone can top up. When the
  meter hits zero the asset stays registered and indexers may keep it, but it stops
  paying. That is the "keep my token current" subscription, priced per block so epoch
  count is irrelevant.

Why per asset and not per chain: it makes the requester's payment *purchase* the thing
they asked for. A publisher is paid for covering X exactly when X paid to be covered. It
also closes the drain in §1: a junk epoch that claims coverage of X is stealing from the
honest indexer of X, who has the data to challenge it and the motive to do so.

`fundRewards(chainId)` survives as a chain-level top-up for sponsors who do not care
which asset; it is distributed pro rata across the chain's funded assets at claim time.

### Constant-gas bookkeeping

Charging per asset per epoch naively means a loop over assets in `finalizeIndex`. Avoid
it with **period buckets**: a period is a fixed block range (`periodLength`, e.g. 7200
blocks ≈ a day on mainnet). Funding an asset for `K` periods writes
`maintenance / K` into `bucket[chainId][p]` for the next `K` periods (K is bounded,
e.g. ≤ 52). An epoch covers whole periods and claims their buckets. The backfill bounty
is a separate per-asset slot claimed by proof (§4). All claim paths are O(1) or O(K).

## 4. Publishers: stake once, cover explicitly, share on agreement

### 4.1 Stake replaces the per-epoch bond

`registerPublisher()` locks `publisherStake` once. `publishIndex` no longer needs
`msg.value`. Slashing (§5) draws from the stake; a publisher whose stake falls below the
minimum cannot publish until it tops up. `withdrawStake` has an exit delay of one
challenge window so a slashable epoch cannot be abandoned.

This removes the per-epoch capital lock and, more importantly, gives challengers
something to win.

### 4.2 Epochs declare coverage

An epoch commits two roots:

```
root          merkle over leaves (account, chainId, assetsHash)      unchanged
coverageRoot  merkle over leaves (assetKey, fromBlock, toBlock)
```

Coverage is what makes the output deterministic across nodes with different history
floors: the epoch says which range of each asset it stands behind, and everything under
`root` must be derivable from exactly those ranges. Two publishers with different floors
publish different coverage and therefore different, but both correct, roots. A wallet
prefers deeper coverage; the contract does not have to.

The daemon already tracks per-asset scanned ranges and `history_complete`; the coverage
tree is a projection of `asset_cursors`.

### 4.3 One canonical epoch per period, attestations for the rest

For each `(chainId, period)` there is one **proposal** (first `publishIndex` for that
period) and any number of **attestations** (`attestIndex(epochId)` from other staked
publishers, meaning "my node computed the same root and coverageRoot"). An attestation
is one cheap transaction and no calldata.

On finalization the period's buckets pay out:

```
proposer     50%  + gas reimbursement first (§6)
attesters    50%  split equally, only if ≥ 1 attestation exists
(no attesters: proposer takes 100%)
```

Attestation is what turns "one indexer" into "a network of indexers" without any
coordination layer: everyone indexes, one posts, the rest confirm, all get paid.
Because the output is deterministic, an honest attester never has to trust the
proposer; it compares roots locally and either attests or challenges.

A second proposal for the same period with a *different* root is not a proposal, it is a
challenge (§5). A second proposal with the *same* root is an attestation.

### 4.4 What the daemon does

- Build epochs on period boundaries rather than on a timer.
- Before publishing, read the registry: if a proposal for this period exists and its
  roots match ours, attest; if they differ, challenge; else propose.
- `Submitter` is unchanged.

## 5. Challenges: disagreement is the trigger, stake is the prize

`challengeIndex(epochId, root', coverageRoot')` is callable by any staked publisher
before the deadline. The challenger's stake is at risk equal to the proposer's bond
share.

Resolution is still the `arbiter` in this phase, for the reason the README gives: a real
fraud proof means proving a source-chain log from the registry chain. Two things change:

- **Payout.** The loser is slashed `slashAmount`; half goes to the winner, half is burned
  (or sent to the chain bucket). Attesters on the losing side are slashed a smaller
  `attesterSlash`, so copying a root without computing it is not free.
- **Path to removing the arbiter.** Because coverage is explicit, a challenge is always
  "account A touched asset X in block B within your declared range and your leaf for A
  omits X" or the reverse. That is a single receipt proof against a known block hash.
  On chains that expose source-chain block hashes (L2 → L1 via the L1 block oracle, or
  same-chain via EIP-2935), this is an on-chain fraud proof and the arbiter role becomes
  `address(0)`. Same chain first, cross-chain later.

## 6. Gas: reimburse what was spent, then pay the reward

This is the part that "solves gas sponsoring" without a sponsor.

### 6.1 Reimburse measured gas at finalization

`publishIndex` records `gasUsed × block.basefee` (bounded by `maxGasReimbursement`) on
the epoch. `finalizeIndex` pays that back **first**, from the period's buckets, before
splitting the reward. The publisher's return is therefore `≥ 0` whenever the buckets
cover gas, independent of the base fee that day. Same for `finalizeIndex` itself: the
caller of `finalizeIndex` is rebated its own gas from the same buckets, so a keeper (or a
wallet's cron, or the proposer) will always finalize.

Order of payment from a period's buckets:

```
1. finalize caller gas rebate
2. proposer publish gas reimbursement
3. proposer / attester reward split
```

If the buckets cannot cover step 1 and 2, the shortfall is a loss, and the daemon should
not publish: `evmscand` reads the period's funded balance from the registry and skips
periods that do not pay. That is the market signal: unfunded chains do not get epochs.

### 6.2 Put the registry on an L2

The registry is chain-agnostic already (every record carries `chainId`). Deploying it on
Base or Arbitrum makes `publishIndex` cost cents while the indexed chain can be mainnet.
This is the biggest single lever and needs no code.

### 6.3 Phase 2: the registry as its own paymaster

The remaining problem is bootstrapping: a new publisher needs a funded EOA before its
first reimbursement. The clean solution is ERC-4337 with **the registry itself as the
paymaster**: it holds an EntryPoint deposit funded from the chain buckets, and
`validatePaymasterUserOp` accepts a user operation only if it is a call to
`publishIndex`/`attestIndex`/`finalizeIndex` from a staked publisher. `postOp` charges
the actual gas to the period bucket, which is the same money §6.1 would have paid out.
There is no third party to trust; the paymaster's policy is the contract's own rules.

This slots in behind `hintreg.Submitter` as a second implementation. It is deferred
until §6.1 is in and the reimbursement numbers are observed.

### 6.4 Numbers

Approximate, for sizing `maxGasReimbursement` and default rates:

| Call | Gas | Mainnet @ 10 gwei | Base @ 0.05 gwei |
| --- | --- | --- | --- |
| `publishIndex` (2 roots, uri, event) | ~180k | 0.0018 ETH | ~0.00001 ETH |
| `attestIndex` | ~50k | 0.0005 ETH | negligible |
| `finalizeIndex` + payout | ~90k | 0.0009 ETH | negligible |

With daily periods on an L2 registry, one asset paying `0.001 ETH` per period sustains a
proposer and several attesters with margin. On a mainnet registry a chain needs roughly
`0.005 ETH` per period across its assets before publishing is worth it.

## 7. Consumers and where new money comes from

Reads are free and unauthenticated in this phase. Three revenue paths, in the order they
are likely to matter:

1. **Issuers.** A token team paying `requestIndexing` for its own contract is the
   natural first customer: "be discoverable in every self-hosted wallet" is a listing
   fee they already pay elsewhere. This is what the design is priced for.
2. **Chain-level sponsors** via `fundRewards`: a chain or an ecosystem fund that wants
   its whole chain covered.
3. **Paid reads** (x402 on the API, explicitly deferred): revenue is forwarded to
   `fundRewards` for the chain queried, so a wallet that pays for answers is funding the
   indexers who produced them. The contract needs nothing new for this.

## 8. Should there be a token?

Only if it does a job ETH cannot. The jobs worth considering:

| Job | Does a token help? | Verdict |
| --- | --- | --- |
| Fee / unit of account | No. Adds a swap for every party. | ETH via `paymentToken`. |
| Publisher stake | Marginally: a work token makes the stake's value rise with network usage, which raises the cost of attack as the network matters more. | Later, if stake size becomes a security concern. |
| Bootstrapping subsidy before fee revenue exists | Yes, this is the classic reason. Inflationary rewards for early publishers. | Only with a hard sunset, and only after §3 makes wash-indexing unprofitable: a subsidy paid per epoch is farmable by publishing epochs for chains nobody asked about. Tie any subsidy to *funded* coverage only. |
| Governance of parameters (`periodLength`, slash amounts, arbiter) | Yes, if the arbiter is to be removed by something other than fraud proofs. | Multisig now, token later. |

Recommendation: ship §2–§6 in ETH. Revisit a token when either the stake needs to scale
with usage or a bootstrap subsidy is actually needed, and design it as a work token
(stake + governance) with rewards still paid in ETH from fees.

## 9. Attack surface, after the changes

| Attack | Cost to attacker | Why it fails |
| --- | --- | --- |
| Post junk root, collect reward (§1.1) | stake at risk | The honest indexer of any asset the epoch claims loses money to the junk epoch, so it challenges; the junk publisher is slashed. |
| Spam tiny epochs | gas per epoch | Reward is per block of coverage, buckets pay once per period. Extra epochs earn nothing. |
| Attest without indexing | `attesterSlash` if wrong | Free-riding earns a share only when the proposer is right; losing side is slashed. Acceptable: an attester that only copies correct roots is harmless. |
| Register spam assets | `assetBond` per asset | Unchanged. |
| Wash indexing (fund your own asset, index it, collect) | zero net, minus gas | Harmless with fee-only rewards. Becomes an exploit the moment an inflationary subsidy exists (§8). |
| Colluding majority publishes a wrong root | all stakes | Needs one honest node with a bond to challenge. Same trust assumption as every optimistic system; the arbiter is the backstop until fraud proofs (§5). |
| Requester griefs by revoking mid-backfill | loses bounty | Bounty is not refundable once a backfill for the asset has started (first epoch declaring partial coverage). |

## 10. Migration from v1

Contract-side, in the order that pays off soonest:

1. **Done.** Coverage root on epochs; per-asset funding paid per block replacing
   `rewardPool[chainId]`. Fixes §1.1 (a junk epoch now steals from a specific asset's
   indexer, who can challenge) and §1.2. The bounty/maintenance split and period
   buckets were skipped: one balance per asset and a claim per asset per epoch is
   enough at hackathon scale.
2. **Done, daemon-side only.** The publisher quotes coverage before building and does
   not post below its floor. On-chain gas reimbursement and the finalize caller rebate
   are not built; with one publisher there is nobody else to reimburse.
3. Publisher stake + attestations + slashing.
4. Same-chain receipt fraud proof; arbiter becomes optional.
5. Registry-as-paymaster.

Daemon-side, 3 adds the read-before-publish step in §4.4.

## 11. Decisions

1. **Unit of account: ETH.** Decided. The `paymentToken` seam is not built; adding it
   is mechanical.
2. **Hackathon scope: minimum.** Decided. Steps 1 and 2 of §10 as described there.

Still open, for after the hackathon:

3. **Registry chain.** L2 (Base recommended) versus staying on the indexed chain. This
   sets every default in §6.4.
4. **Period length**, once attestations make periods necessary. Daily is the proposal.
5. **Split and slash constants.** 50/50 proposer/attesters, slash equal to one period's
   reward × 10, attester slash one-tenth of that. Placeholders until the L2 choice is
   made.
