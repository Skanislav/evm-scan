# Mirroring an index without trusting the mirror

## The problem this solves

`docs/RECOVERY.md` fixed the part where an index died with the publisher's Postgres:
an epoch now points at a snapshot, and `snapshot.Verify` checks that document against
the roots on chain. That made the table recoverable. It did not make it *usable by a
client*.

A wallet that wants the index has two options today, and both are bad:

1. **Ask the API.** `GET /v1/accounts/{addr}/contracts` is one round trip and it is
   trust-me. There is an inclusion proof next to it, but the wallet is still asking
   one service what the answer is.
2. **Download the snapshot.** Self-authenticating, and tens of megabytes — *per
   epoch*, because a merkle root is a fingerprint and there is no way to check that
   the 400 rows which changed are the only ones that changed.

So the honest, verifiable path costs a full re-download every epoch, and nobody will
pay it. The result is that the commitment is checkable in principle and unchecked in
practice, which is close to the opposite of the point.

## The shape of the answer

Keep the table in the client, and sync only what changed.

- Rows live in the client's own SQLite, synced by [Evolu](https://evolu.dev) over
  **range-based set reconciliation** — fingerprints compared by range, so a client
  pulls only the rows it is missing, in a number of round trips that grows
  logarithmically with the table.
- The client rebuilds the keccak root from its own rows and compares it with
  `latestFinalizedEpoch` on the registry.

That second bullet is the whole security argument, and it is unchanged from
RECOVERY.md: **availability is the only thing a host is trusted for.** An Evolu relay
is blind by construction — it moves encrypted rows and fingerprints and cannot read
them — and it is interchangeable, because nothing about the verification depends on
which relay served a row.

### Where this sits relative to docs/CLIENT-SIDE.md

That document asks which *work* can leave the daemon, and draws its line at whether a
client's answer ends up in the database — inside a root the publisher has bonded.
This lands on the safe side of that line, and not by accident: a mirror **writes
nothing back**. It consumes a commitment that already exists. A relay serving bad
rows lies only to readers who can detect it, exactly as a reader who supplies a lying
RPC to the lens lies only to themselves.

So this is the read-side counterpart to Tier 1 rather than a step toward Tier 3.
Tier 3 — clients computing index rows — is still blocked on the omission problem
described there, and nothing here touches it.

### What is Aztec-shaped about this, and what is not

The resemblance is real but narrow, and it is worth being exact about, because the
analogy leads somewhere wrong if it is taken too far.

**Holds.** State lives off chain in the client; a commitment lives on chain; the
client can prove its state against the commitment. Append-only version rows are the
same device Aztec's note-hash tree uses — history that is never edited is what keeps
a commitment to a *past* state checkable later.

**Does not hold.** Aztec's root is enforced by the protocol at insertion, with a
nullifier tree preventing double-use, and its leaves are private. Here the root is
posted by the publisher about itself and guarded only by a challenge window, and the
leaves are public token logs. Nobody enforces the transition from epoch N to N+1, and
there is nothing to keep secret. So this is not private state with a root binding; it
is a public table with a commitment, distributed peer-to-peer. Evolu's owner-scoped
encryption comes along for free and protects nothing, because the data was already
public.

## How it fits together

```
  publisher                          relay                        wallet
  ─────────                          ─────                        ──────
  evmscand                        (blind, replaceable)         local SQLite
     │                                   │                          │
     │ GET /v1/epochs/{id}/snapshot      │                          │
     ▼                                   │                          │
  ingestSnapshot ──── version rows ──────┼──── RBSR delta ─────────▶│
     │  (checks the doc against          │      (only what          │
     │   the chain first)                │       changed)           ▼
     │                                   │                    verifyAsOf
     │                                   │                          │
     └───────────── HintRegistry ────────┴──────────────────────────┘
                  latestFinalizedEpoch → root, coverageRoot
```

Note which side pays for what. The publisher downloads its own whole table every
epoch, which is free — it is the machine that produced it. Wallets get the delta.
That asymmetry is why **no change to the daemon was needed for any of this**:
`/v1/epochs` already exposes `onchain_epoch_id` and `status`, and
`/v1/epochs/{id}/snapshot` already streams the table.

## What a mirror stores

Immutable versions, never edited:

| account | sinceEpoch | assets |
| --- | --- | --- |
| `0x…01` | 4 | `[A1]` |
| `0x…02` | 4 | `[A1]` |
| `0x…02` | 7 | `[A1, B2]` |
| `0x…03` | 9 | `[]` ← tombstone |

`resolveAsOf(rows, n)` takes the newest version of each account at or below `n`. Two
things follow, and both are load-bearing:

- **A client mid-sync can still verify an older epoch.** It resolves the cut at epoch
  7 while rows from epoch 9 are already arriving. One mutable row per account would
  leave it holding a mixture of two epochs and able to rebuild neither.
- **Reconciliation converges.** A row that is never edited has a fingerprint that
  never changes, so RBSR stops re-sending it. A table of mutable rows would churn.

`sinceEpoch` is the **on-chain** epoch id, never the publisher's local one — a wallet
gets its number from `latestFinalizedEpoch`, and keying by anything else would mean a
client could not look up what it had just synced. (Confusing those two ids is the
standing bug in this codebase; see the note in `snapshot.URI`.)

Rows are scoped by `(registry, chainId)`. `leafHash` commits to the chain id, so rows
about two chains belong to two different trees, and two registry deployments on one
chain are two different sets of commitments.

## Leaf order, and why it is already correct

A root is built from a *sequence*. Sorted-pair hashing means a proof carries no
direction bits, but it does not make the tree order-independent: the same leaves in a
different order give a different root, with nothing to say which was meant.

The publisher's order is ascending by account, compared as bytes. The whole path:

| Step | Where | Order |
| --- | --- | --- |
| rows selected | `store.SnapshotIndex` (`internal/store/index.go:281`) | `ORDER BY account` over `BYTEA`, which Postgres compares bytewise |
| tree built | `hintreg.Publisher.Build` (`internal/hintreg/publisher.go:136-141`) | `Index: i`, the slice position — so `epoch_leaves.idx` *is* that order |
| document written | `store.EachEpochLeaf` (`internal/store/epochs.go:373`) | `ORDER BY idx` |

`sortLeaves` reproduces that from an unordered set, which is what lets a mirror
rebuild the root from rows SQLite handed back in any order. **No change to the Go
side was required**; the ordering was already deterministic end to end.

It is checked rather than trusted, because the failure is silent. `ingestSnapshot`
verifies the *document* (leaves hashed in the order written) and then re-reads the
*stored rows* and verifies those too (leaves hashed in sorted order) before marking
the epoch done. If those orders ever diverged, the first check would pass and every
wallet would fail forever with the ambiguous "still syncing, or the relay is lying"
message — pointing at the wrong culprit. One extra root build per epoch, on the one
machine that can fix it.

## What a match proves, and what it does not

A match says: *these rows are exactly what that epoch committed.* It does not say the
epoch's contents are true. The index is a hint — a wallet uses it to learn which
contracts are worth pulling history for, then reads that history from a source it
trusts. A finalized epoch is one nobody challenged, not one anybody verified.

A **mismatch is ambiguous**, and the code says so rather than guessing:

> unverified at epoch 9: the index root does not match. Either this mirror has not
> finished syncing, or it is serving rows the publisher did not commit — the chain
> does not say which, so re-check once sync is idle before treating it as dishonest.

Nothing on chain distinguishes those two. `IndexPublished` carries no leaf count, so
there is no number to compare against, and inventing a completeness signal would be
worse than admitting there is not one. Re-verify when sync settles.

## Running it

```bash
make test-mirror        # the TypeScript suite (needs node + npm)
go test ./internal/...  # includes the fixture-staleness checks
```

The parity fixtures are generated by Go, which is the side `cmd/evmscan-verify`
already checks against the real Solidity verifier:

```bash
go test ./internal/snapshot/ -run Fixtures -update-fixtures   # leaf/root vectors
go test ./internal/hintreg/  -run MirrorFixtures -update-fixtures  # ABI blobs
```

Without the flags those tests **assert** the committed files still match, so changing
a hash or an ABI on the Go side fails in CI rather than in somebody's wallet. Both
run under plain `go test ./...`, so the guard needs no Node toolchain.

## State of the code

| Layer | File | Verified |
| --- | --- | --- |
| commitment encoding | `mirror/src/merkle.ts` | **yes** — vectors from Go |
| snapshot parse + rebuild | `mirror/src/snapshot.ts` | **yes** — whole documents from Go |
| version rows, resolve, diff | `mirror/src/state.ts` | **yes** |
| verification + its wording | `mirror/src/verify.ts` | **yes** |
| registry ABI both ways | `mirror/src/registry.ts` | **yes** — blobs packed by go-ethereum |
| ENS names → address, and back | `mirror/src/names.ts` | **yes** — blobs packed by go-ethereum from the Universal Resolver ABI |
| ingest, idempotence, ordering | `mirror/src/ingest.ts` | **yes** — against `MemoryStore` |
| the whole flow (`syncLatest`) | `mirror/src/ingest.ts` | **yes** — fixture `eth_call`, injected fetch |
| Evolu adapter logic | `mirror/src/evolu.ts` | **partly** — against a fake Evolu |
| Evolu itself: relay, RBSR, keys | — | **no** — never run here |

The last two rows are the honest boundary. `@evolu/common` requires Node ≥ 24.20.0
and this toolchain is older, so the adapter is written against the published API and
exercised against a stand-in that implements the documented `upsert` / `createQuery`
/ `loadQuery` surface. That covers the bugs which live on this side of the seam — a
row id that is not deterministic, a missing scope, a `bigint` pushed through a JS
number — and covers nothing about Evolu's own behaviour.

**The first thing a real run will hit** is the initial ingest: with no previous
state, `diff` emits one row per account, so bootstrapping the live Base index means
about 540,000 `upsert` calls through a mutation path built for interactive apps.
Whether that wants batching, chunking across ticks, or `ShardOwner` partitions is
unknown here — it was not measured, and no bulk-insert limit was checked. Steady
state is not the problem: an epoch that changed 400 accounts writes 400 rows.

**The second is read-after-write visibility.** `markIngested` reads the epoch's
coverage row back in order to restate it, and `loadQuery` is documented as batched
and cached — so it may not yet reflect the `putCoverage` that ran moments earlier.
The adapter throws when the row is missing rather than writing a placeholder,
because a placeholder would mark the epoch complete with an empty asset set, after
which `coverage()` returns nothing forever and every later verify reports a
coverage-root mismatch blamed on an unfinished sync. Failing loudly is the right
answer, and it is also a path a first real run may find; if it does, the fix is to
await the mutation (`onComplete`) rather than to soften the check.

This is deliberate, not a shortcut: **Evolu is not a dependency of correctness.**
The `MirrorStore` interface is the seam, and verification is a hash of rows, not a
property of where they were kept. If the binding is wrong it will be wrong in ways a
first run against a relay shows immediately, and it cannot make an unverified mirror
report a false pass.

## Wiring it to a real Evolu

```ts
import { createEvolu, createIdFromString, SimpleName } from "@evolu/common";
import { evoluWebDeps } from "@evolu/web";
import { createMirrorStore, readCommitment, syncLatest, verifyAsOf } from "@evm-scan/mirror";

const Schema = { /* see MIRROR_TABLES in mirror/src/evolu.ts */ };
const evolu = createEvolu(evoluWebDeps)(Schema, {
  name: SimpleName.orThrow("evmscan-mirror"),
  transports: [{ type: "WebSocket", url: "wss://your-relay" }],
});

const REGISTRY = "0x…";     // where HintRegistry is deployed
const INDEXED_CHAIN = 1n;   // the chain the index is *about*

const store = createMirrorStore({ evolu, createIdFromString, registry: REGISTRY, chainId: INDEXED_CHAIN });

// Publisher, on a timer: chain → uri → table → delta. `call` is an eth_call
// against the chain the REGISTRY is on, which is not necessarily INDEXED_CHAIN —
// the live deployment indexes mainnet with its registry on Base, so pointing this
// at the indexed chain calls an address with no code and reads the empty answer as
// "no epoch yet".
const synced = await syncLatest(store, { registry: REGISTRY, chainId: INDEXED_CHAIN, call });

// Wallet: rebuild the root from local rows and compare with the chain.
const commitment = await readCommitment(call, REGISTRY, INDEXED_CHAIN);
const result = verifyAsOf(await store.rows(), await store.coverage(commitment.epochId), commitment);
console.log(result.reason);
```

A wallet that lets someone type a name resolves it through the same `call`, before it
touches a row:

```ts
import { resolveName, reverseName, OffchainNameError } from "@evm-scan/mirror";

const { address, name } = await resolveName(call, "vitalik.eth"); // one eth_call to the Universal Resolver
const primary = await reverseName(call, address, INDEXED_CHAIN);  // null when nothing is claimed
```

`resolveName` throws `OffchainNameError` with the gateway URLs for a CCIP-Read name and
does not follow them; whether to trust a third-party gateway is the wallet's decision.
Nothing a name says reaches the mirror's rows: it becomes an address and that is what
is looked up. Normalization is NFC + lowercase, the same transform as the daemon's
`internal/ens.Normalize` and the page, and the normalized form comes back with the
answer so a name the ENS app would render differently is visible.

The publisher holds a `SharedOwner` (it has the write key); wallets get the
`SharedReadonlyOwner` derived from it with `createSharedReadonlyOwner`, which carries
the id and decryption key and no ability to write. Publishing that to the world is
the intended use — the data is public.

## What is deliberately not built

- **A delta endpoint on the daemon.** The ingest sidecar is the publisher and already
  has the previous state locally, so it diffs against itself. A
  `GET /v1/epochs/{id}/delta` would help non-Evolu HTTP clients and is the obvious
  next step, but nothing here needs it.
- **Incremental verification.** Sorted-pair hashing gives no non-membership proofs,
  so a client checks the whole set or one leaf, never "these 400 rows are the only
  change". RBSR saves bandwidth, not verification work. Fixing that means a
  sorted-key tree with range proofs — a `HintRegistry` change, and `evmscan-verify`
  re-run against the new verifier.
- **Per-user private state.** The Aztec-shaped version of this — a wallet's own
  watchlist or labels, encrypted, committed per user — is a different project. Evolu
  would be native to it; the on-chain half would not be, since per-user roots cost
  gas, leak activity timing, and without nullifiers amount to a notarised backup.
