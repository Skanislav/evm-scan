# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

evm-scan is a Go daemon that answers "which contracts has this address touched?" using only a
self-hosted, snap-synced geth node. It discovers token contracts by watching the chain head,
indexes per-account interactions only for contracts that get *promoted*, and commits the
resulting `account → contracts` table on-chain as a merkle root in `HintRegistry.sol`.
README.md explains the design rationale in depth; read it before changing architecture.

## Commands

```bash
make build            # builds bin/evmscand, bin/evmscan-demo, bin/evmscan-verify, bin/evmscan-deploy, bin/evmscan-restore, bin/evmscan-ens
make check            # what CI runs: gofmt -w, go vet, go test ./...
go test ./...         # unit tests only (hermetic, no node or DB needed)
go test ./internal/merkle/ -run TestName -v      # single test
make contracts        # recompile contracts/src/*.sol -> contracts/out/*.json (needs node; installs solc@0.8.28 into scripts/)
```

Local end-to-end loop (needs PostgreSQL 16+ and a `geth` binary; see docs/DEMO.md):

```bash
make devchain         # foreground geth --dev, IPC at .devchain/geth.ipc
make demo             # deploys HintRegistry + demo tokens, generates traffic, writes config.demo.yaml
make run              # evmscand against config.demo.yaml; UI at http://127.0.0.1:8080
```

Node-backed integration tests skip unless pointed at a chain:

```bash
EVMSCAN_TEST_NODE="$PWD/.devchain/geth.ipc" EVMSCAN_TEST_TOKEN=0x... go test ./internal/token/ -run Probe -v
```

Go 1.24+. `config.demo.yaml` contains a dev-chain private key and is gitignored; never commit
a config with a publisher key. Secrets can be supplied via `EVMSCAN_DATABASE_DSN`,
`EVMSCAN_PUBLISHER_KEY`, `EVMSCAN_REGISTRY_ADDRESS`, `EVMSCAN_NODE`, `EVMSCAN_API_LISTEN`.

## Architecture

Single process (`cmd/evmscand`) running, per configured chain, an `indexer.Service` plus a
shared `hintreg.Mirror`, optional `hintreg.Publisher`, and the HTTP API. Module path is
`github.com/Skanislav/evm-scan`.

**Data flow through the packages:**

- `internal/chain` is the *only* package that talks to a node. Everything else is written
  against the `chain.Source` interface (read) and `chain.Sender` (write, in `tx.go`). Tests
  fake `Source` directly (see `prunedNode` in `chain/history_test.go`). `Dial` enforces
  `require_local_node`; `SweepLogs` is the adaptive-window `eth_getLogs` walker every scan
  uses; `HistoryFloor` binary-searches for the oldest block the node can serve logs for.
- `internal/evmlog` decodes only the five identity-carrying token events (`Transfer`,
  `Approval`, `ApprovalForAll`, `TransferSingle`, `TransferBatch`) from indexed topics. It
  never unpacks event data, so it is total. `WatchedTopics()` is the topic filter that
  bounds every node query.
- `internal/indexer` runs three goroutines per chain inside `Service.Run`:
  - **discovery** (`discovery.go`): unfiltered-by-address sweep forward from the head,
    writing only per-contract counters to `candidates`. `Promote` turns a candidate into an
    indexed asset; `auto_promote` is off by default. Demand promotes on its own:
    `PromotableCandidates` and `DemandedUnseen` read `(voters − against) >= min_voters`
    from `asset_demand_totals`, and `spam_at` wins over any number of signers.
  - **follower** (`followTick`): walks forward from each asset's anchor, handles reorgs by
    comparing stored block hashes and rewinding `pending_events`.
  - **backfiller** (`runBackfill`): walks backward from the anchor toward
    `max(hint_from_block, historyFloor)`. Assets whose walk is truncated by the floor get
    `history_complete: false`.
  - `Aggregate` in `aggregate.go` folds logs into `store.Interaction` rollup rows.
- `internal/store` is hand-written SQL over pgx (no ORM by design). Schema lives in
  `migrations/*.sql`, embedded and applied in filename order by `Store.Migrate`; new
  migrations must be named `NNNN_name.sql`. Addresses are 20-byte `BYTEA`. Key tables:
  `assets` + `asset_cursors` (two-pointer scan state), `candidates` (discovery counters),
  `interactions` (the rollup), `pending_events` (unconfirmed buffer), `epochs`/`epoch_leaves`
  (commitments).
- `internal/hintreg` bridges the on-chain registry both ways: `Mirror` pulls registered hints
  into `assets` and nudges the follower; `Publisher` builds an epoch from
  `store.SnapshotIndex`, commits the merkle root via `publishIndex`, finalizes its own
  epochs after the challenge window (`FinalizeDue`), and `ProofFor` serves inclusion
  proofs. Transactions go through the `Submitter` interface (`submitter.go`); `EOASubmitter`
  is the only implementation. A submission reference is persisted (`epochs.submission_ref`,
  status `submitted`) before the receipt wait, and `ResumePending` settles it after a
  restart, so nothing is ever posted twice. `Build` refuses an unchanged root unless forced.
- `HintRegistry` adjudicates disputes in one of two modes fixed at construction, read via
  `Client.Mode`. **Oracle mode** (UMA Optimistic Oracle V3, `contracts/src/IOptimisticOracleV3.sol`,
  `MockOptimisticOracleV3.sol` for tests): `publishIndex` asserts the commitment, the bond is
  an ERC-20 the publisher must approve (`Publisher.ensureBondAllowance`), `finalizeIndex`
  settles the assertion, and the arbiter is the zero address so `resolveChallenge` and
  `setGateways` revert forever. **Local-arbiter mode**: wei bonds, one key resolves
  challenges and may edit the gateway list. That key is the OpenZeppelin
  `Ownable2Step` owner (`arbiter()` is a view over `owner()`, zero in oracle mode), so
  a leaked arbiter is rotated with `transferOwnership` then `acceptOwnership` from the
  new key — `evmscan-deploy -transfer-owner` / `-accept-owner` — rather than by
  redeploying; the compile script pulls `@openzeppelin/contracts` (pinned in
  `scripts/package.json`) into the standard input so verification stays complete. In
  both modes bonds, window and pricing are `immutable` constructor arguments with no
  setters; the constructor takes an `Economics` tuple plus the initial gateway list,
  packed with `hintreg.ConstructorArgs`. Changing the rules means a new deployment;
  changing the key does not.
  `registry_integration_test.go` (needs a dev node) covers the oracle path.
- `HintRegistry.requestIndexing` is a paid registration whose surplus becomes the asset's
  funding (`getFunding`). An epoch commits a `coverageRoot` over per-asset
  `(assetKey, fromBlock, toBlock)` leaves built from `asset_cursors`
  (`hintreg.CoverageFromCursors`); after `finalizeIndex` returns the bond, `claimCoverage`
  pays `rewardPerBlock` per block of a funded asset's range not paid for before
  (`Publisher.ClaimDue`). `Build` quotes the coverage with `claimable` and refuses to
  store an epoch below `Publisher.MinReward` (`ErrUnfunded`). That is how gas is
  sponsored: the EOA fronts it and the people who funded the assets pay it back.
  `merkle.CoverageLeaf` must match `HintRegistry.coverageLeaf` byte-for-byte; the
  hermetic `registry_sim_test.go` runs the whole loop on go-ethereum's simulated backend.
  docs/TOKENOMICS.md is the full design; only its minimum is built.
- `internal/chainset` is the live set of running chains — source, worker, pricer per
  chain, behind an RWMutex, in config order. Everything that used to range a map of
  chains reads through it, because the set changes while the process is up. It does
  no construction on purpose: that lives in `cmd/evmscand/chains.go` (`supervisor`),
  which is what keeps `internal/api`'s dependency on the indexer down to the `Worker`
  interface. `supervisor.StartStored` is what `POST /v1/chains` calls.
- A network can be added at runtime: `POST /v1/chains` dials the operator's RPC,
  cross-checks `eth_chainId` against the id asked for, persists to `chains`
  (migration 0006) and starts indexing. `internal/ens` resolves `<label>.on.eth`
  through ENS's chain registry on mainnet — ENSIP-10 wildcard `resolve` wrapping an
  ENSIP-24 `data()` record holding an ERC-7930 chain-only address — so a network can
  be named rather than numbered. It is on-chain, one `eth_call` at head, no gateway.
  The registry carries identity only; the RPC endpoint is always operator-supplied.
  The same package holds the hint-name scheme `<hex>.hints.<parent>` (`HintName`,
  `ParseHintName`), `Normalize` (NFC + lowercase, not ENSIP-15; the page and the mirror
  apply the identical transform), and the ENSv2 registry/factory ABIs `cmd/evmscan-ens`
  needs. It does **not** resolve account names: those are resolved in the client, and
  the API takes addresses only.
  `internal/chainprofile` holds the per-chain tuning ENS does not carry (block time,
  confirmations, log windows, native asset), since a confirmation depth copied
  between chains means a different amount of wall clock on each.
- `chains.trust` decides whether a chain's data may back a commitment. Loopback and
  verifying light clients are `verified`; a third-party RPC is `unverified` and
  `Publisher.Build` refuses it with `ErrUntrusted` — a bond is money staked on logs
  we did not verify. An operator may promote a chain with
  `PATCH /v1/chains/{id} {"trust":"verified"}`, which is behind `EVMSCAN_API_TOKEN`
  and recorded in `trust_set_at`/`trust_set_by`. Resolving a chain's ENS name proves
  a name maps to a chain id and says nothing about who serves the logs; it must never
  raise trust.
- docs/MULTICHAIN.md is the full design. Steps 1–6 of its §12 are built; cross-chain
  asset identity (`asset_groups`, the bridge-selector probe, the lens fan-out) and the
  registry demand table are designed and not built.
- `internal/merkle` must match `HintRegistry.leafHash` / `verifyInclusion` byte-for-byte:
  leaf = `keccak256(abi.encode(account, chainId, keccak256(abi.encodePacked(sorted unique
  assets))))`, sorted-pair keccak tree. Changing either side requires changing the other and
  re-running `cmd/evmscan-verify`, which checks the Go proof against the Solidity verifier.
- `internal/hintfilter` is the `.xorf` membership filter: a binary-fuse8, sorted-u64
  or bloom table over 64-bit keys, published so a reader can narrow a portfolio read
  locally instead of naming their address to the API. Keys are
  `uint64be(keccak256(subkey ‖ parts)[0:8])` with
  `subkey = keccak256(secret ‖ "evmscan/xorf/v1" ‖ chainId ‖ kind)`; a non-empty secret
  blinds the file so only its holder can test it. `KindInterop` keys an **ERC-7930**
  interoperable address instead of a bare one, putting the chain inside the preimage
  so a single filter spans every chain — its subkey binds `chainId` 0, because binding
  a chain there as well would answer for one chain through keys that claim all of
  them, and a subkey mismatch presents as an empty wallet rather than an error.
  `StructureBloom` exists for the size the other two are bad at: at 65 keys fuse8's
  fixed segment geometry costs 198 bytes and sorted-u64 costs 562, where a bloom is
  0.12% in 128 bytes — small enough to publish on-chain or as an ENS text record.
  `HintBits` is 1024 and **not** 256: one storage slot is the obvious size and the
  wrong one, measuring 9.1% false positives at fifty tokens against 0.05% at 128
  bytes, and on Base the difference between writing one word and four is a tenth of a
  cent. Past 128 the extra bytes buy decimal places rather than round trips, since
  once false positives fall below the real holdings the holdings decide the batch
  count; the headroom that is left absorbs incremental `|=` additions before a
  rebuild. Its bitmap is MSB-first bytes, not packed words, so the JavaScript reader
  indexes it without reproducing Go's word endianness. `testdata/public-bloom-interop.xorf`
  (Go-written, 256 bits) pins the JavaScript reader; `testdata/browser-slot-interop.xorf`
  (1024 bits) was written by a browser bloom builder that no longer exists — the
  reader's own wallet is committed as an enumerable list now, see below — and stays as
  a reader fixture only. **The Go writer and the JavaScript
  reader in `web/index.html` must agree byte-for-byte** — `internal/hintfilter/testdata`
  is the fixture that enforces it, regenerated with `go test ./internal/hintfilter
  -update`, and `testdata/browser-watch.xorf` (WebAuthn prf) plus
  `testdata/browser-watch-pbkdf2.xorf` (password) pin the reverse direction (the browser
  builds blinded watchlists; only Go builds fuse filters). A watchlist's header carries a
  `SaltDesc` saying how to re-derive its secret — which provider, which credential, which
  salt, and for PBKDF2 the iteration count — but never the secret, which is what lets a
  file be opened months later. Construction is
  deterministic, so a published filter can be rebuilt and diffed. Built by
  `cmd/evmscan-hint`, served at `GET /v1/hints`, `GET /v1/hints/{name}.xorf` and
  `.json`. docs/PRIVACY.md is the threat model. **Status:** the routes, the writer and
  the fixtures are live; the private lookup and the watchlist builder that read them
  are in `web/hints.js` behind no UI since the 2026-09-13 ship (docs/SHIP.md §3, §8).
- `internal/token` probes metadata and balances via `eth_call` **at head only**. Nothing in
  the codebase may request historical state; that is what keeps snap sync sufficient.
- `internal/lens` runs `contracts/src/AssetLens.sol` as a *deployless* `eth_call` (creation
  code plus request as calldata) to read an account's whole position, balances, allowances,
  NFT ids, nonce and 7702 delegation, in one call at head; served at
  `/v1/accounts/{addr}/portfolio` (`internal/api/portfolio.go`). `contracts/evmtest` is a
  separate Go module that executes the lens in a real EVM (`make test-evm`), kept apart so
  go-ethereum's in-process node stays out of the daemon's dependency graph.
- `contracts/src/HintResolver.sol` is "hints out": an ENSIP-10 wildcard resolver bound to
  one HintRegistry that serves the committed index as ENS text records under
  `<hex>.hints.<parent>` (docs/ENS.md). Its `evmscan.contracts` record reverts
  `OffchainLookup` at the registry's gateways and the callback delegates to
  `HintRegistry.contractsOfCallback`, so any ENS client (viem, ens-cli, the app) reads the
  index verified against the latest finalized root with no evm-scan code. `cmd/evmscan-ens`
  deploys it and attaches it under an ENSv2 name; `evmscan-verify -ens` reads it back and
  checks it against `contractsOf`; `ens_sim_test.go` runs the loop on the simulated backend.
  `internal/ccip.Resolve` packs the callback by the selector the revert named, so it serves
  both contracts; the gateway does not check `sender`, because ENS's Universal Resolver
  rewrites it and the answer is verified where it is used. `registry.ens_parent` only
  lets account responses carry `hint_name`. **Status:** designed for the ENSv2 Sepolia
  beta, sim-tested, no deployment and no addresses; off the ship as a future
  exploration of custom resolvers (docs/SHIP.md D2, docs/ENS.md).
- `contracts/src/HintSignedResolver.sol` is the same resolver for a chain the registry is
  **not** on: ENS names live on mainnet, the registry on Base, and a mainnet resolver
  cannot verify a Base root. It pins a `signer` (the publisher key, rotatable by its
  `Ownable2Step` owner) and answers `addr`, `evmscan.registry`, `evmscan.chain` and
  `evmscan.signer` itself; every other text key reverts `OffchainLookup` with the full
  `resolve(name,data)` calldata, and `resolveWithProof` recovers the gateway's
  signature over `keccak256(0x1900 ‖ resolver ‖ expires ‖ keccak(request) ‖
  keccak(result))` and checks the expiry. `internal/api/ensgateway.go` (`GET
  /ens/{sender}/{data}`, `POST /ens`) builds the records from the same finalized leaf
  `/ccip` proves and signs them with `Deps.Signer` (`hintreg.Signer`, the
  `EOASubmitter`'s key), refusing any sender but `registry.ens_resolver` once that is
  set. **Signer-trust, not root-verified**: a stolen publisher key forges these
  records where a proof would not, and `read.html` (off the nav) and `evmscan-ens
  check` say which path answered. `internal/ccip/signed.go` is the codec;
  `ens_signed_sim_test.go` runs the loop on the simulated backend. `evmscan-ens
  deploy-signed-resolver` deploys it; the subnode is created from the name owner's
  wallet (docs/MAINNET.md §6d). **Status:** built, never deployed; `hints.evm-scan.eth`
  does not resolve today and the deployment is off the ship (docs/SHIP.md D2).
- An account's cross-chain **hint** is a `KindInterop` bloom over its `(chain, token)`
  pairs, stored in `account_hints` (migration 0011) only under the account's EIP-712
  signature (`Hint(account, digest, deadline)` under the domain `{evm-scan hint, 1}`,
  no chain, no contract; `internal/api/hintsig.go`), served at
  `GET /v1/accounts/{addr}/hint` and as the `evmscan.hint` record; without a stored
  one the daemon builds a bloom from the account's committed contracts. A hint
  **orders** a sweep and **removes nothing**. **Status:** the routes stay; the
  browser-built, signed hint card is off the page since the 2026-09-13 ship, so in
  practice only the daemon-built bloom is served. The reader's signed act is now the
  verdict (invariants below), whose EIP-712 shape is a copy of `Hint`'s.
- `internal/api` depends on the indexer through the small `Worker` interface, not the package.
  `gateway.go` is the ERC-3668 gateway for `HintRegistry.contractsOf`; `internal/ccip` holds
  the response codec and an ERC-3668 client shared with `cmd/evmscan-verify -ccip`. The
  callback only accepts the latest finalized epoch, so the gateway reads that id from the
  registry, not from the local table. Each contract in `GET /v1/accounts/{addr}` and
  `/contracts` carries `committed: bool` (in the latest finalized epoch's leaf for
  this account, from `epoch_leaves`) and `demand: {"for": n, "against": n}`.
  Routes use Go 1.22 method-prefixed patterns on `http.ServeMux`. `authorized` guards
  everything that is not a read, minus the allowlisted exceptions (`POST /ccip` and
  `POST /ens`, which any ERC-3668 resolver has to reach; `POST /v1/verdict`, a
  reader's signed verdict; `/v1/demand/relay`, a vote the signer already authorised;
  `POST /v1/accounts/{addr}/hint`, written under the reader's own signature) —
  an inverted rule, so a new mutating route is guarded before anyone remembers to add
  it, and `auth_test.go` is what holds the exceptions open.
- Money buys indexing and does not buy position. `HintRegistry.Funding` keeps
  `vouched` beside `balance`: `balance` drains as `claimCoverage` pays the publisher,
  so a well-funded, well-indexed asset reads as zero there — the same as one nobody
  ever wanted — which makes it useless for ranking. `vouched` only ever rises.
  `orderAssets` in `internal/api/handlers.go` and the wallet render in
  `web/index.html` apply one rule (docs/SHIP.md §4): (1) any operator report, or
  `against > for` among signed verdicts, sinks below every other row regardless of
  the amount, because the registry is open and otherwise the cheapest attack is to
  buy the top of somebody's wallet; (2) contracts `committed` in the latest
  finalized epoch's leaf for this account come first; (3) then `for − against`
  descending; (4) then `vouched` descending, then activity. Reports live on
  `assets` (migration 0009), not on `candidates`: a contract someone paid to
  register never passes through discovery, so no operator decision on a candidate
  can reach it. One report — or one signer's `against` with nobody `for` — is
  enough because ordering is not adjudication: deranking a good contract costs it
  a place and a reader one extra balance read, while ranking a scam puts it at the
  top of a wallet, and those are not the same mistake. A report can never revoke,
  un-index or refund: the funding already bought a backfill and the coverage is
  already in a root.
- A candidate carries at most one live **operator decision** (not to be confused
  with a reader's verdict, `POST /v1/verdict`). `spam_at` drops it out of
  `PromotableCandidates` — the only candidate query auto-promote reads, so that one
  clause is the whole rule, and `DemandedUnseen` excludes candidates entirely — and
  promotion clears the mark rather than sitting beside it, which is what
  lets `/v1/decisions` be a single ordered scan over `COALESCE(promoted_at, spam_at)`.
  Discovery keeps counting a spam contract; a decision is about what to index, not
  what to watch.
- `web/` is the UI, served by `http.FileServer` from `WebDir` — no build step, no bundler.
  `index.html` is one page of five tabs (wallet, overview, triage, accounts, graph).
  The wallet tab is the reader flow of docs/SHIP.md §2 — enter, read, classify (the
  signal weights live in one object at the top of the page), verify, *Sign my
  verdict*, remember — and the four operator tabs are untouched (D5); the
  cross-chain sweep is one secondary card off the main path (D3).
  `graph.js` is the WebGL graph, imported the first time that tab is opened because
  three.js is most of a megabyte and most visits never ask for a picture. `hints.js` is
  imported the same way and for the same reason — the index filter is well over a
  megabyte — and holds the two things that read one: the private lookup, which answers
  "which indexed contracts has this account touched" from the downloaded file so the
  daemon never learns the address, and the blinded-watchlist builder — both in code
  behind no UI since the ship (§3) — plus the browser's memory of an account and the
  legacy unsigned vote (below). It cannot close
  over this file's scope, so the filter primitives are handed to it on
  `window.evmscanHints`; there is deliberately only one implementation of the
  arithmetic on the page, because a second one would be a second thing to keep
  byte-identical with Go. `ensrec.js` is the ENS record codec — the list format
  `HintResolver` serves, the `text`/`resolve` ABI by hand, a Universal Resolver read
  that reports `OffchainLookup` rather than following it — imported by `hints.js`
  and by `read.js`, so the format has one implementation. `read.html` + `read.js` is
  the **separate reader flow**, off the nav since no resolver serves the name
  (docs/SHIP.md D2) and kept as the reference reader: an account or name in, the registry's
  `<hex>.hints.<parent>` name, its `evmscan.contracts` record read through ENS on the
  reader's RPC (the resolver's `OffchainLookup` is followed in the browser by hand so
  the gateway is named on screen, and the callback verifies the answer against the
  latest finalized root), balances from a deployless `AssetLens` call on another
  RPC; the only request to this origin is `GET /v1/lens`, which never carries an
  account. Token metadata is attacker-controlled text from the chain, so everything
  interpolated into markup goes through `esc()`.
- `mirror/` is a separate TypeScript package (`make test-mirror`, own `node_modules`, not
  in the Go build): the commitment encoding ported for clients, plus a local-first mirror
  that keeps the committed rows in the client's SQLite via Evolu and rebuilds the keccak
  root to check them against `latestFinalizedEpoch`. Rows are immutable
  `(account, sinceEpoch)` versions — `sinceEpoch` is the **on-chain** epoch id — so a
  client mid-sync can still resolve and verify an older epoch. `mirror/testdata` is
  generated by Go (`go test ./internal/snapshot/ -run Fixtures -update-fixtures` for the
  leaf/root vectors, `go test ./internal/hintreg/ -run MirrorFixtures -update-fixtures`
  for the ABI blobs); both tests assert the files are current under plain `go test`, so
  changing an encoding on the Go side fails there. Nothing in the daemon depends on any of
  this, and `internal/merkle` remains the reference implementation. docs/LOCALFIRST.md is
  the design, including what is not verified (Evolu itself needs Node ≥ 24.20.0).
- `contracts/` embeds compiled artifacts from `contracts/out/` so `go build` needs no Node
  toolchain. After editing a `.sol`, run `make contracts` and commit the regenerated JSON.
- `Dockerfile` + `deploy/entrypoint.sh` run evmscand next to a Helios light client on
  loopback (Sepolia). Helios verifies logs against receipts roots and calls via
  `eth_getProof`, so `require_local_node` stays on. Its verifiable history is ~8191 blocks
  (EIP-2935) and `eth_getLogs` is capped at 4096 blocks; `deploy/config.railway.yaml` is
  sized for that. See docs/RAILWAY.md.

**Invariants to preserve when editing:**

- Discovery writes counters only, never per-account rows. Per-account indexing and history
  reads are reserved for promoted assets.
- The `eth_subscribe` stream is a wake-up signal only; all logs enter the index through
  `eth_getLogs`. Do not add a second ingestion path.
- Events shallower than `confirmations` stay in `pending_events`; the `interactions` rollup
  is only ever written from confirmed blocks.
- Backfills stop at the probed history floor, never assume genesis is reachable.
- Two node operations only: `eth_getLogs` and `eth_call` at head.
- Account names resolve in the client: the page through the Universal Resolver on the
  reader's RPC, the mirror through its injected `EthCall`. The daemon never resolves a
  name, no API parameter takes one, and nobody here follows an ERC-3668 gateway on a
  reader's behalf — the one exception is `read.html`'s registry mode (in code, off
  the nav), which follows the resolver's `OffchainLookup` in the reader's own browser
  with the gateway named on screen; that is the reader following it, not us.
- A reader's act on their own wallet is **one signed verdict: not a list, not a
  transaction, not a vote button** (docs/SHIP.md D1, §4). The page proposes a split
  of the holdings into recognized and unrecognized from signals the API already
  carries — `price.confidence`, `known`, `vouched_wei`, `demand`, `roles`,
  `reports`, a symbol lookalike — the reader flips what it got wrong and signs once:
  EIP-712 `Verdict(address account, uint64 chainId, bytes32 digest, uint256 deadline)`
  under the domain `{name: "evm-scan verdict", version: "1"}`, no chain, no
  verifying contract (same reasoning as `Hint`); `digest = keccak256(concat over
  pairs sorted by address of (address ‖ int8 weight))`, weight ∈ {−1, +1}, a 0 is not
  sent and absence is 0. `POST /v1/verdict` (`internal/api/verdict.go`,
  `verdictsig.go`) recovers the signer, which must equal `account`, requires
  `deadline` in the future and strictly greater than the one stored for
  `(chain, voter)` in `account_verdicts` (migration 0012) — that is the replay guard
  — and **replaces** the voter's rows for that chain: rows not in the list are
  deleted, `weight` upserted for the rest, response `{recorded, cleared,
  indexed_here}`. Go and the page compute the digest independently and
  `internal/api/testdata/verdict.json` pins both. The voter is stored as
  `keccak256(salt ‖ chainId ‖ account)` under the salt generated once in migration
  0010, so an account counts once and the table cannot be walked back to who holds
  what. `POST /v1/verdict` is allowlisted in `guarded()` beside `/v1/demand/relay` and
  `/ccip`, and `auth_test.go` holds it open. `POST /v1/demand`, the unsigned +1
  (`vote` in `web/hints.js`, curl), is behind the operator token since the verdict
  replaced it on the page — at `min_voters: 1` an open one buys a backfill per curl; `GET /v1/demand` reports `voters` and
  `against` per contract and `indexed_here` per chain, and a verdict may name a
  chain this deployment does not run — demand for an unindexed chain is what tells
  an operator which chain to add, and `DemandedUnseen` promotes it as soon as that
  chain runs. Demand is a priority signal and nothing else: `PromotableCandidates`
  orders by it and promotes on `(voters − against) >= min_voters` alone
  (`DemandedUnseen` reaches a contract discovery never counted, promoted with source
  `demand`); `spam_at` outranks any number of signers, promotion stays budgeted by
  `max_promotions_per_tick`, a verdict buys no position, and no verdict changes what
  a balance read says. `min_voters` is 3 on a metered node — a single fresh address
  must not buy a backfill; the live profile runs 1 for demo day (docs/SHIP.md D4).
- The on-chain counter is still there and off the page. `HintRegistry.vote` and
  `voteFor` (EIP-712 `Vote(voter, chainId, tokens, nonce, deadline)`, carried to the
  frozen contract by `POST /v1/demand/relay` with the publisher's sender,
  `Deps.Relay`: simulated first, one transaction per voter **and chain** per ten
  minutes, at most twenty tokens, refusing a vote that counts nothing new,
  remembering consumed nonces, waiting for the receipt because the next chain's
  vote cannot be signed against anything but the mined state; `GET
  /v1/demand/relay?voter=` hands out domain, types and nonce and reports
  `available: false` on a registry that predates `voteFor`) are mirrored by
  `Mirror.syncDemand` into `asset_demand_onchain` and counted as `voters` beside the
  API rows, so the same account through both paths counts twice. No route is
  removed and the page does not offer the transaction (docs/SHIP.md D1). Two earlier
  shapes were built and cut: a 1,024-bit bloom under an `evmscan.hint` text record
  (unreadable without walking the daemon's token list), then the enumerable list as
  `evmscan.contracts` on the reader's own name (measured 418,386 gas for 10
  contracts, 1,043,335 for 30, 2,131,297 for 65 on mainnet ENS — per wallet, for
  what a counter stores once). Cost any ENS write with `eth_estimateGas` against a
  real resolver, never from SSTORE arithmetic.
- What a browser remembers about an account is spent on the next lookup, and the
  shape of the spending is the invariant. The memory is
  `localStorage["evmscan.verdict.<account>"]`: the signed pairs and their deadline,
  so the next lookup is pre-split the same way before the daemon answers, and the
  reader's own stored verdict overrides the page's proposal for that row. It
  **adds**: every contract the memory names is asked about whether or not the index
  offers it, and the list read through the lens is index ∪ committed ∪ memory
  (docs/SHIP.md D3). It **orders**: recognized rows go ahead of the per-lookup cap,
  though never ahead of a committed asset. It **removes nothing**: a −1 row is still
  asked about, still read from the chain, still valued, and drawn in the lower
  group behind the same toggle that moves it back — it is stored as addresses, not
  as a filter, because an undo that cannot be enumerated is not an undo. Older keys
  (`evmscan.seen.<account>`, `evmscan.aside.<account>`) are no cache, not a broken
  one. Nothing is read from ENS on the lookup page. Every failure is a note on
  screen and an empty seed, never a lost lookup. The verdict is the reader's whole
  triage: there is no separate set-aside, no report and no hint card, and the
  valuation is untouched by any of it.
- Hint filters annotate, never filter. A token list is curated and therefore
  incomplete, so dropping what is not on one hides real holdings of long-tail tokens.
  `known` rides alongside a portfolio row and is absent — not false — when no list is
  configured; `known_only=true` is the caller's explicit opt-in.
- A filter is never trusted. It says where to look; the live lens read says what is
  there. A false positive costs one `balanceOf`; there are no false negatives except
  from staleness, which is why an index filter carries the block it is true as of.
- The cross-chain sweep in `web/index.html` is client-only and a secondary card, not
  the main path (docs/SHIP.md D3): viem's bundled chain
  registry (lazily imported — the barrel is ~800 separate requests) intersected with
  a CORS-open token list, then the same deployless `AssetLens` call per chain. The
  lens returns ~920 bytes per token against EIP-170's 24,576-byte ceiling, so 24 per
  call is a measured near-optimum rather than a conservative guess, so batching is
  not a lever and curation of the list is. **The index filter must never narrow that
  sweep**: it answers "is this pair in the index", the index is bounded by the
  promoted asset set, and a miss therefore means "not indexed" rather than "no
  balance" — narrowing by it took a real account from 65 holdings to 1. Every
  endpoint the sweep reaches sees the account, which is why the disclosure sits on
  the button and not in a footnote.
- An index filter's digest is fixed inside `Publisher.Build`, from the same
  `SnapshotIndex` call as the merkle root, and stored on `epochs.filter_keccak`. The
  bytes are never stored: they rebuild deterministically, and the serving path
  asserts the rebuilt digest against what the epoch committed rather than trusting
  either. The filter's own `epochId` header stays `-1`, because the digest has to be
  fixed before the epoch has an id — the epoch names the filter, not the reverse.
