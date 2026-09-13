# Product shape

Updated against merged source at `646c062` (2026-09-13), including portable user
state and the unified commit flow. This describes repository behavior, not a
verification of the live deployment. Remaining cleanup proposals are labeled below.

## What the product is

**evm-scan helps a wallet find which contracts to read, reads their current
balances, and remembers the owner's choices so the next read is more useful.**

The shared index learns which accounts touched requested token contracts. The
reader checks balances at head, proposes recognized/unrecognized groups, and lets
the owner correct them. Signed choices guide future indexing and reads. The trie
work makes those choices recoverable outside one browser.

The product serves wallet readers and wallet developers. Operators run the node,
indexer and publisher that supply its shared discovery layer.

## Three kinds of information

| Information | What it means | Authority |
| --- | --- | --- |
| Indexed interactions | Contracts an account touched within scanned history | Node logs; publisher's snapshot can be checked against a finalized registry root |
| Current holdings | What the queried contracts report now | Head-state lens read through the selected node; prices have separate confidence |
| Wallet preferences | Contracts the owner recognizes or wants remembered | Owner's signature; portable state combines both in one revision |

Keep these meanings visible. A signed asset list is not a publisher-verified
interaction list. A registry proof authenticates a published list; it does not
prove complete chain coverage, token legitimacy or today's balance. Failed reads
remain unknown. History floors and bounded discovery limit what can be found.

## The reader loop

1. Enter an address or resolve a name in the browser.
2. Read contracts from the index and the owner's remembered list. Offer a broader
   network sweep when needed; disclose its token-list and RPC limits.
3. Show live holdings, proposed classification and reasons. The owner corrects it.
4. Sign reviewed wallet state. Recognition contributes indexing demand;
   remembered assets seed later reads without requesting a backfill.
5. Reuse or restore that state next time. Keep negative and unrecognized entries
   enumerable, readable and reversible; classification does not change valuation.

All commit links open the portable-state card; the separate asset-commit page has
been removed. The legacy *Sign my verdict* path remains when public state is not
selected; opting in routes that button through full state review. Legacy verdict
and asset-commit APIs remain compatible. Reading an address needs no signature.
Publishing a personal ENS checkpoint is a separate, optional transaction.

When an exact signed asset list exists, the network picker uses its chains and
contracts. Otherwise it starts with chains found in the answer and the active
lookup chain, falling back to the default set if none are available. That discovery
view offers “Show more chains.” Chains lacking an endpoint remain unselected.

## What the trie rewrite actually changes

The merged [portable-state implementation](USER_STATE.md) contains two sparse
binary tries in `internal/userstate`:

- **Per-account state:** keys identify a verdict or remembered asset by chain and
  contract. One signed revision binds the root, previous revision and deadline.
  Immutable snapshots can be exported, verified and imported.
- **Aggregate checkpoint:** account keys map to signed revision identifiers.
  Membership and absence proofs check whether an account is in that checkpoint.
  Optional ENS records anchor an individual revision or the aggregate root.

These are 256-bit keyed trees with distinct leaf, branch and empty hashes and
ordered children. They authenticate wallet state. An absence proof means absent
from that published checkpoint, never “this wallet owns nothing.” An ENS anchor
does not provide the snapshot bytes or guarantee that the operator published the
newest revision. Recovery still needs a backup or available replica.

**The indexed-history tree has not been rewritten.** `internal/merkle/merkle.go`
still builds sorted-pair Merkle trees, and `Publisher.Build` still reads a full
`SnapshotIndex`. Replacing this with a sparse trie is a separate candidate in
[SHIP.md §8](SHIP.md#8-deliberately-not-in-the-ship). It would require a versioned
contract/verifier change and matching Go, gateway, snapshot and mirror formats.
Incremental publication would also need incremental storage and update logic;
changing the tree type alone does not remove full snapshot work.

## Remaining cleanup proposals

| Area | Proposed direction | Current behavior |
| --- | --- | --- |
| Product language | Use “indexed history,” “live holdings,” and “signed wallet state” consistently | The page still calls the exact list a “committed filter,” while `committed` also means registry inclusion |
| Reader actions | Make the relationship between standalone verdicts and public state explicit | Commit links share one card; standalone verdict signing remains an alternative |
| Exclusion semantics | Group and prioritize, preserve visible holdings and undo | `renderCommittedFilter` says “excluded”; `walletTokens` skips speculative candidates when an exact list exists. State the bounded read scope explicitly |
| ENS | Keep checkpoint anchoring distinct from ENS index lookup | State checkpoints use existing ENSv2 Sepolia resolvers; older custom index resolvers remain deferred |
| Deferred features | Keep private filters, watchlists, old vote flows and mirror experiments outside the core reader story | Inventory consumers before deleting code or routes |

The card measures requests and bytes for making a record and reading it back,
including the hosts contacted. These figures cover the measured button actions;
they do not measure the preceding portfolio sweep or establish a scaling law.
See [the measurement limits](USER_STATE.md#what-each-half-costs).

## Operating and trust boundaries

Discovery stores counters; promotion buys the per-account history walk. Signed
demand can trigger promotion subject to thresholds, operator decisions and budgets.
Funding sponsors coverage; it does not certify an asset. The daemon submits,
finalizes and claims rewards through transactions, fronting gas. Publication and
new indexing stop without a running, funded operator.

The repository provides self-host instructions and configurable RPCs. Hosted APIs
and RPCs still observe lookups and can refuse service. Portable state is an explicit
public disclosure, including negative verdicts; the private filter flow is outside
the current UI. Reader signatures do not transfer assets. Registry dispute outcomes
depend on the configured arbiter/oracle; a root is not a fraud proof. Wallet funds
remain usable if the service disappears, but recovering preferences requires data
availability. No repository-level license file was found; resolve that before
claiming unrestricted reuse.

