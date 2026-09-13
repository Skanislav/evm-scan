# Portable user state

Portable state is a public, opt-in backup of an account's cross-chain verdicts and
remembered asset list. One account signature authorizes both storage and indexing
demand. Asset-list entries only prioritize subsequent reads. Negative verdicts
remain enumerable and remain in live balance reads.

The wallet's **Portable wallet memory** card is available even when the index has
no holdings. Review the complete list, adjust verdicts, and sign it. Download a
backup after signing. Browser storage is a cache; the server keeps immutable signed
snapshots. Neither a snapshot nor an ENS commitment asserts a current balance.

## Format v1

`web/userstate.mjs` and `internal/userstate` independently implement this format.
The hash is Keccak-256 (not NIST SHA3-256). JSON is transport, not the hash preimage.
Hex case and entry insertion order do not affect roots. Integer fields in the
portable format are canonical unsigned decimal strings, avoiding JavaScript number
rounding. Addresses are exactly 20 bytes and hashes exactly 32 bytes.

Each entry has `kind`, `chain_id`, `address`, and (only for verdicts) `weight`:

- `verdict`: namespace byte `01`, weight byte `01` or `ff` (+1 or -1).
- `asset`: namespace byte `02`, value byte `01`; no weight.
- Key: `keccak256(namespace || uint64be(chain_id) || address)`.
- Leaf: `keccak256(00 || key || value)`.
- Branch: `keccak256(01 || leftHash || rightHash)`; never sort the children.
- Empty leaf at depth 256: `keccak256(02)`.
- Empty subtree at depth d: `branch(empty[d+1], empty[d+1])`.

Paths traverse all 256 key bits, most significant bit first. Empty subtrees are
implicit. Duplicate keys and unknown namespaces are rejected. Absence means no
entry; a zero verdict is removed before signing. An empty or null entries array
represents the empty trie. Nodes are immutable and addressed by their hash.

Limits: 200 verdicts per chain, 200 remembered assets across chains, 2,000 entries,
256 KiB per state API write. Chain IDs must be positive signed-64-bit values, matching
the existing database. Live admission deadlines must fit Unix seconds in int64.

The snapshot carries `version: 1`, `account`, `state_root`, `revision`, `previous`,
`deadline`, `entries` and the 65-byte EOA `signature`. The signature is EIP-712:

```
domain: { name: "evm-scan state", version: "1" }
State(address account, bytes32 stateRoot, uint64 revision,
      bytes32 previous, uint256 deadline)
```

The EIP-712 digest is the revision identifier. It commits to the state root and
metadata, not to signature bytes. Require canonical low-s signatures. There is no
chain or verifying contract in the domain: state is cross-chain and portable
between servers. Version 1 is EOA-only, like the existing verdict endpoint.

Revision 1 has a zero previous identifier; successors increment by one and name the
prior signed revision. A fresh deadline is required for a new live admission, but
expiry does not invalidate archived state. An identical retry is idempotent. The
API also requires the generation observed before review, rejecting concurrent
legacy writes as well as concurrent state revisions.

## API and legacy compatibility

| Endpoint | Behavior |
| --- | --- |
| `GET /v1/accounts/{address}/state` | `{state: {id, snapshot, generation, legacy_changed}, projection}`; 404 before opt-in. Projection exposes current verdicts and assets for review, not as a signed snapshot. |
| `POST /v1/accounts/{address}/state` | `{snapshot, generation}`; verifies account signature, accepts a successor, atomically projects verdicts and assets. Reader-authenticated; no operator token. |
| `GET /v1/state/revisions/{id}` | Immutable signed snapshot. |
| `POST /v1/state/checkpoints` | Build a complete offchain aggregate export. Operator-authenticated; sends no transaction. |
| `GET /v1/state/checkpoints/{root}` | Complete checkpoint with accounts and signed snapshots. |
| `GET /v1/state/checkpoints/{root}?account=0x...` | Membership or absence proof for that account. |
| `GET /v1/state/status` | Configured ENS target and latest publication jobs; no keys or RPC credentials. |

Legacy verdict and asset-commit writes remain accepted. After opt-in they increment
the account's generation and mark its projection changed. They never mutate an
archived revision or manufacture a signature. A new state signature explicitly
replaces the current projections; the review includes intervening legacy changes.
Legacy replay deadlines never decrease. Aggregate publication includes the latest
**signed snapshot**, which may differ from the legacy live projections.

A new snapshot replaces the full cross-chain verdict set. The browser preserves
unseen rows and other chains, and presents them for review. Exact asset lists keep
the existing sorted-pair digest in legacy responses. Signing an empty snapshot
retracts the account's verdicts and clears its remembered asset list, without
un-indexing any contract or changing live valuation.

Existing browser verdict memory and database asset commits do not retain the old
signatures. They can seed a review but require a new signature. No automatic bulk
migration claims to authenticate historical data.

## Two ENSv2 Sepolia flows

Both flows use the existing resolver's `setText`; no custom contract is deployed.
The ENS registry locates the resolver; application records are stored in that
resolver. Current ENSv2 documentation describes the Sepolia test deployment and
warns its interfaces can change:

- [ENSv2 overview](https://docs.ens.domains/ensv2/overview/)
- [Permissioned Resolver and record-scoped roles](https://docs.ens.domains/ensv2/permissioned-resolver/)

### Manual account checkpoint

The reader supplies their Sepolia RPC and an existing ENSv2 name. The browser
resolves it through the Universal Resolver, reads the record directly, estimates
gas (which also checks write permission), and asks the connected account to send
`setText(namehash, "evmscan.state", "evmscan:state:1:<revision-id>")`.

The revision identifier binds the actual trie root. The browser waits for twelve
confirmations and checks the record again. Restore reads the onchain record at
head, downloads the identified revision from the current server, and verifies its
signature and root. If that server lacks the snapshot, import a backup or use a
replica. An alias that rewrites the target or an offchain-only resolver is refused.

### Automatic aggregate checkpoint

The operator owns one ENSv2 name. A second sparse trie maps
`keccak256(UTF8("evmscan/accounts/v1") || account)` to the 32-byte signed revision
identifier. The same leaf/branch/empty encoding applies. Its root is written as:

```
setText(namehash, "evmscan.states", "evmscan:states:1:<aggregate-root>")
```

A proof has 256 sibling hashes ordered root-to-leaf. Its key must equal the expected
account key. `value` is base64 bytes in Go's JSON encoding; null proves absence.
Clients verify the aggregate proof, account signature and individual state root.
The ENS root proves what the operator published, not that the operator included
every submitted account or published the newest possible revision.

Configure the daemon (no private key in YAML):

```yaml
state:
  enabled: true
  name: states.your-name.eth
  resolver: "0x..."                  # that name's existing ENSv2 resolver
  # namehash: "0x..."               # optional; derived and checked against name
  node: http://127.0.0.1:8545        # Sepolia; override with EVMSCAN_STATE_NODE
  require_local_node: true
  interval: 10m
  max_gas: 300000                   # cap, not an estimate
  max_fee_wei: "10000000000"        # maximum gas price per gas unit
```

Set `EVMSCAN_STATE_PUBLISHER_KEY` to a dedicated, funded Sepolia sender. It must not
be the existing index publisher's sender. Using ENSv2's resolver permissions, grant
this sender **only** `evmscan.states` on the operator name via
`authorizeTextRoles(dnsEncodedName, "evmscan.states", sender, true)` from its admin.
The daemon does not register names, grant itself roles, or resolve account names.
The configured resolver must continue to be the resolver serving the configured
name; after a resolver migration, update the config. The browser verifies through
ENS rather than trusting the daemon's configured resolver.

Automatic publication is disabled by default. When enabled it checks every ten
minutes (configurable), skips unchanged roots and polls pending receipts every ten
seconds. Every tick has a 45-second timeout. Gas estimation must succeed; there is
no guessed-gas fallback. Gas and price caps defer publication rather than increase
spending. Maximum transaction cost is bounded by their product.

The complete checkpoint is saved before signing. Signed transaction bytes and hash
are saved before broadcast. Restarts rebroadcast the same bytes and reconcile the
receipt, serializing sender use with a database advisory lock. Twelve descendant
blocks, a canonical receipt and matching head readback are required before marking
confirmed. Disappearing receipts revoke that status. A reverted receipt is kept as
failed. Fee-stuck transactions remain pending; v1 does not automatically replace
fees or discard a nonce. The dedicated key must not be used by another process or
a different database.

## Export and replacement-server recovery

```sh
make build
bin/evmscan-state -in wallet-backup.json
bin/evmscan-state -in wallet-backup.json -import
bin/evmscan-state -export-checkpoint > checkpoint.json
bin/evmscan-state -in checkpoint.json
bin/evmscan-state -in checkpoint.json -import
```

Database operations use `EVMSCAN_DATABASE_DSN`; they migrate that target database
first. Verification alone needs no database or node. Imports restore immutable
history and nodes only, never live heads or indexing demand. Even an expired
admission signature can be restored and checked. On an empty replacement server,
a fresh signed successor can name an imported previous revision to establish a
new live head. On an existing server the live head always controls succession;
importing cannot roll it back.

A complete aggregate export contains every referenced account snapshot. The CLI
rebuilds each state root, checks every signature and reconstructs the aggregate
root. It refuses incomplete exports. Exports up to 256 MiB are supported by the CLI.
Current exports reproduce state without requiring the entire edit history.

Browser backups are retained when a server is unavailable, corrupt, older or on a
different branch. Explicit backup import or **Use latest server snapshot** selects
a different history only after a confirmation. Snapshots separated by multiple
unavailable revisions require explicit selection rather than an assumed ancestry.
Manual and aggregate ENS checkpoints remain separately labeled; neither silently
supersedes the browser's newer signed state.

## Verification

```sh
go test ./...
go test ./internal/store -run TestUserState -state-dsn 'postgres://.../scratch'
go test ./internal/statepub -state-pub-dsn 'postgres://.../scratch'
npm ci --prefix web/tests
npm --prefix web/tests test
# Install Chromium with the test package's Playwright, or set
# PLAYWRIGHT_CHROMIUM_EXECUTABLE to an installed Chromium binary.
npm --prefix web/tests run test:browser
```

The explicit database test flags create and discard isolated schemas in the supplied
database. Browser tests use real Chromium with a mocked server, wallet and ENS RPC;
no transaction is sent to a network. Fixtures can be deliberately regenerated with
`go test ./internal/userstate -update-state`, then checked with the browser codec.
Real ENSv2 Sepolia publication requires operator-supplied names, permissions, RPC
and funded keys; no deployment is bundled or implied by the tests.

## Architecture review

Chosen default: publicly authorized snapshots, independent account signatures,
manual user checkpoints plus operator batch checkpoints, exports and replicas.

- Censorship resistance: the server can withhold data and the operator can delay
  inclusion. Browser backups and replica imports provide an exit; a root alone
  does not provide availability. No IPFS or automatic replication is promised.
- Open implementation: the format, Go/JavaScript codecs, fixtures, API and restore
  command are included. No repository license has been supplied; this change does
  not choose one on the owner's behalf or claim unrestricted redistribution rights.
- Privacy: opted-in account/token pairs and verdicts become public. Hashing token
  keys does not hide predictable entries. Existing non-opted-in verdicts remain
  blinded; their per-account projection is not exposed by the new read endpoint.
- Security: signatures authenticate user state; ENS owners and resolver admins
  control checkpoint records and may change resolver implementations. The publisher
  uses a separate key with one-record authority. It cannot forge user signatures.
  Public state contains no wallet spending approval.

Accepted limitations: EOA signatures, public disclosure, operator inclusion control,
ENS name/resolver authority, retained-copy availability, explicit conflict selection,
and a testnet-only publication target. No garbage collection is implemented: all
accepted snapshots and published checkpoints are retained.
