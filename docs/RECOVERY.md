# Recovering an index from the chain

## The problem this solves

`HintRegistry` commits a merkle root. A root proves a leaf to whoever already holds
that leaf, and recovers nothing on its own: epoch 1 commits 540,059 accounts in 32
bytes, and no amount of reading the chain turns those 32 bytes back into a table.

So before this existed, an index lived exactly as long as the publisher's Postgres.
The commitment made it *auditable by the publisher* and useless to everyone else,
which is close to the opposite of the point. Three things were missing, and only the
third needed any new thinking:

1. The table was never published anywhere.
2. The asset set was not on chain. `listAssets` returns only assets registered
   through `requestIndexing` — on the live deployment that is 1 of 8. The other 7
   were promoted locally, and `assetKey` is a keccak that does not invert, so even
   the coverage leaves do not name them.
3. Nothing could tell a real table from a forged one.

## The shape of the answer

`Epoch.uri` already existed for this — *"Pointer to the full index table (ipfs://,
https://, ...)"* — and `IndexPublished` already emits it. **No contract change was
needed.** A recovery starts from a log on the chain and ends with a working index.

The document it points at is self-authenticating. `snapshot.Verify` recomputes every
leaf from the account and its assets, and every coverage leaf from the asset key and
its range, then rebuilds both trees. If the roots equal what the registry holds, the
file is exactly what was committed — no matter who served it.

That inverts the usual hosting problem: **availability is the only thing a host is
trusted for.** A snapshot can be mirrored to IPFS, S3, a colleague's laptop, or
handed over on a USB stick, and it is still checkable against the chain. Nothing in
the daemon needs to know where the copies are.

## Recovering

```bash
# 1. Find the pointer. It is in the IndexPublished log; getEpoch also has it.
cast logs --address $REGISTRY 'IndexPublished(uint256,uint64,uint64,uint64,bytes32,bytes32,string,address)' \
  --rpc-url $BASE_RPC
cast call $REGISTRY 'latestFinalizedEpoch(uint64)(bool,uint256)' 1 --rpc-url $BASE_RPC

# 2. Check the table against the chain before believing a byte of it.
evmscan-verify -snapshot https://…/v1/epochs/2/snapshot \
  -node $BASE_RPC -registry $REGISTRY

#   VERIFIED: this table is exactly what epoch 1 committed.
#     540059 accounts over 8 assets, blocks 25935372..25944437 on chain 1

# 3. Rebuild. The snapshot is re-verified here too; -skip-verify exists but says so.
evmscan-restore -snapshot https://…/v1/epochs/2/snapshot \
  -node $BASE_RPC -registry $REGISTRY -dsn "$EVMSCAN_DATABASE_DSN"
```

`evmscan-restore -dry-run` verifies and reports without writing.

The daemon keeps the table only for the latest finalized epoch and its successors;
older epochs' leaves are pruned once a newer one finalizes, and their snapshot and
proof endpoints answer 410 from then on. The roots stay on chain forever, so an
epoch you may want to restore *to* has to be mirrored while it is current — which
is the point of the uri being public.

## What comes back, and what does not

| | Recovered | Why |
|---|---|---|
| account → contracts | **exactly** | this is what `root` commits |
| the asset set | **exactly** | from the coverage leaves, checked against `coverageRoot` |
| per-asset scan ranges | **exactly** | so a restarted follower resumes at the right block |
| `event_count`, `roles`, per-pair first/last block | **no** | no leaf commits them |

The last row is the honest boundary. A restored node knows which contracts an
account touched and can prove it; it does not know how many times. Those columns
come back as the epoch's own range with a zero count, and refill as the follower
observes real events. Inventing them would produce numbers that look like
measurements and are not.

This also means a restore does **not** need an archive node. That matters more than
it sounds: Helios serves roughly 8,191 blocks, so a daemon that lost its database
could never re-derive old interactions from logs at all. Loading the committed rows
is not an optimisation, it is the only way back.

## Publishing

`registry.commitment_uri` is the template, expanded when the epoch is built —
before `publishIndex` is called, because the contract takes the uri as an argument
and has no setter. An epoch published with the wrong string carries it forever.

- `{id}` — the publisher's own epoch id, which `GET /v1/epochs/{id}/snapshot` takes.
- `{chain}` — the indexed chain.

Neither is the on-chain epoch id: the registry assigns that during `publishIndex`,
which is after the uri has already been handed to it. A reader never needs it, since
it finds the whole string in the log.

The snapshot endpoint is deliberately open even when `api.auth_token` is set. That
token guards endpoints which spend the deployment's money; this one exists to be
fetched by strangers, and a recovery path nobody can reach is not a recovery path.

**Epochs published before this shipped carry `uri = ""` permanently.** Their tables
can still be dumped by id and mirrored by hand — the roots are on chain, so an old
snapshot verifies exactly like a new one — but the chain will not tell anyone where
to look.

## Serving it from the daemon is the floor, not the goal

The default template points at the daemon itself, which is the one host guaranteed
to exist. It is also the host that disappears in precisely the scenario this
document is about. Mirror the file somewhere that outlives the service; because it
is self-authenticating, doing so costs nothing but disk.
