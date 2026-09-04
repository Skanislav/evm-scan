# Running the demo

The demo runs entirely against a local chain. No third-party RPC is used, which is the
point — but it also means you need a `geth` binary.

## 0. Get geth

Any geth ≥ 1.14 works. If you don't have one, build the exact version this repo already
depends on, straight from the Go module cache:

```bash
go build -o bin/geth github.com/ethereum/go-ethereum/cmd/geth
export PATH="$PWD/bin:$PATH"
```

## 1. Postgres

```bash
createdb evmscan
psql -c "CREATE ROLE evmscan LOGIN PASSWORD 'evmscan'" 
psql -c "ALTER DATABASE evmscan OWNER TO evmscan"
```

The daemon applies its own migrations on start (`database.auto_migrate`).

## 2. A dev chain

```bash
mkdir -p .devchain
geth --dev --dev.period 2 --datadir .devchain \
  --http --http.addr 127.0.0.1 --http.port 8545 --http.api eth,net,web3 \
  --ipcpath "$PWD/.devchain/geth.ipc"
```

`--dev.period 2` mines every two seconds so the follower has a moving head to track.
The IPC socket is what evmscand connects to; loopback HTTP/WS also work.

> Snap sync is what a *real* deployment uses. A dev chain has no history to sync, so it
> stands in for a snap-synced node here — the code path is identical, because everything
> goes through the same `chain.Source`.

## 3. Bootstrap

```bash
make build
./bin/evmscan-demo -node "$PWD/.devchain/geth.ipc" -out config.demo.yaml
```

This deploys `HintRegistry`, two ERC-20s and an ERC-721, funds six deterministic demo
accounts, generates mints / transfers / approvals between them, registers the three
tokens as hints, and writes a ready-to-run config.

Note the printed user addresses — you'll paste one into the UI.

> `config.demo.yaml` contains a dev-chain private key derived from a fixed seed. It is
> gitignored. Never reuse it anywhere real.

## 4. Run

```bash
./bin/evmscand -config config.demo.yaml
```

Open <http://127.0.0.1:8080>. You should see the registry mirror pick up three hints,
the backfill complete, and the index fill in.

## 5. What to actually look at

**The registry mirror is the permissionless path.** Nothing told evmscand about those
tokens; it read them out of `HintRegistry`. Register another contract from any account
and the indexer picks it up within `registry.sync_interval`.

**The two-pointer scan.** In the assets table, watch `backfill` while the head keeps
moving. The follower is already producing data from the registration anchor forward
while history fills in behind it.

**Commit the index on-chain**, then verify it without trusting the API:

```bash
curl -XPOST localhost:8080/v1/epochs -d '{"publish":true}'

./bin/evmscan-verify \
  -node "$PWD/.devchain/geth.ipc" \
  -registry "$(grep -m1 address config.demo.yaml | cut -d'"' -f2)" \
  -epoch 1 -account 0xYOUR_DEMO_USER
```

`evmscan-verify` recomputes the asset digest and leaf from the returned asset list rather
than trusting the digests the API reported, replays the merkle proof in Go, then calls
`HintRegistry.verifyInclusion` on-chain. Both verifiers must agree — that is the check
that the Go tree builder and the Solidity verifier encode leaves identically.

## Recompiling contracts

Artifacts are committed under `contracts/out/`, so `go build` needs no Node toolchain.
After editing Solidity:

```bash
make contracts
```

## Integration tests

Unit tests are hermetic. The node-backed ones skip unless pointed at a chain:

```bash
EVMSCAN_TEST_NODE="$PWD/.devchain/geth.ipc" \
EVMSCAN_TEST_TOKEN=0xYOUR_TOKEN \
  go test ./internal/token/ -run Probe -v
```
