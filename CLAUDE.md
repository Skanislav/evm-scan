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
    indexed asset; `auto_promote` is off by default.
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
  challenges and may edit the gateway list. In both modes bonds, window and pricing are
  `immutable` constructor arguments with no setters, and the arbiter cannot be reassigned;
  the constructor takes an `Economics` tuple plus the initial gateway list, packed with
  `hintreg.ConstructorArgs`. Changing the rules means a new deployment.
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
  fixed segment geometry costs 198 bytes and sorted-u64 costs 562, where a bloom hits
  0.18% in 128 bytes and **fits one EVM storage slot at 256 bits** — which is what
  makes a per-account hint publishable on-chain or as an ENS text record. Its bitmap
  is MSB-first bytes, not packed words, so the JavaScript reader indexes it without
  reproducing Go's word endianness. **The Go writer and the JavaScript
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
  `.json`. docs/PRIVACY.md is the threat model.
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
  lets account responses carry `hint_name`.
- `internal/api` depends on the indexer through the small `Worker` interface, not the package.
  `gateway.go` is the ERC-3668 gateway for `HintRegistry.contractsOf`; `internal/ccip` holds
  the response codec and an ERC-3668 client shared with `cmd/evmscan-verify -ccip`. The
  callback only accepts the latest finalized epoch, so the gateway reads that id from the
  registry, not from the local table.
  Routes use Go 1.22 method-prefixed patterns on `http.ServeMux`. `authorized` guards
  everything that is not a read, minus one allowlisted exception (`POST /ccip`, which any
  ERC-3668 resolver has to reach) — an inverted rule, so a new mutating route is guarded
  before anyone remembers to add it, and `auth_test.go` is what holds the exception open.
- Money buys indexing and does not buy position. `HintRegistry.Funding` keeps
  `vouched` beside `balance`: `balance` drains as `claimCoverage` pays the publisher,
  so a well-funded, well-indexed asset reads as zero there — the same as one nobody
  ever wanted — which makes it useless for ranking. `vouched` only ever rises.
  `orderAssets` in `internal/api` sorts by it, and any report sinks a contract below
  every unreported one regardless of the amount, because the registry is open and
  otherwise the cheapest attack is to buy the top of somebody's wallet. Reports live
  on `assets` (migration 0009), not on `candidates`: a contract someone paid to
  register never passes through discovery, so no candidate verdict can reach it. One
  report is enough because ordering is not adjudication — deranking a good contract
  costs it a place and a reader one extra balance read, while ranking a scam puts it
  at the top of a wallet, and those are not the same mistake. A report can never
  revoke, un-index or refund: the funding already bought a backfill and the coverage
  is already in a root.
- A candidate carries at most one live verdict. `spam_at` drops it out of
  `PromotableCandidates` — the only query auto-promote reads, so that one clause is the
  whole rule — and promotion clears the mark rather than sitting beside it, which is what
  lets `/v1/decisions` be a single ordered scan over `COALESCE(promoted_at, spam_at)`.
  Discovery keeps counting a spam contract; a verdict is about what to index, not what to
  watch.
- `web/` is the UI, served by `http.FileServer` from `WebDir` — no build step, no bundler.
  `index.html` is one page of five tabs (wallet, overview, triage, accounts, graph);
  `graph.js` is the WebGL graph, imported the first time that tab is opened because
  three.js is most of a megabyte and most visits never ask for a picture. `hints.js` is
  imported the same way and for the same reason — the index filter is well over a
  megabyte — and holds the two things that read one: the private lookup, which answers
  "which indexed contracts has this account touched" from the downloaded file so the
  daemon never learns the address, and the blinded-watchlist builder. It cannot close
  over this file's scope, so the filter primitives are handed to it on
  `window.evmscanHints`; there is deliberately only one implementation of the
  arithmetic on the page, because a second one would be a second thing to keep
  byte-identical with Go. Token metadata is attacker-controlled text from the chain, so
  everything interpolated into markup goes through `esc()`.
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
  reader's behalf.
- Hint filters annotate, never filter. A token list is curated and therefore
  incomplete, so dropping what is not on one hides real holdings of long-tail tokens.
  `known` rides alongside a portfolio row and is absent — not false — when no list is
  configured; `known_only=true` is the caller's explicit opt-in.
- A filter is never trusted. It says where to look; the live lens read says what is
  there. A false positive costs one `balanceOf`; there are no false negatives except
  from staleness, which is why an index filter carries the block it is true as of.
- The cross-chain sweep in `web/index.html` is client-only: viem's bundled chain
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
