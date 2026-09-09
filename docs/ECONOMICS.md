# Economics: fees, bonds and who pays for the work

Status: **proposal, minimum built.** PR #19 built the payment path: `requestIndexing`
deposits funding on an asset, an epoch commits a `coverageRoot` over the block ranges it
scanned per asset, and `claimCoverage` pays the publisher per newly covered block once
the epoch finalizes. PR #22 makes every economic parameter an immutable constructor
argument. The rest of this document is design: the threat model, gas-denominated
prices, leaf-scaled bonds, the dispute split and the registrant-chosen rate.

Covers issues #2, #4, #7, #14, #15 and #18, and the pricing ideas in #5. It replaces an
earlier draft built around a chain-wide reward pool with an equal split. Where this
reverses that draft, the reason is stated so it is not proposed again.

## 0. Threat model

**What is protected.** That a hint list was derived from the chain by the rule the spec
states, and that whoever derived it is the one paid for it. The money at risk is bonds in
flight and asset funding balances. Nothing here makes a hint *useful*; the registry says
which contracts an account touched, not which ones matter.

**Who can act.** Anyone can register, fund, publish, challenge and finalize. There is no
roster. The adjudicator, UMA's DVM in oracle mode or one key in local-arbiter mode,
decides disputes and nothing else.

**Security conditions.** Preconditions the economics assume, not goals they achieve. No
number in section 9 makes a deployment safe that fails one of these.

- **C1. Two independent operators run a full indexer, and one of them disputes a root
  it cannot reproduce.** Finalization means nobody challenged; with one operator it
  means nothing. A second node under the same operator does not count. The economics
  make the second operator's position affordable (section 7) and permissionless, but do
  not pay it to duplicate the first's work (section 6). A deployment has to arrange C1
  itself: a second wallet vendor, a foundation node, an operator who indexes anyway.
- **C2. A wrong root is well-defined.** The epoch commits its input set and history
  floor (#14) and a dispute names an account and its evidence (#15). Until then a bond
  is a bet on the adjudicator's mood. Section 8 is deferred for this reason.
- **C3. The adjudicator answers honestly.** Bonds only work if the loser loses. In
  local-arbiter mode this is a trusted key, and the deploy tool says so.
- **C4. `block.basefee` is set by protocol, not by a participant.** Every price in
  sections 4 and 5 is gas units times basefee. This holds on every EIP-1559 chain. Where
  the basefee is near zero, and mainnet at 0.07 gwei in September 2026 counts, the
  `minBasefee` floor is the price and does the work instead.

**Attacks the design prices:**

| Attack | Attacker | Handled in |
| --- | --- | --- |
| Register spam contracts to force backfills | registrant | §4: minimum funding, never refunded |
| Register and revoke in one block to grief eager mirrors | registrant | §4: asset bond locked for `assetLock` |
| Publish a wrong root | publisher | §5, §7: leaf-scaled bond, half to the challenger |
| Declare coverage that was not done | publisher | §6, §7: a false coverage root is a wrong root |
| Park honest epochs in `Challenged` | challenger | §7: half the bond burned, half to the publisher; nothing else is blocked |
| Dispute an honest epoch to win the race for its coverage reward | rival publisher | §7: publisher bonds at least twice the reward at stake |
| Self-dispute to move a bond between pockets | publisher | §7: burn |
| Run many identities to multiply income | publisher | §6: paid per covered block, once |
| Fund your own asset and claim it back | publisher | §12: nets to a loss after gas |
| Dust a million accounts with one registration | registrant | not an economics problem; §8 and #18 |

## 1. Principles

- **No protocol token.** Everything settles in the native asset or, in oracle mode, the
  ERC-20 the oracle already bonds in. A token adds a price to defend and a governance
  surface to capture, and buys nothing the design needs.
- **Every parameter is fixed at deploy and stated in gas units.** Constructor arguments,
  no setters (#4, #7, PR #22). Multiplied by the basefee at call time, so a deployment
  made at 0.07 gwei is still right at 7. Different numbers means a new deployment.
- **Pay for work, charge for cost.** Whoever imposes a cost on indexers pays money that
  is not returned. Whoever indexes is paid from it, per block covered, once. Bonds are
  separate: security that comes back if you behaved.
- **One payment path.** Every wei a publisher earns comes through `claimCoverage`. No
  second ledger, no periods, no shares.
- **Losing a dispute costs more than starting one.** Otherwise any epoch can be parked in
  `Challenged` for free, which is where the zero-bond defaults leave us.
- **The registry is a public good.** Reads are free. The system covers the marginal cost
  registrants create; it does not turn a profit.

## 2. Who does what

| Actor | Does | Pays today | Imposes on others |
| --- | --- | --- | --- |
| Registrant | Registers a `(chainId, token)` hint, optionally funds it | Gas, a refundable bond (default 0) | A full log-history backfill and permanent storage on every indexer |
| Publisher | Runs a node, indexes, publishes roots, finalizes, claims | Node, database, gas per epoch, one bond per epoch in flight | Nothing if honest; a wrong root misleads every consumer |
| Challenger | Recomputes an epoch, disputes a wrong one | A full indexer, gas, a bond | A stalled epoch while the dispute resolves |
| Consumer | Reads hints and proofs | Nothing | Load on a gateway |
| Oracle voters | Decide disputes (oracle mode) | Their stake in UMA | Nothing |

The imbalance is in the first two rows. Registrants impose the largest cost and pay a
bond they get back. Publishers bear the largest ongoing cost and, on the default branch,
earn nothing. Everything below moves money from the first row to the second, for work
that was done.

## 3. Where the code is today

Default branch:

- `assetBond` is refunded in full on `revokeAsset`. A spammer registers, forces a
  backfill, revokes and is made whole.
- `publisherBond` is flat per epoch, refunded on finalize, one constant for both modes.
- `challengeIndex` matches the publisher's bond. Winner takes all in local-arbiter mode;
  UMA applies its own split in oracle mode.
- Nobody is paid for anything. Deploy defaults: both bonds 0.

PR #19: `requestIndexing` registers and funds in one call. `Epoch` carries a
`coverageRoot` over `(assetKey, fromBlock, toBlock)` leaves. `claimCoverage` pays a
global `rewardPerBlock` for each block of a claimed range not paid before, capped by the
asset's balance. The daemon quotes `claimable` before publishing and skips epochs below a
configured floor.

PR #22: every parameter is `immutable`; `setArbiter`, `setBonds` and `setEconomics` are
gone.

## 4. Registration: funding that is kept, a bond that is not

**Prices are in gas units.**

```
price(g) = g * max(block.basefee, minBasefee)
```

The costs a price has to beat are transactions on the registry chain, and those move
with the basefee, so a price in gas units follows them without retuning. Amounts are
recorded in wei when paid, so a challenger a day later matches what the publisher
posted, not today's basefee. `minBasefee` is the one absolute number. It stops a cheap
chain from making everything cheap: mainnet's basefee has sat below 0.1 gwei for long
stretches, at 0.069 gwei on 8 September 2026 per Etherscan, and an L2's is a fraction of
that. On such a chain the floor binds nearly always and is the effective price, and the
gas-unit denomination only matters if the basefee climbs back above it. Choosing the
floor is therefore the real pricing decision, not a safety detail.

Gas units do not price off-chain work, reading receipts and keeping the result on disk.
That is priced per block of coverage and set by the registrant, below. Oracle-mode bonds
are in an ERC-20 and are derived from the oracle's own minimum instead (section 5).

**One entry point.**

```
requestIndexing{value: assetBond + funding}(chainId, token, kind, fromBlock, ratePerBlock)
    funding >= minFunding
```

Bare `registerAsset` goes away. A hint with no money behind it asks every indexer to
spend on a stranger's behalf, and the daemon already refuses coverage that does not pay.

**Funding is kept.** It is credited to the asset and paid out at `ratePerBlock` per
covered block (section 6). Because it never comes back, spam costs the minimum times the
number of contracts, and the money goes to the publishers whose disks were filled. The
earlier draft sent it to a chain-wide pool; attaching it to the asset makes it buy
something.

**The registrant sets `ratePerBlock`.** It is stored on the asset's `Funding` record and
kept across top-ups. PR #19's global `rewardPerBlock` is one number for a memecoin on an
L2 and a stablecoin on mainnet, and once immutable it is the number most likely to be
wrong. Registrants set a rate; publishers set a floor, `min_rate_per_block_wei`, and
index what clears it. The deployment sets no rate. That is a market for off-chain work
with no governor.

**History costs what it costs.** The balance buys `funding / ratePerBlock` blocks from
the declared `fromBlock`. A contract registered from genesis has more blocks to cover
than one from last week, and a `fromBlock` deeper than the real deploy block buys blocks
nobody will claim. There is no formula for this in the contract.

**`assetBond` is refundable after a lock.** `revokeAsset` works once
`registeredAt + assetLock` has passed, where `assetLock` is at least the challenge
window plus a backfill allowance. It stops register-and-revoke in one block against
mirrors that follow the registry eagerly. It is small; the funding does the real work.

**What this does not stop.** One legitimate-looking spam token airdropped to a million
addresses pays one minimum and lands on a million hint lists. That is a data-quality
problem, not a registration-cost problem (section 8).

## 5. Publishing: a bond that scales with the claim

Roots are cumulative snapshots, so the number of accounts grows while a flat bond does
not. A wrong root over ten accounts misleads ten wallets; over a million, a million.
`leafCount` becomes a field of `Epoch` (#14 wants it bound anyway) and the bond scales
with it.

Local-arbiter mode, native asset:

```
requiredBond(leafCount) = (baseBondGas + perLeafGas * leafCount) * max(block.basefee, minBasefee)
```

Oracle mode, `bondCurrency`:

```
minBond                 = oracle.getMinimumBond(bondCurrency)   // read at publish time
requiredBond(leafCount) = minBond * (1 + leafCount / leavesPerMinBond)
```

Two formulas because one constant cannot be both wei and USDC. The earlier draft priced a
single `baseBond` in ETH and was wrong in oracle mode by whatever the currency was.
Deriving from `getMinimumBond` puts the bond in the right currency, clears UMA's floor
and follows UMA's repricing without a setter here. The constructor's
`publisherBond >= getMinimumBond` check goes; the per-assertion path enforces it.

**The required bond is a floor.** `publishIndex` accepts more. Section 7 says when a
publisher should post more.

**Capital in flight.** Hourly epochs on a 7200 s window keep two bonds locked. Choose
`perLeafGas` or `leavesPerMinBond` so a mainnet-scale publisher's lockup is meaningful
but not a barrier to a second publisher entering, which C1 needs. Section 9 has numbers.

## 6. Paying for the work: per asset, per block, once

PR #19's mechanism, adopted as the only payment path. An epoch commits a `coverageRoot`
with one leaf per scanned asset, `(assetKey, fromBlock, toBlock)`. After finalization
anyone calls `claimCoverage(epochId, claims[])`. For each asset the contract pays the
publisher `ratePerBlock` for every block of the range not already paid, capped by the
asset's balance, and extends the paid range. Replays pay nothing. Ranges another epoch
already covered pay nothing. A hundred trivial epochs pay the same as one.

**Why not a pool with an equal split.** The earlier draft released a share of a per-chain
pool to every publisher who finalized in a period. Bonds are refunded on finalize, so a
share cost the gas of one trivial epoch, about 180k, and everything above that was sybil
profit. The pool also bought nothing specific: paying for asset X did not make X more
likely to be indexed. Per-block coverage has neither problem. A second identity earns
only for blocks the first did not cover, which is to say for work.

**What this means for C1.** Coverage pays once per block per asset. The first to finalize
a range is paid; a second publisher covering the same range earns nothing for it.
Pay-for-work is not pay-for-redundancy. The second operator earns from ranges the first
skipped (deeper history, underfunded assets), from catching a wrong root (section 7),
and from being first when the first stalls. Whether that keeps it running is a
deployment question. For a small deployment the honest answer is that the second
operator runs for its own reasons, and the economics only make sure it is not penalised
for existing. Paying agreeing publishers (attestations) is the alternative; section 12
says why it is not adopted.

**Gaps are paid as covered.** Claiming `[100, 200]` after `[10, 20]` was paid pays
`21..200`. The coverage leaf was bonded and unchallenged, the same standing as every
account leaf. Declaring coverage that was not done is publishing a wrong root.

**Only finalized epochs count.** Rejected epochs earn nothing and leave the range fresh.
Challenged epochs earn nothing until they resolve. A publisher who never finalizes is
never paid, which is why the daemon's finalization and claim sweeps matter.

**Where the money comes from.** `requestIndexing` and `fundAsset`. Consumer payments are
a possible later source (section 10). The publisher's share of a forfeited challenger
bond goes to the publisher directly. There is no inflation, because there is no token.

## 7. Disputes: symmetric bonds with a burn

The challenger posts the wei recorded on the epoch. On resolution:

| Outcome | Publisher's bond | Challenger's bond |
| --- | --- | --- |
| Root upheld | Returned | Half to publisher, half burned |
| Root rejected | Half to challenger, half burned | Returned |

**Why burn half.** If the loser's whole bond went to the winner, a publisher could
challenge its own epoch from a second address for free. A burn makes self-dispute, and a
frivolous challenge from a friend, cost real money. It is UMA's rule, so oracle mode has
it already; local-arbiter mode mirrors it so both modes have the same incentives.

**Why this stops parking.** Parking costs the griefer half a bond per epoch, paid to the
publisher, and scales with leaf count. A challenged epoch blocks nothing else: later
epochs finalize on schedule and `latestFinalizedEpoch` moves on. Parking buys a delay of
one epoch, for the DVM's voting period, at a price the publisher collects.

**Disputes and the reward.** A challenged epoch cannot claim until it resolves, and in
oracle mode that takes days. Meanwhile a rival can finalize the same ranges and claim
them, so an upheld publisher may find its blocks already paid to someone else, possibly
the challenger under a second key. Two rules handle it:

- Half the challenger's bond goes to the upheld publisher (table above).
- The publisher bonds at least twice the reward at stake. The daemon posts
  `max(requiredBond, 2 * claimable)`. Losing the race is then fully compensated and the
  redirect costs the attacker more than it moves. The contract cannot enforce this,
  because coverage leaves are off-chain; an under-bonded publisher risks only its own
  reward.

**No cap on live disputes per challenger.** A per-address cap costs a second address
nothing, so the only sybil-proof control is price. Stalling every epoch a publisher
posts for a week costs `epochs * bond / 2`, paid to that publisher, while consumers keep
reading the unstalled epochs. A well-funded griefer can still delay any one epoch by the
DVM period. That is the cost of using an oracle.

**What a challenger supplies.** #15 defines the payload: the account, what its assets
hash should have been, and the log positions that prove it. In oracle mode it is
referenced from the assertion so voters can check it; in local-arbiter mode it is an
event the arbiter reads. None of this works until a wrong root is well-defined (C2), so
#14 is a prerequisite.

**The honest challenger's return.** Half the publisher's bond has to exceed the cost of
running an indexer long enough to notice. With leaf-scaled bonds that holds for large
deployments and fails for small ones, which have little to protect. An operator already
publishing challenges at near-zero marginal cost and has a direct interest: a rival's
rejected epoch leaves ranges its own next epoch can claim.

## 8. Hint quality: deferred to the ERC draft

One spam token dusted to a million addresses (#18) pays one minimum and lands on every
consumer's hint list. No price on the registrant reaches those consumers. This needs a
data rule, not a price.

**Until #3 decides one, the committed set is the raw set**: every contract in the
epoch's input set (#14) that emitted an event naming the account within the covered
range. That is what `Publisher.Build` does today. It is imperfect and it is
reproducible, and reproducible is what sections 5 to 7 rest on. A rule two indexers
apply differently gives two "correct" roots and turns every dispute into opinion.

The earlier draft's two rules both failed that test. "Commit only what the account acted
on" needs the transaction sender, which `eth_getLogs` does not return, and a list of
event roles that count. "Distinct senders below a threshold" reads each indexer's local
candidates table, which depends on its own history floor. Neither belongs in a root
before it is in the spec.

Meanwhile the local API can serve a filtered view behind a flag. Whatever rule #3 adopts
changes what a leaf means and has to be bound into the commitment, most naturally via
the input-set hash from #14. The options are recorded in #18.

## 9. Worked numbers

Illustrative only; every deployment sets its own. Assumes a mainnet registry with
`minBasefee = 1 gwei`. Mainnet's basefee was 0.069 gwei on 8 September 2026 (Etherscan
gas tracker), so the floor binds and the amounts below are what the contract charges
today; the 10 gwei column is a spike. Rough gas from PR #19: `publishIndex` 180k,
`challengeIndex` 150k, `requestIndexing` 120k. At 0.07 gwei those transactions cost
0.000013, 0.000011 and 0.000008 ETH.

| Parameter | Value | At the 1 gwei floor (today) | At 10 gwei | Why |
| --- | --- | --- | --- | --- |
| `minBasefee` | 1 gwei | | | Fifteen times today's mainnet basefee, so the floor is the price until gas returns. On an L2 set it a similar multiple above that chain's typical basefee |
| `assetBondGas` | 2 000 000 | 0.002 ETH | 0.02 ETH | About 240 registrations of gas at today's basefee, fifteen at a 1 gwei basefee; locked for `assetLock` |
| `minFundingGas` | 1 000 000 | 0.001 ETH | 0.01 ETH | Ten thousand spam registrations cost 10 ETH, never returned, against 0.08 ETH of gas |
| `assetLock` | 30 days | | | Longer than any realistic backfill plus a challenge window |
| `baseBondGas` | 20 000 000 | 0.02 ETH | 0.2 ETH | Nearly two thousand times the gas of disputing today, over a hundred at a 1 gwei basefee |
| `perLeafGas` | 10 | +0.01 ETH per 1 M leaves | +0.1 ETH per 1 M leaves | A 1 M-account root posts 0.03 ETH today; 10 M posts 0.12 ETH |
| `leavesPerMinBond` (oracle) | 1 000 000 | | | A 1 M-account root posts twice UMA's minimum; the deploy tool prints the result |
| `challengeWindow` | 7200 s | | | Long enough to recompute an epoch, short enough that bonds turn over daily |
| `ratePerBlock` | registrant's choice; 1 gwei reference | | | Not gas-denominated. Genesis backfill (25 M blocks) costs 0.025 ETH; a year at the head (2.6 M) costs 0.0026 ETH |
| Burn share | 50 % | | | Matches UMA |

Registering a token from the chain tip today therefore costs 0.002 ETH bond plus
0.001 ETH funding plus about 0.00001 ETH of gas, and the bond comes back after 30 days.
The minimum funding buys one million blocks at the reference rate, about four and a half
months of head coverage.

Check against section 7: a thousand funded assets at 1 gwei per block and an hourly
epoch of 300 blocks put 0.0003 ETH of coverage at stake against a 0.02 ETH bond. The
"twice the reward" rule only binds on heavily funded assets, and the daemon checks it
anyway.

One quirk to know: the minimum funding scales with the basefee but `ratePerBlock` does
not, so registering during a gas spike deposits more ETH and buys more blocks at the
same rate. Registering at the floor buys the fewest blocks the contract allows.

At that rate head coverage of a thousand assets pays 2.6 ETH a year. That does not fund
a mainnet node and is not meant to. It covers the marginal cost registrants impose and
makes sure a second operator is not paying to exist. The primary publisher is expected
to be a wallet vendor who would run the indexer anyway; C1 says the second one has to
come from somewhere too.

## 10. Consumer payments: optional and later

Reads stay free. If a deployment wants a second income for publishers, pay-per-query via
x402 settling into `fundAsset` for the assets a query touched fits everything above and
changes nothing in the contract. Not worth building until two publishers want it.

## 11. Contract changes

Against PR #19 and PR #22:

- `Epoch` gains `leafCount` and, per #14, `inputSetHash` and `historyFloor`.
- `Funding` gains `ratePerBlock`. The global `rewardPerBlock` goes.
- Constructor takes `minBasefee, assetBondGas, minFundingGas, assetLock, challengeWindow,
  burnBps`, plus `baseBondGas, perLeafGas` (local-arbiter) or `leavesPerMinBond`
  (oracle). All immutable.
- `registerAsset` removed. `requestIndexing(chainId, token, kind, fromBlock,
  ratePerBlock)` requires `msg.value >= assetBond() + minFunding()`, records
  `registeredAt` and the rate, credits the rest to the balance. `fundAsset` keeps the
  rate.
- `revokeAsset` reverts before `registeredAt + assetLock`.
- `publishIndex` takes `leafCount` and requires `requiredBond(leafCount)`: as `msg.value`
  in local-arbiter mode, or as a `bondCurrency` amount passed to `assertTruth` in oracle
  mode. The constructor's `getMinimumBond` check goes.
- `challengeIndex` matches the wei recorded on the epoch, as today.
- `_resolve` applies the section 7 split in local-arbiter mode. Oracle mode is unchanged.
- `claimCoverage` pays at the asset's `ratePerBlock`.
- Views `requiredBond(leafCount)`, `assetBond()`, `minFunding()` evaluate the basefee
  formula so the daemon and the deploy tool quote what the contract will demand.
- Nothing from the earlier draft's pool: no `rewardPool`, `claimReward`, `fundRewards`,
  `rewardPeriod`, `releaseBps` or `RewardClaimed`.

Daemon: bonds `max(requiredBond(leafCount), 2 * claimable)`, gains
`min_rate_per_block_wei`, keeps PR #19's claim sweep next to the finalization sweep.
`evmscan-deploy` prints every parameter and the oracle bond implied for 1 M and 10 M
leaves, and refuses zero for `minBasefee`, `baseBondGas` and `challengeWindow`.

## 12. Open questions

- **`minBasefee` per chain.** Chosen by judgement from that chain's basefee history.
  Everything else follows from gas; this does not. On today's mainnet, at 0.07 gwei, the
  floor is the price nearly all the time, so this one number sets every bond and minimum
  until the basefee recovers. Setting it too low makes spam and parking cheap; too high
  keeps out the second publisher C1 needs.
- **Minimum funding in the bond currency in oracle mode?** A stablecoin minimum is more
  predictable for registrants but cannot follow the basefee. This document assumes
  native.
- **Farming your own funding.** A publisher who funds a hint and claims it back loses the
  gas. Not an attack today; it becomes one the day any subsidy exists, which is one more
  reason there is none.
- **Redundancy is unpaid.** If C1 cannot be met without paying a second, agreeing
  publisher, the alternative is attestations against a stake, which PR #19's earlier
  design sketched. Not adopted: it pays per identity and needs stake to survive sybil,
  and that is a much larger contract.
- **The hint-quality rule (section 8)** belongs in the ERC draft (#3) with #18. Until
  it is decided the committed set is the raw set.
