# evm-scan

Permissionless asset indexing for the access layer, built on your own node.

Wallets need to answer one question fast: **"what does this address hold?"** Answering it
properly means scanning the whole chain, which is why almost every wallet outsources it
to a handful of indexing providers. That is a real centralisation point in an otherwise
decentralised stack — and the data being outsourced is entirely public.

evm-scan takes the opposite approach: run a node, index only what someone actually asked
for, and publish the result back on-chain so nobody has to trust the indexer.

## The idea

Three moves, each of which removes a dependency:

**1. Index an allowlist, not a chain.** A general indexer must process every log ever
emitted. evm-scan only scans contracts that appear in an on-chain `HintRegistry`. The
index is bounded by the registered asset set rather than by chain size, which is what
makes it small enough to run yourself.

**2. Registration is permissionless and optimistic.** Anyone can register a
`(chainId, contract)` pair. There is no allowlist committee and no review. That is safe
because a hint confers nothing: it only asks the indexer to read logs that are already
public. A bad hint wastes our disk, not your trust. Registration buys exactly one
guarantee — that contract's full log history gets read **at least once**, which is
enough to build a complete per-account index for it.

**3. The output is a hint, not an oracle.** The derived `account → contracts` table is
committed on-chain as a merkle root with a challenge window. A wallet uses it to learn
*which contracts are worth pulling history for*, then fetches that history from whatever
source it trusts — including its own node. Nothing here has to be believed, which is why
publishing it permissionlessly is safe.

## Why snap sync is enough

The node requirement is the difference between "you could run this" and "you won't".
An archive node is terabytes; a snap-synced geth is not, and it is sufficient here.

Snap sync backfills every header, body and **receipt** to genesis while keeping state
only at the head. So it serves exactly the two operations this design uses:

| Need | Call | Available after snap sync |
| --- | --- | --- |
| Which accounts touched a contract | `eth_getLogs` over all history | yes — receipts are backfilled |
| What does this account hold now | `eth_call` at head | yes — head state is present |

What it cannot serve is **historical state**, so nothing in this codebase asks for a
balance at an old block. That constraint is deliberate and load-bearing: it is the reason
the node requirement stays at snap sync.

Balances are read live rather than summed from `Transfer` events, which also happens to
be the *correct* choice — rebasing, fee-on-transfer and upgradeable tokens all make
event-derived balances wrong.

## Architecture

```
    HintRegistry (on-chain)
    ├── registerAsset(chainId, token, kind, fromBlock)   permissionless, bonded
    └── publishIndex(chainId, from, to, root, uri)       optimistic, challengeable
              │  ▲
       mirror │  │ commitments
              ▼  │
    ┌──────────────────────┐        ┌─────────────────────────┐
    │      evmscand        │        │  your own geth          │
    │                      │        │  (snap sync)            │
    │  backfiller  ────────┼───────►│  eth_getLogs            │
    │  follower    ────────┼───────►│  eth_subscribe          │
    │  api         ────────┼───────►│  eth_call (head)        │
    └──────────┬───────────┘  IPC   └─────────────────────────┘
               │
          PostgreSQL          interactions rollup + pending buffer
```

**Two workers per chain, splitting at the registration anchor.** When an asset is
registered at block *N*, the **follower** walks forward from *N* and the **backfiller**
walks backward toward its deploy block. A newly registered contract produces useful data
within one block instead of after a full history scan, while history fills in behind it.

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

## API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/accounts/{addr}/contracts` | **The discovery surface.** Contracts this account has touched. |
| `GET` | `/v1/accounts/{addr}` | Same, enriched with token metadata and live balances. |
| `GET` | `/v1/assets` | Registered hints and their scan progress. |
| `POST` | `/v1/assets` | Register a hint locally (gated by `api.allow_registration`). |
| `GET` | `/v1/assets/{addr}/accounts` | Accounts known to have touched a contract. |
| `GET` | `/v1/epochs` · `POST /v1/epochs` | List / build + publish commitments. |
| `GET` | `/v1/epochs/{id}/proof?account=` | Inclusion proof for `verifyInclusion`. |
| `GET` | `/v1/status` · `/v1/health` | Sync state, node locality, index size. |

Every account response carries `as_of_block` so a caller can pin what it saw.

## Layout

```
contracts/src/       HintRegistry.sol + demo tokens; artifacts committed to contracts/out
internal/chain/      the only place that touches a node (Source read / Sender write)
internal/evmlog/     log decoding — which topics we watch and who is in them
internal/indexer/    backfiller, follower, reorg handling, rollup aggregation
internal/store/      PostgreSQL: rollup, pending buffer, commitments
internal/merkle/     commitment tree; must match HintRegistry byte-for-byte
internal/hintreg/    registry mirror (pull hints) + publisher (push commitments)
internal/api/        HTTP surface
cmd/evmscand/        the daemon
cmd/evmscan-demo/    devnet bootstrapper
cmd/evmscan-verify/  independent proof checker
```

`make check` runs fmt, vet and tests. Integration tests skip unless `EVMSCAN_TEST_NODE`
points at a node.

## Known limits

Being explicit about what this does *not* do:

- **Challenge adjudication is centralised.** `resolveChallenge` is settled by an
  `arbiter`. A real fraud proof would need to prove a log's existence on the source chain
  from another chain — receipt proofs against a known block hash — which is genuinely
  hard and out of scope here. The honest path forward is an optimistic oracle.
- **Reorgs deeper than `confirmations` are not repaired.** Same as every indexer of this
  shape; the lag is configurable.
- **Only identity-carrying token events are decoded** (`Transfer`, `Approval`,
  `ApprovalForAll`, `TransferSingle`, `TransferBatch`). A contract whose interactions
  never surface an address in an indexed topic will not produce hints.
- **ERC-1155 balances are not reported.** Balance there is per token id and the index
  tracks contracts, not ids. Reporting nothing beats reporting a misleading zero.
- **Commitments are cumulative snapshots**, so cost grows with total accounts rather than
  with recent activity. Fine at this scale; deltas or an accumulator would be the fix.
