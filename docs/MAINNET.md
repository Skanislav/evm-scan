# Deploying evm-scan on Ethereum mainnet

This is the runbook for a real deployment: mainnet chain, real ETH, a registry whose
rules cannot be changed after the constructor runs. [docs/RAILWAY.md](RAILWAY.md) is
the Sepolia version of the same thing and stays the place to rehearse. Everything
here assumes you have done that rehearsal at least once.

Read sections 0 and 1 before you spend anything. They decide whether the rest of the
document is worth following.

## 0. What a Railway deployment can and cannot answer

The hosted setup runs a [Helios](https://github.com/a16z/helios) light client next to
the daemon so that `require_local_node` stays true without a node on disk. Helios can
only verify blocks the EIP-2935 history contract still serves: **8191 blocks**. On
Sepolia that is most of a day. On mainnet, at 12-second blocks, it is **about 27
hours**.

That is not a tuning parameter. It is the whole shape of the deployment:

- every backfill stops at the floor, and every asset reports `history_complete: false`;
- `hint_from_block` older than ~27 hours is unreachable, so `requestIndexing` with a
  token's real deploy block buys a range the indexer cannot serve;
- the answer `contractsOf` gives is **"which contracts has this address touched in
  roughly the last day"**, not "ever".

There is a real product in that — a wallet asking "what did this address just
interact with" does not need genesis — but it is not the same product, and the
commitment you bond says so. Decide which one you are deploying:

| | Helios on Railway | Full or archive node |
| --- | --- | --- |
| History | rolling ~27 hours | whatever the node holds |
| `history_complete` | always false | true once the backfill reaches the floor |
| Where it runs | Railway, no volume needed beyond Helios state | a machine with 1–2 TB of NVMe; not Railway |
| Trust | verified against beacon headers | your own node |
| Cost | an RPC quota that serves `eth_getProof` | hardware |

The rest of this document is the Railway path. For the node path, the only
differences are that `chains[].node` points at your geth IPC socket instead of
loopback Helios, `backfill_window` and `discovery.lookback` can be far larger, and
sections 1 through 7 are unchanged.

## 1. What it costs, and the order that cannot be undone

**Immutable at construction.** `HintRegistry` takes its `Economics` tuple and its
gateway list in the constructor and has no setter for any of it. In oracle mode
`arbiter` is `address(0)`, so `setGateways` — the one entry point a key would still
hold — reverts permanently as well. Changing any of it means deploying a new registry
and moving every asset registration and every funder to it.

The consequence for ordering: **the Railway service and its public domain must exist
before you deploy the registry**, because the gateway URL is part of the constructor
call. Sections 2 and 3 come before section 5 for that reason and no other.

**Gas.** Each epoch is three transactions from the publisher EOA: `publishIndex`,
`finalizeIndex` after the challenge window, and `claimCoverage`. Together they are on
the order of 1.5M gas, plus roughly 30k per asset in the claim. At 10 gwei that is
about 0.015 ETH per epoch; at 40 gwei, 0.06 ETH. `min_expected_reward_wei` in the
config is what stops the daemon posting an epoch the funding will not cover — set it
from the gas price you are actually seeing, not from the default.

**Bond.** In oracle mode the publisher bond is an ERC-20, not ETH. The publisher EOA
needs both: ETH for gas and the bond token for the assertion. The bond is returned
when the epoch finalizes, so it is working capital, not a fee — but it is locked for
the whole challenge window, so size it against how many epochs you want in flight.

## 2. Rehearse on Sepolia in oracle mode

Do this even if you have already run the Sepolia demo, because the demo runs in
local-arbiter mode and mainnet will not.

```bash
make build
./bin/evmscan-deploy -node https://<sepolia rpc> -key 0x<deployer key> \
  -oracle 0x<sepolia OOv3> -bond-currency 0x<sepolia bond token> \
  -publisher-bond <at or above the oracle minimum> \
  -asset-bond 0 -challenge-window 3600 \
  -min-funding 1000000000000000 -reward-per-block 100000000000 \
  -gateway 'https://<sepolia domain>/ccip/{sender}/{data}.json'
```

The tool prints the mode it deployed in; confirm it says `optimistic-oracle` and
`arbiter none`. Then run one full epoch through it (section 7) and **record the gas
each of the three transactions actually used**. Those numbers are what you put in
`fallback_gas` and `min_expected_reward_wei` for mainnet. The value shipped in
`deploy/config.mainnet.yaml` is a starting point sized for oracle mode, not a
measurement of your deployment.

## 3. Railway service and Postgres

Identical to [docs/RAILWAY.md](RAILWAY.md) section 3, with three differences:

1. Add `EVMSCAN_CONFIG=/app/config.mainnet.yaml`. The image ships both hosted
   profiles and defaults to the Sepolia one.
2. Set `HELIOS_NETWORK=mainnet`.
3. `HELIOS_EXECUTION_RPC` must be a **mainnet** RPC that serves `eth_getProof`, and
   `HELIOS_CONSENSUS_RPC` a mainnet beacon API serving the light-client endpoints.
   Mainnet block density means discovery does far more verified receipt fetches per
   hour than on Sepolia; check your provider's quota against
   `discovery.max_blocks_per_tick` and `discovery.interval` before you leave it
   running.

| Variable | Value |
| --- | --- |
| `EVMSCAN_CONFIG` | `/app/config.mainnet.yaml` |
| `EVMSCAN_DATABASE_DSN` | `${{Postgres.DATABASE_URL}}` |
| `EVMSCAN_REGISTRY_ADDRESS` | filled in after section 5 |
| `EVMSCAN_PUBLISHER_KEY` | the key from section 4 (sealed) |
| `HELIOS_NETWORK` | `mainnet` |
| `HELIOS_EXECUTION_RPC` | mainnet RPC with `eth_getProof` |
| `HELIOS_CONSENSUS_RPC` | mainnet beacon API with light-client endpoints |
| `HELIOS_CHECKPOINT` | a recent finalized beacon block root |

Deploy it once with `EVMSCAN_REGISTRY_ADDRESS` unset or pointing at nothing, purely
to obtain the public domain. Write the domain down; it goes into the constructor.

Set `HELIOS_CHECKPOINT` explicitly on mainnet rather than relying on the public
fallback list. It is the weak-subjectivity anchor the whole trust argument rests on:
take a recent finalized beacon block root from a source you trust and refresh it if
the service is ever down for more than about two weeks.

## 4. Keys

Two distinct keys. Do not reuse one for both.

- **Deployer** — pays for the registry deployment and the initial `requestIndexing`
  calls. Never goes near Railway. Hardware wallet or a key you can retire; after
  section 6 it has no privileged role, because there is no privileged role.
- **Publisher** — lives in `EVMSCAN_PUBLISHER_KEY` on Railway and signs every epoch.
  It holds ETH for gas and the bond token for assertions. Treat it as hot: it can
  spend gas and lock bonds, and nothing more. It cannot touch asset funding, cannot
  change the registry, and cannot resolve a dispute.

The publisher key is a Railway environment variable, which means it is readable by
anyone with access to that project. Size its balance accordingly: enough for a few
days of epochs, topped up, not a treasury.

## 5. Choosing the economics

Every value below is fixed forever by the constructor. Fill this table in on paper
before you run anything.

| Parameter | What it does | How to choose it |
| --- | --- | --- |
| `-oracle` | UMA Optimistic Oracle V3. Non-empty selects oracle mode. | Take the mainnet OOv3 address from UMA's official deployments page and verify it on Etherscan. Do not copy it from here or anywhere else. |
| `-bond-currency` | ERC-20 the publisher and disputer bonds are denominated in. | Must be on UMA's whitelist for that oracle. Read `getMinimumBond(currency)` on the oracle before you deploy. |
| `-publisher-bond` | Bond per commitment, in bond-currency units. | At or above `getMinimumBond`. The constructor checks this and reverts with `BadBond` if it is short, so a wrong value costs you a failed deployment rather than a broken registry. Leave headroom: **UMA can raise a currency's minimum later, and if it rises above this value every `publishIndex` starts reverting with no fix but a new registry.** |
| `-asset-bond` | Wei `registerAsset` requires; refundable via `revokeAsset`. | Spam pricing only. Zero is defensible; a small non-zero value discourages junk in `listAssets`. |
| `-challenge-window` | Seconds a commitment is disputable; the assertion liveness in oracle mode. | Long enough that a watcher can re-derive the index and dispute — hours, not minutes. It also sets how long each bond is locked. |
| `-min-funding` | Least `requestIndexing` accepts above the asset bond. | Enough that a request is worth the gas of servicing it. `min-funding / reward-per-block` is the smallest number of blocks anyone can buy. |
| `-reward-per-block` | Wei paid per newly covered block of a funded asset. | This is the whole economic model. Work backwards: one epoch over the ~8191-block window pays `8191 × reward-per-block` for a fully-funded asset, and that has to beat the ~1.5M gas the epoch costs. At 10 gwei, break-even on a single asset is around 2 gwei per block. |
| `-gateway` | ERC-3668 URL template, repeatable. | The Railway domain from section 3, as `https://<domain>/ccip/{sender}/{data}.json`. In oracle mode this list can never be changed. Add more than one if anyone else will run a gateway; a client may also bring its own, so a gateway going down degrades lookups rather than breaking them. |

## 6. Deploy the registry

```bash
./bin/evmscan-deploy -node https://<mainnet rpc> -key 0x<deployer key> \
  -oracle 0x<mainnet OOv3> \
  -bond-currency 0x<whitelisted ERC-20> \
  -publisher-bond <units> \
  -asset-bond 0 \
  -challenge-window 7200 \
  -min-funding <wei> \
  -reward-per-block <wei> \
  -gateway 'https://<domain>/ccip/{sender}/{data}.json'
```

Check the printed summary before doing anything else. It must say:

```
mode             optimistic-oracle
oracle           0x…
bond currency    0x…
arbiter          none — disputes are settled by the oracle, and no admin
                 setter on this deployment is reachable.
```

If it says `local-arbiter (FALLBACK)`, you passed no `-oracle` and you have deployed a
registry where one key settles every dispute. On mainnet that is almost certainly not
what you want; deploy again.

Then set `EVMSCAN_REGISTRY_ADDRESS` on Railway to the printed address and redeploy the
service.

### 6a. Redeploying the live Base registry for votes

The registry that has been live since 2026-09-10 (`0xE51eFF3d13Cc857a2aA1F6335592Fb2B80fA1375`
on Base, local-arbiter mode) predates `vote`/`listDemand`. Nothing on a registry has
setters, so on-chain demand means a new deployment with the same economics and the
same gateway, read back off the live contract so nothing is retyped:

```bash
make build
./bin/evmscan-deploy -node https://<base rpc> -key 0x<deployer key> \
  -arbiter 0x9cC4B3F6f0da6a5DbBa587d15Df7332e290EeeFf \
  -asset-bond 0 \
  -publisher-bond 0 \
  -challenge-window 3600 \
  -min-funding 100000000000000 \
  -reward-per-block 10000000000 \
  -gateway 'https://evm-scan-production.up.railway.app/ccip/{sender}/{data}.json'
```

The tool will print `local-arbiter (FALLBACK)`, and §6's "deploy again" does not
apply: that is the mode the live Base registry already runs in, chosen on purpose
(docs/TOKENOMICS.md §6.2), and the arbiter above is the live one.

### 6b. Signed votes

Since `voteFor` the registry accepts a vote signed by its voter and carried by
anyone. The daemon carries them with the publisher key (`POST /v1/demand/relay`),
so a reader needs no gas on Base. A registry deployed before `voteFor` makes the
relay report `available: false`. Deploying the signed-vote registry is the same
command as §6a with the current arbiter; the daemon must be redeployed with the
matching artifacts afterwards. The relay and the on-chain vote stay in code and
are off the reader page: the page's act is one signed verdict to the daemon
(`POST /v1/verdict`, docs/SHIP.md D1), and only the API counter promotes.

### 6c. Rotating the arbiter key

Since the Ownable2Step change the arbiter is the registry's owner and a leaked key is
rotated, not redeployed. Two transactions from two keys, and nothing changes until the
second lands, so a mistyped address cannot orphan the registry:

```bash
# from the current owner
./bin/evmscan-deploy -node https://<base rpc> -key 0x<current key> \
  -registry 0x<registry> -transfer-owner 0x<new address>
# from the new key
./bin/evmscan-deploy -node https://<base rpc> -key 0x<new key> \
  -registry 0x<registry> -accept-owner
```

`arbiter()` follows the owner, so `/v1/status` and the deploy tool's mode summary
show the new key as soon as it accepts. The publisher key is a separate matter: it is
whatever `EVMSCAN_PUBLISHER_KEY` holds and needs no on-chain step. The two registries
deployed before this change (`0xE51e…1375`, `0xCDe4…98f2`) have an immutable arbiter
and cannot be rotated; they are abandoned.

Done on 2026-09-12, three times, same economics and gateway throughout. First
`0xcde45355570e25b90e9aadf6cb1a999ee7f198f2` (immutable arbiter, abandoned the same
day when its arbiter key leaked). Then `0xf6ba84CA25d949E99c241C27980E90aF1095F13b`
after the Ownable2Step change (owner `0x91C1…A0B9`), abandoned hours later for the
signed-vote registry. The live one: `0x6D021dBe3A5804F6AC4faE7A20117dF8d7525Ad7`,
deploy tx `0x599f58ff962f4cc635db3207f2a0c408cd990c72a93afa0414a5b0f1b5649624`, owner
and arbiter `0x91C117Faa280B6b6f0413b71cAa2b9F7372bA0B9`, verified on Basescan. Reading the receipt back off `base-rpc.publicnode.com`
lagged by minutes while `mainnet.base.org` had it at once; the tool's receipt wait
timed out on the first and the deployment was fine, which is exactly the "do NOT
deploy again" case the tool warns about.

Then point `EVMSCAN_REGISTRY_ADDRESS` on Railway at the printed address and redeploy.
Three things follow from a fresh registry and none of them is a fault:

- It has no finalized epoch until the publisher posts one and the challenge window
  passes, so `contractsOf`, the `/ccip` gateway and any ENS record built on it answer
  `NoFinalizedEpoch` for at least an hour. Check the first epoch actually lands: `Build` refuses an
  unchanged root, so the publisher posts only once the local table differs from what
  the last registry saw.
- Assets registered on the old registry (GHO, unfunded) keep their local rows with
  `source: onchain`; the new registry does not list them and the mirror does not
  revoke what it cannot see.
- Until then the mirror logs once that the registry serves no demand and keeps
  going; signed verdicts and API votes work regardless.

Approve the bond. The daemon does this itself on its first publish
(`ensureBondAllowance` approves exactly one bond, never unlimited), but it needs the
tokens to be there: send the publisher EOA at least one bond's worth of the bond
currency, plus ETH for gas.

### 6d. `hints.evm-scan.eth`: the signed resolver on mainnet

**Not part of the current ship** (docs/SHIP.md D2): the resolver is built and
sim-tested, never deployed, and `hints.evm-scan.eth` does not resolve today. Kept as
the runbook for when it is.

The registry is on Base and ENS on mainnet, so the mainnet name would be served by
`HintSignedResolver`, whose answers the daemon signs (docs/ENS.md). Five steps, the
first and second from the name owner's wallet:

1. Deploy the resolver (the deployer becomes its owner; the classifier stops Claude
   from sending mainnet transactions, so this is yours to run):
   ```bash
   ./bin/evmscan-ens deploy-signed-resolver -node https://<mainnet rpc> -key 0x<owner key> \
     -signer <ens_signer from GET /v1/status> \
     -gateway 'https://evm-scan-production.up.railway.app/ens/{sender}/{data}.json' \
     -chain-id 1 -registry 0x6D021dBe3A5804F6AC4faE7A20117dF8d7525Ad7 -registry-chain-id 8453
   ```
   Verify it on Etherscan with `scripts/verify-contract.sh -chain 1 -name HintSignedResolver …`.
2. Create the subnode from the owner of `evm-scan.eth` (unwrapped, so the ENS registry
   accepts the owner directly): on `0x00000000000C2E074eC69A0dFb2997BA6C7d2e1e`,
   `setSubnodeRecord(node = namehash("evm-scan.eth"), label = keccak256("hints"),
   owner = <your address>, resolver = <the address from step 1>, ttl = 0)`.
3. Service records on `evm-scan.eth` itself, in the ENS app: `evmscan.registry` =
   `eip155:8453:0x6D021dBe3A5804F6AC4faE7A20117dF8d7525Ad7`, `evmscan.hints` =
   `https://evm-scan-production.up.railway.app/v1/hints`, `evmscan.api` =
   `https://evm-scan-production.up.railway.app`.
4. On Railway set `EVMSCAN_ENS_RESOLVER` to the resolver and redeploy; the profile
   already carries `ens_parent: "evm-scan.eth"`. From then on `/ens` signs only for it
   and `/v1/status` shows `ens_resolver` and `ens_signer`.
5. `./bin/evmscan-ens check -node https://<mainnet rpc> -name <hex>.hints.evm-scan.eth`,
   and any ENS client: `viem.getEnsText({name, key: 'evmscan.contracts'})`.

### 6e. Verify the source on the explorer

A registry nobody can read is a registry nobody can check. Verification is a
recompile, so the explorer needs the same input this repo compiled — `make
contracts` writes `contracts/out/standard-input.json` for exactly that, and the
constructor arguments are read off the creation transaction rather than re-encoded
from the flags you believe you passed:

```bash
ETHERSCAN_API_KEY=... scripts/verify-contract.sh \
  -chain 8453 -address 0x<registry> -tx 0x<deploy tx> -rpc https://<rpc>
```

One Etherscan key works across chains on the v2 API, `-chain` selecting which.
If it reports that the creation code is not a prefix of the deployed input, the
working tree is not the commit that produced the contract: check that commit out
and run `make contracts` again.

## 7. Seed it and test the whole loop

Register and fund the assets you actually want indexed. On mainnet, `fromBlock` should
be inside the ~27-hour window — a real deploy block buys a range Helios cannot verify:

```bash
./bin/evmscan-deploy -node https://<mainnet rpc> -key 0x<deployer key> \
  -registry 0x<registry> \
  -request 0x<token>:20:<recent block>:<wei>
```

Then walk the loop end to end. Each step has an observable that tells you the previous
one worked.

```bash
# 1. The daemon is up, on its own verified node, and moving.
curl https://<domain>/v1/health      # 200 {"status":"ok"}
curl https://<domain>/v1/status      # node_local: true, head_block advancing

# 2. The asset arrived through the mirror and is being scanned.
curl https://<domain>/v1/assets      # your token, history_complete: false

# 3. Post a commitment rather than waiting for the timer.
curl -XPOST https://<domain>/v1/epochs -d '{"publish":true}'
curl https://<domain>/v1/epochs/1    # onchain_status: proposed, submission_ref set
```

`409 index and coverage unchanged` means the roots match the last commitment — pass
`"force": true` to build anyway. `402 coverage is worth less than the publisher's
minimum` is the `min_expected_reward_wei` guard doing its job, not a failure: fund
the assets or lower the threshold.

```bash
# 4. The Go proof and the Solidity verifier agree. This is the invariant that
#    matters: internal/merkle must match HintRegistry.leafHash byte for byte.
./bin/evmscan-verify -api https://<domain> -node https://<mainnet rpc> \
  -registry 0x<registry> -epoch 1 -account 0x<account>
```

Wait out the challenge window. The publisher tick then calls `finalizeIndex`, which
settles the assertion and returns the bond, and `claimCoverage`, which pays
`reward-per-block` for every newly covered block of each funded asset.

```bash
# 5. The epoch finalized and the gateway serves the finalized root.
curl https://<domain>/v1/epochs/1    # onchain_status: finalized, claim_tx, reward_wei

# 6. An ERC-3668 client resolves contractsOf through the gateway and the contract
#    verifies the answer. This exercises the immutable gateway list.
./bin/evmscan-verify -ccip -node https://<mainnet rpc> \
  -registry 0x<registry> -account 0x<account>

# 7. The publisher was paid back. Compare its ETH balance against the gas the three
#    transactions cost; reward_wei on the epoch is what the registry paid.
```

Step 7 is the one that decides whether the deployment is sustainable. If `reward_wei`
is consistently below the gas the epoch cost, either the assets are underfunded or
`reward-per-block` was set too low — and `reward-per-block` cannot be changed. Raise
funding per asset, or redeploy.

## Operating notes

- **The ~27-hour window is the main failure mode.** If the service is down longer than
  that, the follower's tail falls outside what Helios can verify and every follow tick
  fails until you reset the affected `asset_cursors` rows to a block inside the window.
  `/v1/health` turns 503 while that is true. A Railway restart policy and an alert on
  `/v1/health` are worth more here than anywhere else.
- **Set `EVMSCAN_API_TOKEN` before the publisher key.** `POST /v1/epochs`,
  `/promote` and `/v1/assets` spend something — the publisher's gas, or a backfill
  against a paid RPC quota — and with no token set they are open to anyone with the
  URL. With one set they need `Authorization: Bearer <token>`; reads, including the
  ERC-3668 gateway, are never guarded, because a public index is the point.
  `min_expected_reward_wei` bounds what a single epoch can waste but not how many
  someone can ask for.
- **`auto_promote` is off in the mainnet profile.** Every promotion spends verified
  RPC quota on a backfill nobody paid for. `requestIndexing` is the path that
  reimburses it; leave promotion to that unless you are deliberately sponsoring.
- **Coverage claims are one transaction per epoch** carrying a proof per asset. That
  is fine at tens of assets and will eventually need chunking; watch the gas on
  `claim_tx` as the asset count grows.
- **Quoting coverage is one `eth_call` per asset**, and through Helios each one is a
  verified call. Building an epoch over many assets is therefore slow before it is
  expensive.
- **Prices**, where the daemon serves them, come from Chainlink and Uniswap
  deployments read at head; mainnet is the chain with the most complete built-in
  defaults, including the Chainlink Feed Registry. Nothing about pricing touches the
  commitment path — a missing price is an empty field, never a wrong number.
- **Disputes.** In oracle mode you do not resolve anything: UMA's DVM does. If a
  commitment is disputed, `finalizeIndex` reverts until the vote lands, the daemon
  keeps simulating it and costs no gas while it waits, and the outcome is mirrored
  into the local epoch status either way. A rejected epoch loses the bond; that is
  the design working.
