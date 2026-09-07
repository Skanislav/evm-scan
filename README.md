# evm-scan

Permissionless asset indexing for the access layer, built on your own node.

Wallets need to answer one question fast: **"what does this address hold?"** Answering it
properly means scanning the whole chain, which is why almost every wallet outsources it
to a handful of indexing providers. That is a real centralisation point in an otherwise
decentralised stack — and the data being outsourced is entirely public.

evm-scan takes the opposite approach: run a node, index only what someone actually asked
for, and publish the result back on-chain so nobody has to trust the indexer.

## The idea

Four moves, each of which removes a dependency:

**1. Discover contracts at runtime, don't demand a list.** The indexer watches the head
with a topic filter and records every contract emitting identity-carrying token events.
That costs a counter per contract — no per-account rows, no history reads. Nothing has
to be known up front.

**2. Spend history only on what earns it.** A discovered contract is just a *candidate*.
Only when one is promoted — because it was registered on-chain, because an operator
said so, or because it crossed an activity threshold — does it get a per-account index
and a walk back through its history. Discovery is cheap and wide; indexing is expensive
and narrow. Keeping those separate is what makes the whole thing fit on one machine.

**3. Registration is permissionless and optimistic.** Anyone can register a
`(chainId, contract)` pair in the on-chain `HintRegistry`. There is no allowlist
committee and no review. That is safe because a hint confers nothing: it only asks the
indexer to read logs that are already public. A bad hint wastes our disk, not your
trust. Registration is the authoritative "this contract is worth indexing" signal, and
it buys one guarantee — that contract's available log history gets read **at least
once**, which is enough to build a complete per-account index for it.

**4. The output is a hint, not an oracle.** The derived `account → contracts` table is
committed on-chain as a merkle root with a challenge window. A wallet uses it to learn
*which contracts are worth pulling history for*, then fetches that history from whatever
source it trusts — including its own node. Nothing here has to be believed, which is why
publishing it permissionlessly is safe.

## Why a snap-synced node is enough

The node requirement is the difference between "you could run this" and "you won't".
This design uses exactly two node operations, and neither needs an archive:

| Need | Call | Requires |
| --- | --- | --- |
| Which accounts touched a contract | `eth_getLogs` | receipts, for the range being read |
| What does this account hold now | `eth_call` at head | head state |

Two things follow, and both are load-bearing:

**No historical state, ever.** Nothing in this codebase asks for a balance at an old
block, so no archive node. Balances are read live from head — which is also the *correct*
answer, since rebasing, fee-on-transfer and upgradeable tokens all make balances derived
from summed `Transfer` events wrong.

**No assumption that history reaches genesis.** A node may have been synced without
ancient receipts, or may prune them later. So the indexer *probes* its node for the
oldest block whose logs it can actually serve (a binary search over `eth_getLogs`, since
there is no RPC for this) and treats that as a hard floor. Backfills stop there. An asset
whose history is truncated by the floor reports `history_complete: false` rather than
implying coverage it does not have.

That is why discovery runs forward from the head rather than backward from genesis:
history is the scarce resource, and the system only spends it on contracts something has
already decided are worth indexing.

## Reading state: one call, no deployment

Discovery answers *which* contracts to ask about. Actually reading them is the other
half, and doing it over plain RPC is worse than it looks: a balance, an allowance, a
symbol, decimals, an owner, a URI — one `eth_call` each. Twenty tokens is well over a
hundred round trips, and because each lands at whatever block the node was on, the
"portfolio" that comes out never existed as a state the chain was ever in.

`contracts/src/AssetLens.sol` does all of it inside one EVM execution: native balance,
ERC-20 balances and allowances, ERC-165 detection, NFT ownership and URIs per token id,
ERC-721Enumerable walking, and the account's own code, code hash and **EIP-7702
delegation target**. One round trip, one block, one consistent snapshot.

**It is never deployed.** `eth_call` with no `to` address executes creation code and
returns whatever the constructor returns, so shipping the bytecode as the call payload
turns a contract into a pure function:

```
eth_call({ data: <AssetLens creation code> || abi.encode(request) }, "latest")
```

That keeps the premise of the project intact. There is no address to trust, no upgrade
key, no deployment to fund on each chain, and nothing to coordinate with anyone: the
lens works against any EVM node, on any chain, the moment it compiles. Same posture as
everything else here — read public state from your own node, ask nobody's permission.

Three consequences worth knowing, all handled in `internal/lens`:

- **The reply is treated as contract code**, so EIP-170's 24576-byte limit applies to
  it (EIP-3860's 49152 applies to the payload). Too large a portfolio fails the call
  outright rather than truncating, so the client batches tokens and halves the batch
  when a node says no — the same adaptive shape the `eth_getLogs` sweep uses. Chains
  differ on where that ceiling sits, which is exactly why the limit is discovered
  rather than assumed.
- **Nonces are not in there.** The EVM has no opcode for another account's nonce, so
  no contract — deployless or not — can report one. It comes from
  `eth_getTransactionCount` alongside the call, and the response says whether that
  read landed instead of printing a confident `0`.
- **Hostile tokens are boxed.** Every read the lens makes is a staticcall with capped
  gas and capped returndata, and nothing a token does can revert the batch. A token
  that stays silent comes back as *unknown*, never as zero: a wallet rendering a
  failed call as a balance of nought is a bug, not a rounding error.

## Architecture

```
    HintRegistry (on-chain)                       Optimistic Oracle V3
    ├── registerAsset(chainId, token, kind)       permissionless, bonded
    ├── publishIndex(chainId, from, to, root) ──► assertTruth
    ├── challengeIndex(epochId) ──────────────►   disputeAssertion
    ├── finalizeIndex(epochId) ───────────────►   settleAndGetAssertionResult
    └── resolved / disputed callbacks ◄───────  (the oracle calls back)
              │  ▲
       mirror │  │ commitments
              ▼  │
    ┌──────────────────────┐        ┌─────────────────────────┐
    │      evmscand        │        │  your own geth          │
    │                      │        │  (snap sync, any depth) │
    │  discovery   ────────┼───────►│  eth_getLogs (all)      │
    │  follower    ────────┼───────►│  eth_subscribe          │
    │  backfiller  ────────┼───────►│  eth_getLogs (promoted) │
    │  api         ────────┼───────►│  eth_call (head)        │
    └──────────┬───────────┘  IPC   └─────────────────────────┘
               │
          PostgreSQL     candidates (counters) + interactions rollup
```

**Discovery is wide and shallow; indexing is narrow and deep.** The discovery sweep is
the one query with no address filter — it looks at every contract on the chain — but it
only ever writes counters (`event_count`, `blocks_seen`) to a `candidates` table. The
per-account index, and any history read, is reserved for promoted contracts. That
asymmetry is what lets a single machine watch a whole chain.

**Three ways in.** A contract becomes indexed by being registered in the on-chain
registry, by an operator promoting it (`POST /v1/candidates/{addr}/promote`), or by
crossing activity thresholds when `auto_promote` is on. The last is off by default:
promotion commits a real backfill, and the registry is the authoritative signal.

**Two workers per promoted asset, splitting at the promotion anchor.** From block *N*,
the **follower** walks forward and the **backfiller** walks backward toward the node's
history floor. A newly promoted contract produces useful data within one block instead of
after a full history scan, while history fills in behind it.

**Reorgs cannot corrupt the index.** `interactions` is a rollup, and events only enter it
once they are deeper than the configured confirmation lag. Shallower events sit in a small
`pending_events` buffer, so a reorg deletes buffered rows and rescans — the aggregate is
never wrong. Storing every raw event would defeat an allowlisted index; buffering only the
unconfirmed window costs almost nothing.

**The subscription is a latency hint, not a data path.** `eth_subscribe` wakes the
follower early, but every log still reaches the index through the same `eth_getLogs`
sweep the poller uses. Two ingestion paths that could disagree, one of which silently
drops data when a subscription dies, is not worth the milliseconds.

**`require_local_node` is enforced, not documented.** The daemon refuses to start against
a non-loopback endpoint unless you explicitly opt out. A guarantee this central should
fail loudly.

## Quick start

Needs Go 1.24+, PostgreSQL 16+, and a geth binary. No third-party RPC required or used.

```bash
# 1. database
createdb evmscan

# 2. a local chain to play against
geth --dev --dev.period 2 --datadir .devchain --ipcpath "$PWD/.devchain/geth.ipc"

# 3. deploy the registry + demo tokens, generate traffic, write a config
make build
./bin/evmscan-demo -node "$PWD/.devchain/geth.ipc" -out config.demo.yaml

# 4. run the indexer + API + UI
./bin/evmscand -config config.demo.yaml
```

Then open <http://127.0.0.1:8080>, or:

```bash
# what has this account touched?
curl localhost:8080/v1/accounts/0xF431…0F9a/contracts

# what does it hold *now*? one deployless eth_call, one block
curl "localhost:8080/v1/accounts/0xF431…0F9a/portfolio?spenders=0x1111…&nfts=8"

# commit the index on-chain
curl -XPOST localhost:8080/v1/epochs -d '{"publish":true}'

# verify that commitment independently, in Go *and* against the Solidity verifier
./bin/evmscan-verify -node "$PWD/.devchain/geth.ipc" \
  -registry 0x… -epoch 1 -account 0xF431…0F9a
```

`evmscan-verify` recomputes the asset digest and leaf locally instead of trusting the
API's, replays the proof in Go, and then calls `HintRegistry.verifyInclusion` on-chain.
Both verifiers have to agree.

To point at a real network, copy `config.example.yaml`, set `node` to your own
snap-synced geth's IPC path, and raise `confirmations` to your reorg tolerance.

## Who settles a disputed commitment

A commitment is optimistic: it finalizes on its own unless someone bonds a challenge
against it inside the window. What happens *after* a challenge is the whole trust
question, and a registry answers it in one of two modes, fixed at deployment and
readable on-chain from `oracle()` / `arbiter()`:

**Oracle mode** — `publishIndex` asserts `(chainId, fromBlock, toBlock, root, uri)` to
UMA's Optimistic Oracle V3, bonding an ERC-20 the deployment names. Anyone disputes it,
either at the oracle or through `challengeIndex`, which is a thin wrapper over
`disputeAssertion`. UMA's vote decides; the registry only reacts to the oracle's
callbacks. There is no arbiter and no reachable admin setter — `setArbiter`, `setBonds`
and `resolveChallenge` all revert permanently, because the arbiter is the zero address
and the oracle is not.

**Local-arbiter mode** — the fallback for a chain with no oracle deployment. Bonds are
in wei and one `arbiter` key settles challenges. It is a trusted deployment wearing a
permissionless write path, which is why the deploy tool says so in capitals.

```bash
# neutral: UMA settles, nobody administers
./bin/evmscan-deploy -node "$PWD/.devchain/geth.ipc" \
    -oracle 0xOOv3… -bond-currency 0xUSDC… \
    -publisher-bond 500000000000000000 -challenge-window 7200

# fallback: one key settles
./bin/evmscan-deploy -node "$PWD/.devchain/geth.ipc" -arbiter 0xyou…
```

The publisher settles its own commitments: `evmscand` sweeps published epochs each
auto-publish tick and calls `finalizeIndex` on the ones the chain will let it settle,
which is what returns the bond. `evmscan-verify` prints the mode and the commitment's
on-chain status alongside every proof it checks, so a consumer sees who could still
overturn the root it just verified.

## API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/accounts/{addr}/contracts` | **The discovery surface.** Contracts this account has touched. |
| `GET` | `/v1/accounts/{addr}` | Same, enriched with token metadata and live balances. |
| `GET` | `/v1/accounts/{addr}/portfolio` | **Live state via the deployless lens**: balances, allowances, NFT ids, nonce, 7702 delegation — one call, one block. |
| `GET` | `/v1/assets` | Registered hints and their scan progress. |
| `POST` | `/v1/assets` | Register a hint locally (gated by `api.allow_registration`). |
| `GET` | `/v1/assets/{addr}/accounts` | Accounts known to have touched a contract. |
| `GET` | `/v1/candidates` | Contracts discovered at the head, ranked by activity. |
| `POST` | `/v1/candidates/{addr}/promote` | Commit a discovered contract to being indexed. |
| `GET` | `/v1/epochs` · `POST /v1/epochs` | List / build + publish commitments. |
| `GET` | `/v1/epochs/{id}/proof?account=` | Inclusion proof for `verifyInclusion`. |
| `GET` | `/v1/status` · `/v1/health` | Sync state, node locality, index size. |

Every account response carries `as_of_block` so a caller can pin what it saw.

## Layout

```
contracts/src/       HintRegistry.sol, AssetLens.sol + demo tokens; artifacts in contracts/out
contracts/evmtest/   separate module: runs the lens in a real EVM (heavy test-only deps)
internal/chain/      the only place that touches a node (Source read / Sender write)
internal/lens/       the deployless AssetLens client: encode, batch, decode
internal/evmlog/     log decoding — which topics we watch and who is in them
internal/indexer/    discovery, backfiller, follower, reorg handling, rollup aggregation
internal/store/      PostgreSQL: rollup, pending buffer, commitments
internal/merkle/     commitment tree; must match HintRegistry byte-for-byte
internal/hintreg/    registry mirror (pull hints) + publisher (push commitments)
internal/api/        HTTP surface
cmd/evmscand/        the daemon
cmd/evmscan-demo/    devnet bootstrapper
cmd/evmscan-verify/  independent proof checker
cmd/evmscan-deploy/  registry deployer; prints the adjudication mode it just fixed
```

`make check` runs fmt, vet and tests, including `make test-evm` — the lens executed in
a real EVM in `contracts/evmtest`, which lives in its own module so go-ethereum's
in-process node never lands in the daemon's dependency graph. Integration tests against
a real node skip unless `EVMSCAN_TEST_NODE` points at one.

## Known limits

Being explicit about what this does *not* do:

- **Disputes are adjudicated socially, not by proof.** In oracle mode a dispute goes to
  UMA's DVM, which is a human vote, not a fraud proof. A real fraud proof would need to
  prove a log's existence on the source chain from another chain — receipt proofs against
  a known block hash — which is genuinely hard and out of scope here. What the oracle
  buys is that no single key decides, and the deployment has no admin path at all.
- **Local-arbiter mode is one key.** On a chain with no oracle deployment the registry
  falls back to an `arbiter` that settles disputes and can retune the bonds.
  `evmscan-deploy` prints a warning when it deploys in that mode; treat such a deployment
  as trusted, not neutral.
- **Reorgs deeper than `confirmations` are not repaired.** Same as every indexer of this
  shape; the lag is configurable.
- **History is only as deep as your node.** The floor is probed, respected and reported,
  but an asset promoted on a node without ancient receipts simply has shallower history.
  `history_complete` on the asset says whether the walk reached what was asked for.
- **Candidate ranking is crude** — event count and distinct blocks. It separates active
  contracts from idle ones, but not a widely-held token from a large spam airdrop. That
  is why `auto_promote` defaults to off and the on-chain registry stays authoritative.
- **Only identity-carrying token events are decoded** (`Transfer`, `Approval`,
  `ApprovalForAll`, `TransferSingle`, `TransferBatch`). A contract whose interactions
  never surface an address in an indexed topic will not produce hints.
- **ERC-1155 balances are not reported by the index-driven views.** Balance there is
  per token id and the index tracks contracts, not ids. Reporting nothing beats
  reporting a misleading zero. The portfolio endpoint *will* return per-id balances,
  but only for ids you name: nothing here discovers which ids an account holds unless
  the contract implements ERC-721Enumerable.
- **The lens reads, it does not verify.** It runs on your node against head state, so
  its answers are exactly as good as that node — which is the point, but it means a
  portfolio is a live read, never a commitment. Nothing about it is published or
  challengeable.
- **Commitments are cumulative snapshots**, so cost grows with total accounts rather than
  with recent activity. Fine at this scale; deltas or an accumulator would be the fix.
