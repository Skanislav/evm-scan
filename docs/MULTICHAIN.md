# Multichain

How evm-scan grows from "a deployment that happens to dial more than one node" to
one that discovers an account's assets across every network it can reach, and can be
pointed at a new network without a redeploy.

Read README.md first for why the index is shaped the way it is. This document only
adds; nothing here changes the merkle leaf, the registry, or the two node operations.

## What already works

More of this exists than it looks like from the outside.

- Every table is keyed `(chain_id, …)`. `assets`, `asset_cursors`, `interactions`,
  `pending_events`, `candidates`, `discovery_cursor`, `epochs` are all chain-scoped
  already, and `chains` is a real table rather than a config echo.
- `HintRegistry` is one contract on one chain that adjudicates hints for *any* chain:
  `assetKey = keccak256(chainId, token)`, and epochs, coverage leaves and
  `latestFinalizedEpoch` are all per `chainId`. A registry on Base can pay for
  Optimism indexing today.
- `cmd/evmscand` already runs one `indexer.Service` per configured chain, and
  `price.Defaults` already carries per-chain oracle and DEX addresses for mainnet,
  Optimism, Base and Arbitrum.

The topology this document is aimed at is already expressible, which is the useful
part: **mainnet indexed, the `HintRegistry` deployed on Base, and a third chain — an
Arc testnet, say — indexed alongside them.** The registry chain has never had to be
one of the indexed chains' peers.

So the gaps are four:

1. **Chains are frozen at startup.** They come from YAML, and adding one means
   editing a file and restarting the process.
2. **Adding one means knowing its chain id.** A number nobody remembers, typed by
   hand, wrong silently.
3. **Assets have no identity above a chain.** `(1, 0xA0b8…)` and `(8453, 0x8335…)`
   are both USDC and the system has no way to say so.
4. **An account lookup is a single-chain question.** Nothing walks from "you hold
   this on mainnet" to "you also hold it on Base".

Everything below is in service of those four, in the order that they constrain each
other. The first two are what make a network *addable*; the last two are what make
having several of them worth anything.

## 1. The trust boundary comes first

Adding a chain by RPC URL means indexing from someone else's node. That is not a
data-quality footnote; the publisher posts a *bonded* commitment, and a bond is real
money staked on data we did not verify. `require_local_node` exists precisely to make
that failure loud, and a runtime `POST /v1/chains` with a `https://…/<api-key>` URL
would quietly route around it.

So every chain carries a trust level, and it is derived, not asserted:

| level | how it is reached | what it may do |
| --- | --- | --- |
| `verified` | loopback endpoint, or a light client (Helios) that checks logs against receipts roots | indexed, served, **committed on-chain** |
| `unverified` | any remote RPC | indexed, served, **never committed** |
| `quarantined` | its indexer has been failing | not served as fresh; head marked stale |

Rules that follow:

- `chain.Dial` already refuses a non-loopback endpoint under `require_local_node`.
  A chain added at runtime with a remote URL sets `require_local_node: false` *for
  that chain only* and lands at `unverified`.
- `publisherLoop` iterates verified chains only. An unverified chain never produces
  an epoch, so it never risks the bond. `ErrUntrusted` joins `ErrUnfunded` as a
  quiet, expected reason `Build` declines.
- Every API response that carries data from an unverified chain says so:
  `"verified": false` on the chain block, propagated into portfolio and account
  responses. A consumer that wants only committed data filters on it.

### Promoting a chain to verified

An operator who runs their own node behind a reverse proxy, or who has decided a
particular provider is good enough to stake on, may promote a chain:
`PATCH /v1/chains/{id}` with `{"trust": "verified"}`.

Be clear about what that endpoint is. It is the largest thing this API can authorize
— larger than publishing an epoch, larger than promoting an asset — because it puts
the publisher's bond behind logs served by a machine we do not run. Everything else
the API can spend is gas or quota, and both are recoverable. So:

- It joins `spendsSomething` in `internal/api/server.go`, which means
  `EVMSCAN_API_TOKEN` is the boundary. A deployment with a funded publisher and no
  token set is not merely open, it is open to this.
- The decision is recorded, not just applied: `trust_set_at` and `trust_set_by` on
  the chain row, plus a warn-level log naming the chain and the redacted endpoint.
  Six months later, "why is this chain trusted" has an answer.
- Demotion is unremarkable and always allowed. `{"trust": "unverified"}` stops
  future epochs immediately; epochs already posted stand or are challenged on their
  merits, like any other.

### What trust is not

Trust is a claim about **who served the logs**. It is not a claim about the chain's
identity, its popularity, or how the operator found it. In particular, resolving a
chain through ENS (§3) proves that a name maps to a chain id — it says nothing
whatsoever about the node that will be dialled, and must never raise a chain's trust
level. A chain added by name over a public RPC is `unverified`, exactly like one
added by hand over the same RPC.

## 2. Chains become runtime state

### Schema — `migrations/0006_chains_profile.sql`

`chains` grows from a head tracker into the profile:

```sql
ALTER TABLE chains ADD COLUMN enabled           BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE chains ADD COLUMN source            TEXT    NOT NULL DEFAULT 'config'; -- config|api|demand
ALTER TABLE chains ADD COLUMN trust             TEXT    NOT NULL DEFAULT 'unverified';
ALTER TABLE chains ADD COLUMN node_url          TEXT;        -- never returned unredacted
ALTER TABLE chains ADD COLUMN native_symbol     TEXT;
ALTER TABLE chains ADD COLUMN native_decimals   SMALLINT NOT NULL DEFAULT 18;
ALTER TABLE chains ADD COLUMN wrapped_native    BYTEA;
ALTER TABLE chains ADD COLUMN profile           JSONB   NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE chains ADD COLUMN last_error        TEXT;
ALTER TABLE chains ADD COLUMN last_error_at     TIMESTAMPTZ;

-- How the chain was identified, and who vouched for its node. Both are audit
-- trail: the first says a human typed a name rather than a number, the second
-- says an operator staked the bond on someone else's RPC and when.
ALTER TABLE chains ADD COLUMN ens_name          TEXT;
ALTER TABLE chains ADD COLUMN ens_resolved_at   TIMESTAMPTZ;
ALTER TABLE chains ADD COLUMN trust_set_at      TIMESTAMPTZ;
ALTER TABLE chains ADD COLUMN trust_set_by      TEXT;
```

`profile` holds the tuning that is already `config.Chain` — confirmations, windows,
intervals, discovery thresholds, pricing overrides — as one JSON blob rather than
fifteen columns, because it is written and read whole and never queried into.

**The URL is a secret.** It is where the API key lives; commit 5e35689 was about
exactly this. It is stored so a restart can bring the chain back, it is redacted in
every response (`https://eth.example/v1/***`), and `EVMSCAN_NODE_<chain_id>` still
overrides it — that env pattern already exists and is the right seam for rotating a
key without touching the database.

### Config chains and DB chains

Config is authoritative for the chains it names. On start, each `config.Chains` entry
is upserted with `source: 'config'`; DB rows with `source` of `api` or `demand` are
loaded after and never overwrite one from config. A config chain that disappears from
the file is disabled, not deleted — its index is still worth keeping.

### The supervisor

`main.go` currently holds four parallel maps (`sources`, `services`, `workers`,
`pricers`) plus `chainOrder`, built once and read by the API for the process's life.
Runtime mutation makes that a data race. They collapse into one type:

```go
// internal/chainset — the live set of chains this process runs.
type Set struct { mu sync.RWMutex; entries map[uint64]*Entry; order []uint64 }

type Entry struct {
    ID      uint64
    Profile store.ChainProfile
    Source  chain.Source
    Worker  *indexer.Service
    Pricer  *price.Pricer
    Trust   store.Trust
    cancel  context.CancelFunc
}

func (s *Set) Start(ctx context.Context, p store.ChainProfile) error
func (s *Set) Stop(id uint64) error
func (s *Set) Get(id uint64) (*Entry, bool)
func (s *Set) IDs() []uint64          // stable order: config first, then added
func (s *Set) Verified() []uint64
```

`Start` does what the loop in `main.go` does today — dial, verify the reported chain
id against the requested one, build the service and pricer, launch the goroutine —
and then registers the entry under the write lock. Every existing consumer switches
from a captured map to a `*chainset.Set`:

- `api.Deps.Sources/Workers/Pricers/ChainOrder` → `api.Deps.Chains *chainset.Set`.
- `hintreg.NewMirror`'s `head`, `code` and `nudge` closures read through the set, so
  a chain added at 11am is mirrored at 11am without a restart. `nudge` stops being a
  prebuilt map and becomes a lookup.
- `publisherLoop` ranges `Chains.Verified()` instead of `cfg.Chains`.

### Failure is not fatal for a runtime chain

Today an indexer that stops kills the process, and that is right: a config chain that
cannot run means the deployment cannot do its job, and a supervisor restart is the
correct answer. A chain someone added over the API is different — a bad URL must not
take down the mainnet index. So:

- `source: 'config'` → indexer failure cancels the root context, exactly as now.
- `source: 'api' | 'demand'` → indexer failure marks the chain `quarantined`, records
  `last_error`, stops its goroutine, and retries with exponential backoff (1m → 30m).
  `/v1/health` reports it as degraded but the process stays up and `200`s.

### API

```
GET    /v1/chains                 list, with head, trust, floor, assets, cost
POST   /v1/chains                 add and start one              (auth: spends)
GET    /v1/chains/resolve         resolve a name through ENS (see §3)
POST   /v1/chains                 add and start one              (auth: spends)
PATCH  /v1/chains/{id}            enable/disable, retune, rotate URL, set trust (auth: spends)
DELETE /v1/chains/{id}            stop and disable; data retained (auth: spends)
```

`POST` and `PATCH /v1/chains` are added to `spendsSomething`; the two reads stay
open, like every other read here. A chain is the largest spend the API can
authorize — a permanent, per-block RPC bill on every tick from then on, not one
backfill — and `{"trust":"verified"}` on the PATCH is larger still, for the reason
§1 gives.

```jsonc
POST /v1/chains
{
  "chain_id": 8453,
  "name": "base",
  "node": "https://base-mainnet.example/v1/KEY",
  "native": { "symbol": "ETH", "decimals": 18,
              "wrapped": "0x4200000000000000000000000000000000000006" },
  "confirmations": 8,          // optional; profile default otherwise
  "discovery": { "enabled": true, "lookback": 5000 },
  "start": true
}
```

The handler dials, reads `eth_chainId` and refuses a mismatch (same check
`main.go` does today — a node pointed at the wrong network produces a silently wrong
index), probes the history floor, persists, and starts. It responds with the chain
row, URL redacted, `trust: "unverified"`.

## 3. Adding a network by name

Everything above still asks the operator for a chain id. That is the worst part of
the form: a chain id is a number nobody remembers, typing it wrong is silent, and
being wrong about it means an index keyed to a network that is not the one being
read. ENS's [`on.eth` chain registry](https://ens.domains/blog/post/on-eth-chain-registry)
removes it. You type `base`; the number comes from the chain.

### How it resolves

Measured against mainnet rather than taken from the announcement:

- `on.eth` resolves to a chain resolver at
  **`0x2a9B5787207863cf2d63d20172ed1F7bB2c9487A`** on Ethereum mainnet, owned by
  `0xFe89cc7aBB2C4183683ab71653C4cdc9B02D44b7` — the delegated multisig the ENS post
  describes, which hands each name to its chain's operators over time.
- `base.on.eth` has **no resolver of its own** in the ENS registry. Resolution is
  ENSIP-10 wildcard: `supportsInterface(0x9061b923)` is true on the chain resolver,
  and the call is `resolve(dnsEncode("base.on.eth"), innerCalldata)`.
- The inner call is ENSIP-24 `data(bytes32,string)` (`0xecbfada3`) with the key
  **`interoperable-address`**.
- What comes back is an ERC-7930 v1 *chain-only* interoperable address — an
  interoperable address with the address part left empty, which is exactly "a chain,
  named":

  ```
  0001 | 0000 | 02 | 2105 | 00
  ^ver   ^EVM   ^len  ^8453  ^addrLen = 0
  ```

| name | record | chain id |
| --- | --- | --- |
| `ethereum.on.eth` | `0001000001` **`01`** `00` | 1 |
| `optimism.on.eth` | `0001000001` **`0a`** `00` | 10 |
| `base.on.eth` | `0001000002` **`2105`** `00` | 8453 |
| `arbitrum.on.eth` | `0001000002` **`a4b1`** `00` | 42161 |
| `arc.on.eth` | *(empty bytes)* | not registered |

Three properties of that make it worth building on, and all three were checked, not
assumed:

**It is onchain.** No `OffchainLookup`, no gateway, no HTTP. One `eth_call` at head,
which means the invariant survives untouched: still two node operations, still no
historical state, still nothing outside the node in the read path. (We do have an
ERC-3668 client in `internal/ccip` if a future chain's record goes offchain, but
today it is not needed and should not be wired in speculatively.)

**An unknown name is a cheap, quiet no.** `arc.on.eth` returns zero-length bytes
rather than reverting, so "is this chain registered" costs one call and needs no
error handling worth the name.

**It carries identity and nothing operational.** `text(node,"url")` gives
`https://www.base.org/` and `text(node,"avatar")` gives an IPFS icon — enough for a
confirmation card. There is no `rpc` record, no native-currency record, no explorer
record; all of those came back empty. That is the right shape, and it decides the
division of labour below.

### Who supplies what

| | source | why there |
| --- | --- | --- |
| chain id, canonical name, icon, website | **ENS**, at `<label>.on.eth` | it is identity, and identity is what a name registry is for |
| RPC endpoint | **the operator** | the URL is where the API key lives, and a registry has no business holding it |
| confirmations, windows, native asset | **`chainprofile`** (§4), then the operator | tuning, which depends on block time and on the provider, not on the name |

So ENS is the *entry point* to chain onboarding, not a replacement for it. It is
worth being blunt that this is a feature: a public registry that shipped RPC URLs
would be a public registry of endpoints to rate-limit, and one that shipped
confirmations would be wrong for half its consumers.

### The flow

```
type "base"
  → GET /v1/chains/resolve?name=base
      resolve base.on.eth on mainnet → 8453, "Base", base.org, ipfs://…
  → card: is this the chain you meant?
  → operator pastes their own RPC URL
  → dial it, read eth_chainId, REFUSE unless it is 8453
  → chainprofile fills confirmations, windows, native asset
  → probe the history floor, persist, start indexing
```

The `eth_chainId` cross-check is the point of the whole exercise. `main.go` already
performs it for config chains, because "a node pointed at the wrong network would
silently produce a wrong index"; what changes is that it now has an *independent*
second opinion to check against, rather than checking the node against the same
number the operator typed. A typo and a misconfigured endpoint both fail here, and
they fail before anything is written.

### Manual entry is the other first-class path

`arc.on.eth` is not registered, and neither is most of what anyone wants to index on
the day they want to index it — the registry is new and testnets are not its early
population. A design that treated "not in ENS" as an error state would be unusable
for exactly the chains this feature exists to reach.

So a name that does not resolve is not a failure: it clears the form and asks for
what ENS would have supplied. Chain id, RPC URL, native symbol and decimals, entered
by hand, and everything downstream is identical — same dial, same `eth_chainId`
check (against the typed id now), same profile lookup, same start. The only
difference in the stored row is that `ens_name` is null.

### `internal/ens`

A small package, hand-written like the rest of the ABI work in this repo
(`internal/token/token.go`, `internal/lens/wire.go`) — this is 80 lines and does not
justify a dependency:

```go
func Namehash(name string) common.Hash    // ENSIP-1
func DNSEncode(name string) []byte        // ENSIP-10 wire format

// ChainID resolves <label>.on.eth. found is false when the label is not
// registered, which the resolver signals with empty bytes rather than a revert.
func ChainID(ctx context.Context, src chain.Source, label string) (id uint64, found bool, err error)

// Card reads text(url) and text(avatar) for display. Best-effort in the same way
// token.Probe is: a missing record is a blank field, not an error.
func Card(ctx context.Context, src chain.Source, label string) Card
```

The ERC-7930 decode is a dozen lines and should be strict about all three of
`version == 0x0001`, `chainType == 0x0000` and `addressLength == 0`. A record that
is not a chain-only EVM address is not something to interpret generously; it is
something to refuse, because the failure mode of guessing is an index under the
wrong chain id.

The resolver address is a constant with the same posture as
`internal/price/defaults.go`: a well-known deployment, overridable in config, and
echoed in every response that used it so a caller can check it against the one they
trust. If the ENS DAO moves the chain resolver, that is a one-line change and a
config override in the meantime.

### Which node does the resolving

The registry lives on mainnet. A deployment that indexes mainnet — the topology this
is aimed at, with mainnet indexed and the `HintRegistry` over on Base — already has
`Sources[1]`, so resolution costs one `eth_call` on a node we already run and
nothing else.

A deployment that does not index mainnet (the Helios/Sepolia one in docs/RAILWAY.md)
needs an endpoint for this and only this. An optional top-level `ens.node` covers
it; resolution is read-only, at head, and one call per onboarding, so a public RPC is
an entirely reasonable thing to point it at. With neither, `/v1/chains/resolve`
returns 501 and the UI goes straight to manual entry.

`web/index.html` already resolves registry addresses "against a public **mainnet**
RPC in your browser, because the registries live on mainnet whichever chain you are
indexing" — the same fallback applies here, but it stays a fallback. The daemon
endpoint is the primary path, because the daemon is what must be convinced of the
chain id, and a browser telling it the answer would defeat the cross-check.

### API

```
GET /v1/chains/resolve?name=base
{
  "label": "base",
  "ens_name": "base.on.eth",
  "chain_id": 8453,
  "url": "https://www.base.org/",
  "avatar": "ipfs://bafkrei…",
  "resolver": "0x2a9B5787207863cf2d63d20172ed1F7bB2c9487A",
  "known_profile": true,        // chainprofile has tuning for 8453
  "already_indexed": false
}
```

A read, so it is not behind the token — it spends one `eth_call` and tells the
caller nothing they could not read from mainnet themselves. `name` accepts either a
bare label or the full `base.on.eth`. An unregistered name returns 404 with the
label echoed, which is what the UI switches to the manual form on.

`POST /v1/chains` then accepts `ens_name` in place of `chain_id`:

```jsonc
POST /v1/chains
{ "ens_name": "base", "node": "https://base-mainnet.example/v1/KEY" }
```

and resolves, dials, cross-checks and starts. Supplying both is allowed and they
must agree; supplying neither is the error.

### The UI

One panel in `web/index.html`, in the same voice as the rest of it: a name box, a
resolved card (icon, name, chain id, link out), an RPC field carrying the same
warning the existing `#chain-rpc` row already carries about pointing at a third
party, and an Add button. `POST /v1/chains` is auth-gated, so the panel reuses
whatever token input the promote and publish controls already use rather than
introducing a second one. A name that does not resolve swaps the card for the manual
fields in place — same panel, same button, no dead end.

### Not yet: reverse lookup

Going the other way — chain id 8453 → `base.on.eth` — would let §8's demand table
name the chains people have paid for rather than listing bare numbers. Whether the
chain resolver supports reverse resolution is **unverified**; the
`unruggable-labs/ens-7828-resolver` sources are where to check before assuming
either way. Until someone does, `chain_demand` shows ids.

### Standards this leans on

[ENSIP-1](https://docs.ens.domains/ensip/1/) namehash ·
[ENSIP-10](https://docs.ens.domains/ensip/10/) wildcard resolution ·
[ENSIP-23](https://docs.ens.domains/ensip/23/) Universal Resolver
(`0xeEeEEEeE14D718C2B47D9923Deab1335E144EeEe`, the front door a client that is not
already holding the chain resolver's address should use) ·
[ENSIP-24](https://docs.ens.domains/ensip/24/) data records ·
[ERC-7930](https://eips.ethereum.org/EIPS/eip-7930) interoperable addresses ·
[ERC-7828](https://eips.ethereum.org/EIPS/eip-7828) `name@chain` syntax ·
[ERC-7785](https://eips.ethereum.org/EIPS/eip-7785), which is where this goes next:
onchain chain identifiers derived from names, at which point the name is the
identifier rather than a lookup for one.

## 4. Chain profiles, so an unknown network is not misconfigured

Block time varies sixfold across the chains people actually want — 12s on mainnet, 2s
on Base — and `eth_getLogs` range caps are provider-specific. A Base chain that
inherits mainnet's confirmation depth and window sizes is not slightly wrong, it is
wrong by a factor of six in both latency and cost.

`internal/chainprofile` mirrors what `internal/price/defaults.go` already does for
oracles, for the rest of the parameters:

```go
type Profile struct {
    Name           string
    NativeSymbol   string
    NativeDecimals int16
    WrappedNative  common.Address
    BlockTime      time.Duration
    Confirmations  uint64        // ~2 minutes of blocks, rounded up
    BackfillWindow uint64
    TailWindow     uint64
    LogRangeCap    uint64        // 0 = let SweepLogs adapt from scratch
}

func Lookup(chainID uint64) (Profile, bool)
```

Seeded for 1, 10, 8453, 42161, 11155111 and the dev chain, from the numbers already
measured in the Sepolia and Railway deployments. Resolution order for any field:
**request body → config → built-in profile → conservative default**. The conservative
default assumes a 2s chain (short windows, deep confirmations) because being slow and
right is recoverable and being fast and wrong is not.

An unknown chain id with no profile is allowed, but the response says
`"profile": "generic"` so the operator knows the windows are guesses. `SweepLogs` is
already adaptive, so a wrong `LogRangeCap` costs a few retries, not a failure.

## 5. Native asset becomes explicit

Today the native asset exists only implicitly, as the `wrapped_native: true` quote
token in the pricing config. That is enough to price it and not enough to *name* it:
the portfolio endpoint reports a native balance with no symbol and no decimals, and
on a chain whose native asset is not ETH it is simply mislabeled.

`native_symbol` / `native_decimals` / `wrapped_native` on the chain row, filled from
the profile, consumed by:

- `portfolio.accountStateJSON` — the native balance gets a symbol and decimals.
- `newPricer` — `wrapped_native` seeds the quote token list rather than requiring the
  operator to repeat it.
- The asset-group resolver below, which needs to know that WETH on chain A and WETH on
  chain B are the same *kind* of thing without pretending they are the same token.

## 6. Cross-chain asset identity

### Model — `migrations/0007_asset_groups.sql`

```sql
CREATE TABLE asset_groups (
    id          BIGSERIAL PRIMARY KEY,
    symbol      TEXT,             -- display only
    name        TEXT,
    decimals    SMALLINT,
    -- The mainnet (or lowest-chain-id) member, when one is known. A bridged token
    -- names its origin, so most groups have a real anchor rather than a synthetic id.
    origin_chain_id BIGINT,
    origin_address  BYTEA,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE asset_group_members (
    group_id   BIGINT NOT NULL REFERENCES asset_groups (id) ON DELETE CASCADE,
    chain_id   BIGINT NOT NULL,
    address    BYTEA  NOT NULL,
    link_kind  TEXT   NOT NULL,   -- curated|bridge|same_address|symbol
    evidence   TEXT,              -- the selector that answered, or the seed's name
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, address),
    UNIQUE (group_id, chain_id)   -- one member per chain; the strongest link wins
);

CREATE INDEX asset_group_members_group_idx ON asset_group_members (group_id);
```

Membership is `(chain_id, address)`-unique, so a token belongs to exactly one group,
and `(group_id, chain_id)`-unique, so a group has at most one representative per
chain. A stronger link kind displaces a weaker one; a weaker one never displaces a
stronger.

Note what this table does *not* touch: `interactions`, `epochs`, `epoch_leaves`,
`merkle`. Leaves stay `keccak256(account, chainId, assetsHash)` byte-for-byte.
Grouping is a read-side convenience over per-chain truth, which is the only way to
add it without a new `HintRegistry`.

### Link kinds, strongest first

**`curated`** — a seeded table of well-known tokens, chain by chain. This is cheap and
already half-written: `internal/price/defaults.go` lists WETH and USDC (and USDT, DAI
on mainnet) for all four supported chains. Lifting those into a
`internal/assetgroup/seed.go` gives the two tokens that cover most of what anyone
means by "I hold this on another chain", for free, with no calls at all.

**`bridge`** — the interesting one, and the one that fits the project's posture:
*ask the chain*. Canonical bridged tokens name their origin, and every one of these is
a plain `eth_call` at head, so it costs one call per candidate and violates nothing:

| selector | who implements it | returns |
| --- | --- | --- |
| `remoteToken()` | OP Stack `IOptimismMintableERC20` (Base, Optimism, Mode, Zora…) | L1 token address |
| `l1Token()` | the legacy OP Stack interface | L1 token address |
| `l1Address()` | Arbitrum `StandardArbERC20` | L1 token address |
| `bridge()` / `l2Bridge()` | both, for corroboration | the bridge contract |

The answer is an address on the *origin* chain, which is why `asset_groups` anchors on
`(origin_chain_id, origin_address)`: two L2 tokens that name the same L1 address are
the same asset, transitively, without either of them knowing about the other. When the
origin chain is not one this deployment runs, the anchor is still recorded — it is a
stable key regardless.

The probes live in `internal/token` next to `Probe`, as `token.ProbeOrigin`, and are
total in the same way: a contract that does not implement them returns an error and
that is a "no", not a failure.

**`same_address`** — the same address on two chains. True for CREATE2 and
deterministic deployments, and a common enough deployment pattern to be worth
recording, but it is *inference*: an unrelated contract can occupy the same address on
another chain. Recorded, shown, and never sufficient on its own to spend anything.

**`symbol`** — matching symbol and decimals among assets already known to us. This is
the weakest possible signal and it is **display-only**. Anyone can deploy a contract
called `USDC` on any chain for a few cents. A symbol match must never trigger a
promotion, because a promotion costs a full backfill against a metered RPC — that is
a free-backfill attack with a one-line exploit. It also must never be presented
without saying how the link was made; every member row carries `link_kind` into the
API response for exactly this reason.

### When resolution runs

Never in the request path, and never as a sweep over all chains.

- On `Promote` and on `RegisterAsset`: one origin probe on the new asset (1–3
  `eth_call`s), plus a lookup against the curated seed and existing groups. This is
  already the expensive moment — a backfill is starting — so three calls are noise.
- On the mirror's pass over registry hints, same thing.
- Never for a candidate. Discovery writes counters only; that invariant is what keeps
  watching every contract affordable, and probing every candidate for `remoteToken()`
  would be a call per contract per chain, which is exactly the cost blowup the design
  exists to avoid.

### API

```
GET /v1/assets/{address}/peers?chain_id=…    the group, its members, and link kinds
GET /v1/groups?symbol=USDC                    lookup by symbol (display, marked so)
```

```jsonc
{
  "group_id": 41,
  "symbol": "USDC", "decimals": 6,
  "origin": { "chain_id": 1, "address": "0xA0b8…" },
  "members": [
    { "chain_id": 1,     "address": "0xA0b8…", "link": "curated", "indexed": true  },
    { "chain_id": 8453,  "address": "0x8335…", "link": "bridge",  "evidence": "remoteToken()",
      "indexed": true, "verified": false },
    { "chain_id": 42161, "address": "0xaf88…", "link": "curated", "indexed": false }
  ]
}
```

`indexed: false` is a real and useful state: we know the token exists there and we are
not indexing it. That is the affordance the next section spends.

## 7. The account flow, in two tiers

The user-facing ask is "if I have USDC, show me USDC everywhere". That is two
different questions wearing one sentence, and conflating them is how an indexer ends
up accidentally backfilling forty tokens on four chains.

### Tier 1 — live, free, on by default

*Do you hold this elsewhere, right now?* This needs no index at all. For an account
lookup on chain A: take the account's assets, resolve their groups, collect group
members on other enabled chains, and run the **deployless lens** on each of those
chains with that chain's member list. One `eth_call` per chain returns balances,
allowances, NFT ids and the account's native state, at head, with a block number.

That is the whole feature, and it costs one call per extra chain. It reads head state
only, adds no ingestion path, and touches `eth_getLogs` not at all.

Bounded, because an unbounded fan-out is a denial-of-service on our own RPC bill:

- at most `cross_chain.max_chains` other chains per request (default 4),
- at most `cross_chain.max_tokens_per_chain` group members per chain (default 25),
  ordered by the account's value on the origin chain,
- one shared deadline across the fan-out; chains that miss it are reported as
  `"timed_out": true` rather than dropping the response,
- results cached per `(account, chain)` for the pricer's `cache_ttl` (~one block).

Surfaced as:

```
GET /v1/accounts/{address}/portfolio?chain_id=8453&cross_chain=true
GET /v1/accounts/{address}/portfolio?chain_id=all
```

`chain_id=all` is the aggregate view: `chainOf` grows a case for it, returning the
enabled set instead of one chain, and the response becomes a per-chain map with a
`totals` block. Every chain block carries its own `as_of_block` and `verified` flag,
because "as of block N" means nothing across chains and pretending otherwise would be
the one genuinely dishonest thing this design could do.

### Tier 2 — indexed, costly, off by default

*What is my history with this elsewhere?* This needs the asset promoted on chain B,
which means a backfill, which means metered calls. It goes through the existing
`Worker.Promote` path — no second promotion mechanism — gated exactly like
`auto_promote` is:

```yaml
cross_chain:
  promote: false          # off by default, same posture as auto_promote
  min_link: bridge        # curated|bridge only; never same_address or symbol
  max_per_tick: 2
```

The trigger is an account lookup that found a Tier-1 hold on a chain where the token
is not indexed: real demand, from a real balance, on a link we verified by calling the
contract. That is a much better promotion signal than a discovery counter, and it is
the one place where the cross-chain machinery earns its own indexing budget.

`POST /v1/assets` and `POST /v1/candidates/{addr}/promote` also gain an optional
`"promote_peers": true`, so an operator can do it deliberately for one asset without
turning on the automatic path.

## 8. The registry already knows which chains people want

`hintreg.Mirror` calls `head(ctx, a.ChainID)` for every registered hint and gets
`ok == false` for a chain this deployment does not run. Today that result is dropped
on the floor. It is the most honest demand signal in the system: **someone paid
`requestIndexing` for a token on a chain we do not index**, and in the funded model
that is money sitting unclaimed.

Record it:

```sql
CREATE TABLE chain_demand (
    chain_id     BIGINT PRIMARY KEY,
    assets       BIGINT NOT NULL DEFAULT 0,
    funding_wei  NUMERIC(78,0) NOT NULL DEFAULT 0,
    first_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen    TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Served at `GET /v1/chains/demand`, and reported in `/v1/status`: "3 chains have
registered hints you do not index, worth 0.04 ETH in funding". An operator sees what
to add next, priced.

A bare chain id is a poor thing to show someone, and §3 has a name registry sitting
right there — but going that direction means *reverse* resolution, id → name, and
whether the chain resolver supports it is unverified. Check the
`unruggable-labs/ens-7828-resolver` sources before assuming either way; until
somebody does, this table shows ids, and a row the operator recognises is one they
can paste into the resolve box themselves.

Auto-adding is possible and stays off by default — it requires both a built-in
profile *and* a node URL for that chain the operator supplied in advance
(`EVMSCAN_NODE_<id>`), because there is no safe way to invent an endpoint. When both
exist, `chains.demand_auto_add: true` starts the chain as `source: 'demand'`,
`trust: 'unverified'`.

## 9. What changes, by package

| package | change |
| --- | --- |
| `internal/chainset` | **new.** The live chain set; `Start`/`Stop`/`Get`/`IDs`/`Verified` under one lock. |
| `internal/chainprofile` | **new.** Built-in per-chain parameters, the shape `price.Defaults` already has. |
| `internal/ens` | **new.** Namehash, DNS encoding, ENSIP-10 wildcard resolve, ERC-7930 decode. ~80 lines, no dependency. |
| `internal/assetgroup` | **new.** Resolver, curated seed, link ranking. |
| `internal/token` | `ProbeOrigin` — the four bridge selectors, total like `Probe`. |
| `internal/store` | `ChainProfile` CRUD (incl. `ens_name`, `trust_set_by`); `asset_groups` reads/writes; `chain_demand`. Migrations 0006, 0007. |
| `internal/config` | `Chain.Trusted`, `Chain.Native`, top-level `ens` and `cross_chain`, `chains.demand_auto_add`. |
| `internal/api` | `Deps.Chains *chainset.Set` replacing four maps; `/v1/chains*` incl. `resolve` and the trust patch; `/v1/assets/{a}/peers`, `/v1/groups`; `chain_id=all`; `verified` on every chain block. `spendsSomething` gains `POST`/`PATCH /v1/chains`. |
| `internal/hintreg` | `Mirror` reads the set instead of closures over maps and records demand; `Publisher` skips unverified chains (`ErrUntrusted`). |
| `internal/indexer` | `Promote` resolves the asset's group after registering. Otherwise untouched. |
| `cmd/evmscand` | Builds the set, loads DB chains after config chains, hands the set to everything. |
| `internal/price` | `newPricer` takes the native asset from the chain profile. |
| `web/index.html` | The "add a network" panel: name box, resolved card, RPC field, manual fallback. |

## 10. Invariants, checked

- **Discovery writes counters only.** Unchanged: group resolution runs on promotion
  and registration, never on a candidate.
- **`eth_subscribe` is a wake-up signal; all logs enter via `eth_getLogs`.**
  Unchanged: the cross-chain path is `eth_call` only and adds no ingestion.
- **Nothing requests historical state.** The lens fan-out is at head, like every other
  `eth_call` in the codebase.
- **Backfills stop at the probed floor.** Each new chain probes its own floor on
  `Start`, exactly as a config chain does.
- **Two node operations only.** `ProbeOrigin`, the lens fan-out and ENS resolution
  are all `eth_call` at head; nothing new is added. ENS is worth naming explicitly
  because a name registry *sounds* like an off-chain lookup: it is not one here.
  `on.eth` answers onchain, so chain onboarding introduces no HTTP dependency and no
  third service to be down.
- **The merkle leaf is unchanged**, so `cmd/evmscan-verify`, the Solidity verifier and
  the CCIP gateway need no changes at all.

## 11. Out of scope

- **Cross-chain commitments.** One epoch covering several chains would need a new leaf
  shape and a new `HintRegistry`. docs/TOKENOMICS.md:187 already parks this: "Same
  chain first, cross-chain later." Groups are a read-side view; the commitment stays
  per-chain.
- **Bridging or messaging.** We read what bridges wrote. We never send.
- **Non-EVM chains.** The whole index is 20-byte addresses and EVM log topics.
- **Automatic RPC discovery.** No chainlist fetch, no endpoint guessing. A URL is
  operator input, always.

## 12. Order of work

Each step is useful shipped alone, and each is a small commit.

1. `chainprofile` + native asset on the chain row. No behaviour change, fills in what
   is already missing from portfolio responses.
2. `chainset` refactor: four maps → one guarded set, still populated only from config.
   Pure refactor, `make check` is the whole test.
3. Migration 0006 + `/v1/chains` read endpoints. Still no runtime mutation.
4. `internal/ens` + `GET /v1/chains/resolve`. Read-only, testable against mainnet on
   its own, and useful before anything can be added: it answers "what chain is this
   name".
5. `POST/PATCH/DELETE /v1/chains`, trust levels, the `eth_chainId` cross-check,
   publisher skipping unverified, quarantine-on-failure for API chains.
6. The `web/index.html` panel. This is where it becomes "add a network" rather than
   "an endpoint that adds a network".
7. Migration 0007 + curated seed + `ProbeOrigin` + `/v1/assets/{a}/peers`. This is the
   "same token elsewhere" feature, with no account flow yet.
8. Tier-1 live fan-out: `cross_chain=true` and `chain_id=all`.
9. `chain_demand` recording and `/v1/chains/demand`.
10. Tier-2 cross-chain promotion, off by default.

Steps 1–6 make the daemon multichain and give it a way in. 7–8 make it *feel*
multichain. 9–10 make it pay for itself.

The target topology — mainnet indexed, the `HintRegistry` on Base, and a testnet like
Arc added from the browser — is done at step 6, and the mainnet node it already runs
is the same one that resolves the names.
