# Running evm-scan on Railway

For a mainnet deployment read [docs/MAINNET.md](MAINNET.md) instead — the registry's
economics and gateway list are immutable, so the order of the steps there matters and
this document's order does not carry over. This one is the Sepolia rehearsal, and is
worth doing first either way.

This runs the indexer against **Sepolia** without giving up the local-node rule. A
[Helios](https://github.com/a16z/helios) light client runs inside the same container on
loopback, verifies everything an upstream RPC returns against beacon-chain headers, and
the daemon talks only to it. `require_local_node` stays `true`, and it stays honest.

What you get:

- one Railway service built from the `Dockerfile` (evmscand + Helios, supervised by
  `deploy/entrypoint.sh`), plus a Railway Postgres;
- discovery and indexing over the last ~8000 Sepolia blocks (the window Helios can
  verify), with `history_complete: false` on anything deeper;
- commitments posted on a timer by an EOA that is paid back, with a reward, by the
  registry every time one finalizes.

## 1. Deploy the registry on Sepolia

Fund a deployer EOA from a faucet, then:

```bash
make build
./bin/evmscan-deploy -node https://<sepolia rpc> -key 0x<deployer key> \
  -arbiter 0x<deployer address> \
  -asset-bond 0 -publisher-bond 0 -challenge-window 3600 \
  -min-funding 1000000000000000 -reward-per-block 100000000000 \
  -gateway 'https://<domain>/ccip/{sender}/{data}.json'
```

Record the printed registry address. The choices above mean:

| Parameter | Value | Why |
| --- | --- | --- |
| `arbiter` | the deployer | Local-arbiter mode: one key settles disputes. Sepolia does have a UMA Optimistic Oracle V3 deployment, so `-oracle 0x… -bond-currency 0x…` (and a publisher bond at or above the oracle's minimum, in that token) is the neutral choice once the publisher holds some of the bond token. The tool prints which mode it deployed. |
| `publisher-bond` | 0 | The publisher never needs to lock ETH. Trade-off: `challengeIndex` is free too. Fine for a demo. |
| `asset-bond` | 0 | Registration is free; `requestIndexing` still requires `min-funding`. |
| `min-funding` | 0.001 ETH | Least a request must deposit; all of it is the asset's funding. |
| `reward-per-block` | 100 gwei | Paid to the publisher per newly covered block of a funded asset. 0.001 ETH buys 10,000 blocks, about a day of Sepolia; the ~8,000-block Helios window then pays ~0.0008 ETH on first coverage, many times Sepolia gas. |
| `challenge-window` | 1 h | How long a commitment stays proposed before it can be finalized. |

Seed it with something to index. Pick two or three active Sepolia tokens and a recent
block, and pay for them:

```bash
./bin/evmscan-deploy -node https://<sepolia rpc> -key 0x<deployer key> -registry 0x<registry> \
  -request 0x<token>:20:<recent block>:5000000000000000 \
  -request 0x<token>:20:<recent block>:5000000000000000
```

Each request registers the token and deposits the payment above the bond as its funding
(0.005 ETH is 50,000 blocks at the rate above). The mirror picks registrations up on its
next sync; nothing else needs to be told. Top an asset up later with
`-fund 0x<token>:<wei>`.

## 2. Publisher key

Generate a fresh key for the publisher and fund it with a little Sepolia ETH from a
faucet, enough for a handful of transactions. After that the registry pays it back: the
daemon finalizes its own epochs once their window closes, which returns the bond, and
then claims the coverage reward for every funded asset the epoch declared. Set
`min_expected_reward_wei` in the config to what a publish + finalize + claim costs you in
gas and the daemon will not post an epoch that does not pay for itself.

## 3. Railway project

1. **Postgres**: add the Railway Postgres plugin.
2. **Service**: create a service from this repository. `railway.json` selects the
   Dockerfile builder, the `/v1/health` check, restart on failure and a 30 s drain.
   Keep it at one replica: one database writer, one Helios.
3. **Volume** (optional): mount 1 GB at `/data/helios` so Helios keeps its last
   checkpoint across restarts.
4. **Variables**:

| Variable | Value |
| --- | --- |
| `EVMSCAN_DATABASE_DSN` | `${{Postgres.DATABASE_URL}}` (append `?sslmode=require` if you use the public proxy URL) |
| `EVMSCAN_REGISTRY_ADDRESS` | the address from step 1 |
| `EVMSCAN_PUBLISHER_KEY` | the key from step 2 (sealed) |
| `HELIOS_EXECUTION_RPC` | an upstream Sepolia RPC that supports `eth_getProof` (Alchemy, dRPC, …) |
| `HELIOS_CONSENSUS_RPC` | `https://ethereum-sepolia-beacon-api.publicnode.com` |
| `HELIOS_CHECKPOINT` | a recent finalized beacon block root (optional; see below) |
| `EVMSCAN_LOG_LEVEL` | `info` or `debug` (optional) |

`EVMSCAN_API_LISTEN` is derived from Railway's `PORT` by the entrypoint; leave it unset.

The checkpoint is a weak-subjectivity anchor. Helios falls back to a public one when it
is missing, which is fine for a testnet demo. If the service has been down for more than
about two weeks, set a fresh one from any Sepolia beacon explorer.

5. **Deploy**. First boot takes a minute: Helios syncs the sync committee, then the
   daemon probes the history floor (about 25 single-block log queries through Helios),
   then discovery starts walking forward.

## 3b. Advertise the gateway

Once the service has a public domain, tell the registry where `contractsOf` answers
come from. Any ERC-3668 client then resolves lookups through it and verifies the
result on-chain:

```bash
./bin/evmscan-deploy -node https://<sepolia rpc> -key 0x<deployer key> -registry 0x<registry> \
  -gateway 'https://<domain>/ccip/{sender}/{data}.json'
```

`HintResolver` (docs/ENS.md) advertises the same list, read from the registry at call
time, so an ENS client resolving `<hex>.hints.<yourname>.eth` ends up at this gateway too.

That call is `setGateways`, which only the arbiter of a local-arbiter registry can make.
In oracle mode there is no setter, so the list is whatever `-gateway` said at deployment;
pass the final domain then.

Try it: `./bin/evmscan-verify -ccip -node https://<sepolia rpc> -registry 0x<registry> -account 0x<account>`.

## 4. Check it

```bash
curl https://<domain>/v1/health          # 200 {"status":"ok"} or 503 with the failing check
curl https://<domain>/v1/status          # node_local: true, head_block moving, reward_per_block_wei
curl https://<domain>/v1/assets          # the requested tokens, backfilling toward the floor
```

Post a commitment by hand instead of waiting for the 30 minute timer:

```bash
curl -XPOST https://<domain>/v1/epochs -d '{"publish":true}'
curl https://<domain>/v1/epochs/1        # submission_ref, tx_hash, onchain_status: proposed
```

Posting again straight away answers `409 index unchanged`; pass `"force": true` to
override. After the challenge window the next publisher tick finalizes the epoch, the
publisher balance on Etherscan rises by the reward, and `onchain_status` becomes
`finalized`.

Verify a proof independently against the Solidity verifier:

```bash
./bin/evmscan-verify -api https://<domain> -node https://<sepolia rpc> \
  -registry 0x<registry> -epoch 1 -account 0x<account>
```

## Operating notes

- **History is a window, not a floor.** Helios verifies at most the last ~8191 blocks
  (EIP-2935). Backfills stop there and `history_complete` says so. If the daemon is
  down for more than about 27 hours the follower's tail can fall outside the window and
  every follow tick will fail; `/v1/health` turns 503. Reset the affected rows in
  `asset_cursors` to a block inside the window and restart.
- **Cost.** Every block that contains a matching log costs Helios one verified
  `eth_getBlockReceipts` upstream. On Sepolia nearly every block has a `Transfer`, so
  discovery is the main consumer of your RPC quota. Windows in `deploy/config.railway.yaml`
  are sized for that; raise them with care.
- **`eth_getLogs` is capped at 4096 blocks** per call by Helios. Config windows are 2000.
- **The write endpoints are open until you set `EVMSCAN_API_TOKEN`.** `POST
  /v1/epochs`, `/promote` and `/v1/assets` spend gas or RPC quota; with a token set
  they want `Authorization: Bearer <token>` and the page will ask for it once and
  keep it in the browser. Reads are never guarded. Fine to leave open for a demo with
  no publisher key; not once one is set.
- **Publisher modes.** `registry.publisher.mode` is `eoa` and nothing else today. The
  daemon's `Submitter` interface is where a paymaster, relayer or ERC-4337 account would
  plug in; the contract does not need to change for that.
- **Local dry run** without Railway: run Helios on your laptop with the same flags the
  entrypoint uses, then `./bin/evmscand -config deploy/config.railway.yaml` with the
  `EVMSCAN_*` variables exported. `make docker-run` runs the exact image with an
  `.env.railway` file.
