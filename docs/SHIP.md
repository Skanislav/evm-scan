# SHIP.md — the last eight hours

Written 2026-09-13. One page that says what ships, what is cut, what the reader
does on the page, and the order the work runs in. When this file and another doc
disagree, this file wins for the ship.

## 0. The product, in one paragraph

Type a wallet. The page reads its holdings and **sorts them into recognized and
unrecognized on its own**, from signals it already has: a price and where it came
from (a Chainlink feed beats a DEX pool), a curated token list, who paid to index
it, how many others vouched, whether this wallet ever *sent* the token rather
than only received it, and whether the symbol impersonates a listed one. The
reader looks the split over, flips anything the page got wrong, and **signs one
message**. The next lookup, by anyone, is ordered by what was signed. That is
the whole act: no vote button, no aside toggle, no report, no hint card, no
on-chain transaction.

## 1. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **One signature, to the daemon, never on chain.** `Verdict(account, chainId, digest, deadline)` under EIP-712, same shape as the existing `Hint` type in `internal/api/hintsig.go`. The relay and the on-chain `vote`/`voteFor` leave the page; the contract stays frozen at `0x6D02…5Ad7`. | The contract only counts +1 and each on-chain vote is a second signature and a receipt wait. Nothing about the demo needs it. |
| D2 | **ENS leaves the ship.** No signed resolver deployment, `read.html` off the nav, the ENS card off the page, ENSv2 declared future. Name → address in the lookup box stays (browser only, Universal Resolver). | Three inconsistent ENS paths and none deployed. The ENSv2 custom-resolver exploration on Sepolia is a later project. |
| D3 | **The list is index ∪ committed ∪ the reader's own memory**, read through the lens on the active chain. The cross-chain sweep stays as one secondary card, not on the main path. | Two sweeps is two things to explain. |
| D4 | **`min_voters: 1` on the live profile for demo day.** | With 3 a single signer promotes nothing and the effect on stage is only ordering. At 1 a recognized-but-unindexed token gets indexed within a tick. |
| D5 | **Operator surface (assets, triage, ledger, graph) untouched.** | Works, not on the reader's path, no time to move it safely. |

## 2. The reader flow, step by step

1. **Enter** an address or a name. Name resolves in the browser through the
   Universal Resolver on the reader's RPC; the daemon only ever sees an address.
2. **Read.** `GET /v1/accounts/{addr}?prices=true` for the indexed contracts with
   roles, then balances and prices for the union of indexed ∪ committed ∪
   remembered through the lens (`/portfolio`, or the reader's own RPC).
3. **Classify.** Each row gets a proposed verdict and the reasons are drawn as
   chips so the reader can see *why*:

   | Signal | Source (already in the API) | Weight |
   |---|---|---|
   | Chainlink price (`confidence: high`) | `price.confidence` on the portfolio row | +3 |
   | DEX price (`medium`/`low`) | same | +1 |
   | On a curated token list | `known: true` on the row | +2 |
   | Someone paid to index it | `vouched_wei > 0` on the asset | +2 |
   | Others recognized it | `demand.for − demand.against` (new) | +1 per net signer, capped at +3 |
   | This wallet **sent** it | `roles` contains `sender` | +2 |
   | Only ever received, never sent | `roles` = receiver only | −1 |
   | Symbol impersonates a listed one | `lookalikeOf` on the page | −5 |
   | Reported by an operator | `reports > 0` | −5 |
   | No price, not listed, not vouched | all absent | −2 |

   Score ≥ 3 → **recognized**; ≤ −2 → **junk**; otherwise **unsure**. The
   reader's own previous signed verdict overrides the proposal for that row.
   Weights live in one object at the top of the page so they can be tuned in a
   minute.
4. **Verify.** Two groups drawn: recognized on top, the rest below, each row with
   its chips and a single toggle to move it across. Junk is drawn in the lower
   group with a red chip; the reader promotes it with the same toggle.
5. **Sign once.** One button: *Sign my verdict*. EIP-712
   `Verdict(account, chainId, digest, deadline)`; `digest = keccak256` over the
   sorted `(address, weight)` pairs. Wallet shows three named fields. The page
   posts the pairs plus the signature; the daemon recovers the signer, checks it
   is the account, checks the deadline is later than the last one stored, and
   upserts the rows.
6. **Remember.** `localStorage["evmscan.verdict.<account>"]` keeps the signed
   pairs and the deadline so the next lookup is pre-split the same way even
   before the daemon answers.
7. **Next query.** Everyone's list orders by the ordering rule in §4; a
   recognized-but-unindexed token with `net ≥ min_voters` is promoted by the
   next discovery tick and shows up indexed within a minute.

## 3. What is cut from the reader page

Private `.xorf` mode radio and panel; watchlist builder; aside toggle and
buttons; the EIP-712 hint-keep card; "Vote for N" and the per-chain sweep vote
cards; "Sign a vote for the registry" and the relay; "Pay to index it" per row;
add-network panel; challenge-epoch button; the ENS card; the `read.html` nav
link; the native `prompt()` for a per-chain RPC. Code for the private lookup, the
watchlist and the relay stays in `hints.js` and Go behind no UI; nothing is
deleted from Go and no route is removed.

Bugs the map surfaced, fixed in the same pass: the sweep was gated on the
hosted-lookup flag and `HOSTED_LOOKUP_FOR` never cleared; `config.mainnet-base.yaml`
cites a MAINNET.md §8 that does not exist.

## 4. The verdict API (fixed here so two agents build in parallel)

**Migration `0012_verdicts.sql`:**

```sql
ALTER TABLE asset_demand ADD COLUMN weight SMALLINT NOT NULL DEFAULT 1 CHECK (weight IN (-1, 1));
CREATE TABLE account_verdicts (
    chain_id  BIGINT      NOT NULL,
    voter     BYTEA       NOT NULL,   -- keccak256(salt ‖ chainId ‖ account), same as asset_demand
    deadline  NUMERIC(78) NOT NULL,   -- monotonic per (chain, voter): replay guard
    signed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, voter)
);
CREATE OR REPLACE VIEW asset_demand_totals AS
SELECT chain_id, address, SUM(voters)::BIGINT AS voters, SUM(against)::BIGINT AS against
FROM (
  SELECT chain_id, address,
         COUNT(*) FILTER (WHERE weight > 0)::BIGINT AS voters,
         COUNT(*) FILTER (WHERE weight < 0)::BIGINT AS against
  FROM asset_demand GROUP BY chain_id, address
  UNION ALL
  SELECT chain_id, address, voters, 0 FROM asset_demand_onchain
) t GROUP BY chain_id, address;
```

**`POST /v1/verdict`** — new route, allowlisted in `guarded()` beside
`/v1/demand` (`auth_test.go` holds it open):

```json
{
  "chain_id": 1,
  "account": "0x…",
  "deadline": "1757800000",
  "verdicts": [{"address": "0x…", "weight": 1}, {"address": "0x…", "weight": -1}],
  "signature": "0x…65 bytes"
}
```

- EIP-712 domain `{name: "evm-scan verdict", version: "1"}`, no chain, no
  contract (same reasoning as `Hint`). Type
  `Verdict(address account, uint64 chainId, bytes32 digest, uint256 deadline)`.
- `digest = keccak256(concat over pairs sorted by address of (address ‖ int8 weight))`.
  Go and the page compute it independently; a fixture in
  `internal/api/testdata/verdict.json` pins both.
- Signer must equal `account`; `deadline` must be in the future and strictly
  greater than the stored one for `(chain, voter)`.
- The whole set replaces the voter's previous rows for that chain: rows not in
  the list are deleted, `weight` upserted for the rest. A weight of 0 is not
  sent; absence is 0.
- Response `{"recorded": n, "cleared": n, "indexed_here": true}`.
- `POST /v1/demand` stays for the old page and curl, meaning weight +1.

**`GET /v1/demand`** rows gain `against` beside `voters`.

**Account response** (`GET /v1/accounts/{addr}` and `/contracts`): each contract
gains `committed: bool` (in the latest finalized epoch's leaf for this account,
from `epoch_leaves`) and `demand: {"for": n, "against": n}`.

**Ordering rule** (`orderAssets` in `internal/api/handlers.go` and the wallet
render in `web/index.html`):

1. any operator report or `against > for` sinks below every other row;
2. committed contracts first;
3. then `for − against` descending;
4. then `vouched` descending, then activity.

**Promotion:** `PromotableCandidates` and `DemandedUnseen` read
`(voters − against) >= min_voters`. `spam_at` still wins over any number of
signers.

## 5. ENS and the contract, for the record

**ENS.** Three uses exist in the code: names in through the Universal Resolver
(browser only, live, stays); the index out under `hints.evm-scan.eth` through
`HintSignedResolver` (v1 mainnet, publisher-signed, built and never deployed);
the index out through ENSv2 `HintResolver` + `evmscan-ens attach` (designed for
the Sepolia beta, no addresses, sim-tested only). The second and third are off
the ship and the docs say "future". `on.eth` chain-name resolution for
`POST /v1/chains` is unrelated and stays.

**Contract.** Frozen. Used live by the daemon: `publishIndex`, `finalizeIndex`,
`claimCoverage`, `claimable`, `getFunding`, `listAssets`, `listDemand`,
`getEpoch`, `latestFinalizedEpoch`, economics getters. Used by CLI/wallets:
`requestIndexing`, `vote`, `voteFor`, `verifyInclusion`, `contractsOf` +
callback, `setGateways`, ownership rotation. Unused: the UMA oracle branch,
`challengeIndex`/`resolveChallenge` (no arbiter tooling), `revokeAsset`,
`verifyCoverage`, `getAsset`, `isRegistered`, `demandOf`, `oracleMode`,
`renounceOwnership` (inherited; never call it). Pitch line: "one contract with a
funded-hint list, an optimistic commitment list, a coverage payout and a demand
counter".

**Indexing, three axes.** How a contract gets in (four paths; live: demand and
paid `requestIndexing`), what the follower and backfiller do per asset (forward
with a confirmation buffer, backward to `max(hint_from_block, history_floor)`),
what the publisher posts hourly on Base (index root, coverage root, filter
digest; finalize; claim). Oracle vs local-arbiter is read from the contract and
the live one is local-arbiter.

## 6. Work plan — phases

Estimates are wall clock; the sum leaves ~1.5 h slack.

### Phase 1 — build (parallel, three agents, disjoint files) ~2.5 h

| Agent | Files | Deliverable | Done when |
|---|---|---|---|
| **A backend** | `migrations/0012_verdicts.sql`, `internal/api/verdict.go` (+ `verdictsig.go` copied from `hintsig.go`), `internal/store/demand.go` + `discovery.go`, `handlers.go` (`orderAssets`, `committed`, `demand`), `server.go` route + `guarded()`, `auth_test.go`, tests + digest fixture | §4 exactly | `go test ./...` green; against a scratch Postgres a signed verdict replaces a previous one, `against` sinks a row, a bad signer is 401, a stale deadline is 409 |
| **B frontend** | `web/index.html`, `web/hints.js` | §2 flow, §3 cuts, classifier with weights in one object, single-signature button, localStorage memory, ordering rule | runs against the live API through a local proxy with A's stub; the split is visible with chips, one signature round-trips |
| **C docs** | `README.md` (product paragraph, reader flow, API table: `/v1/verdict`, `committed`, `demand`), `CLAUDE.md` (vote → verdict, ENS off the ship, invariants), `docs/ENS.md` (status: future), `docs/MAINNET.md` (§ order, §8 → §6d), `deploy/config.mainnet-base.yaml` (`min_voters: 1` + comment) | docs match §0–§5 | no doc names a control the page no longer has |

B builds against §4 without waiting for A; a stub is fine until the merge.

### Phase 2 — integrate (serial) ~1.5 h

1. Merge A and B; `make check`.
2. Run A's daemon locally on a scratch Postgres with `web/` served from it,
   `/v1/*` reads proxied to Railway where the local index is empty (or a
   restored snapshot from `GET /v1/epochs/{id}/snapshot` via `evmscan-restore`).
   Walk §7 steps 1–7.
3. Fix, commit.

### Phase 3 — deploy (the user, with `!`) ~1 h

1. `railway up` from the working tree. Migration 0012 applies on boot.
2. Publisher key balance on Base: enough for ~3 tx/h for two days.
3. Optional, for the sponsorship story: every asset's funding is 0 and epochs 6–7
   paid zero reward. `evmscan-deploy -request <token>:20:<block ≥ history_floor
   from /v1/status>:<wei ≥ 1e14>` against Base for a **mainnet** token. Not
   refundable; get the block right.
4. Epoch 3 (`built`, never submitted) blocks nothing; leave it.

### Phase 4 — rehearse ~1 h

Walk §7 twice on the live URL, the second time from a clean browser profile,
and record the second run.

## 7. Demo script and checklist

Live URL, wallet `vitalik.eth` or `skas-me.eth`:

1. [ ] `/v1/health` 200; `/v1/status` head advancing; latest epoch `finalized`.
2. [ ] Type the name → address resolved, holdings drawn with balances and prices,
       split into recognized / unrecognized with reason chips. Committed rows
       carry the badge and sit first.
3. [ ] The split is sensible on this wallet: USDC/WETH recognized (feed, listed,
       sent); the homoglyph `꒤5DT` in junk with the lookalike chip; airdrop dust
       unsure or junk.
4. [ ] Flip one row each way. Click *Sign my verdict*; wallet shows account,
       digest, deadline; page confirms `recorded: n`.
5. [ ] Reload → the split is remembered. `GET /v1/demand` shows `for`/`against`
       for the flipped rows.
6. [ ] Same wallet in a second browser profile → the flipped rows sit where the
       signature put them (ordering is shared, memory is not).
7. [ ] A recognized-but-unindexed token: within a minute `/v1/assets` lists it
       with source `demand`, and the next lookup shows it indexed.
8. [ ] Sign again with one row moved back → old rows replaced, not added; a
       replayed old signature is refused.
9. [ ] Beat5: proof loads for the looked-up account and
       `evmscan-verify -api … -epoch N -account …` agrees.
10. [ ] `make check` green on the deployed commit; `git log -1` matches what
        `railway up` shipped.

Operator checks before stage: `EVMSCAN_API_TOKEN` set; publisher funded on
Base; Helios checkpoint fresh; Base RPC key valid; `min_voters: 1` deployed.

## 8. Deliberately not in the ship

ENS index records (v1 signed and v2 verified); on-chain votes from the page;
oracle mode; private `.xorf` lookup and blinded watchlists (kept in code, off
the page); the local-first mirror; multichain asset identity; tokenomics past
per-asset funding per block.

Candidates raised on ship day, for after: a sparse Merkle (binary) trie over the
key space, which would add non-membership proofs at the cost of a contract and
tree change on both sides; quotient filters for the published index filter, which
would add deletion and merging with a canonical layout at roughly twice a fuse8's
size. Neither changes the reader flow above.
