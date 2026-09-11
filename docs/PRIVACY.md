# Privacy

This repo has been careful about trust and careless about confidentiality, and the
two are not the same thing. `docs/CLIENT-SIDE.md` argues about who does the work and
who can lie; this file is about who can *watch*.

## The gap

`HintRegistry.contractsOf` reverts with `OffchainLookup`, the gateway answers, and the
callback re-derives the leaf and checks it against the on-chain root. As the contract
puts it: a gateway can withhold an answer but cannot forge one. That is a real
property and it is the wrong axis.

Every way to ask this system what an account holds names the account:

| path | who learns the address |
|---|---|
| `GET /v1/accounts/{address}/*` | the operator |
| `GET /v1/epochs/{id}/proof?account=` | the operator |
| `GET /ccip/{sender}/{data}` | the gateway operator, unpacked from calldata |

All three are unforgeable. None is unobservable. A gateway that cannot lie to you can
still keep a list of everyone who asked about what.

The data itself is public — it came from logs anyone can read. What is not public is
**the correlation**: which addresses one person cares about, asked in one session,
from one IP. Nothing else in the system protects that, so this is the first thing that
does.

## Tier 4 — filters: shipped

A `.xorf` file is a membership test over 64-bit keys. `internal/hintfilter` writes it,
`web/index.html` reads it, and `internal/hintfilter/testdata` is the fixture that
stops the two drifting — the same discipline `internal/merkle` keeps against the
Solidity verifier, for the same reason.

The privacy argument is short. The file is **static and identical for every visitor**,
so fetching it says nothing about who is fetching, and the test runs in the reader's
browser. The daemon serves one cached blob and never learns the question.

```
GET /v1/hints/tokens-1.json   ->  the candidate addresses (a filter cannot be walked)
GET /v1/hints/index-1.xorf    ->  the membership filter
                                  test locally; the address never leaves the machine
                              ->  survivors go to AssetLens against the reader's own RPC
```

Two numbers, and only one of them is measured.

**Token filter, measured:** the CoinGecko Uniswap list is 5,828 mainnet tokens in
**7,750 bytes** — 1.33 bytes per token, built from the live list and byte-identical
across two independent fetches.

**Index filter, measured** against the hosted mainnet index, epoch 7 (on-chain epoch
4), built from its published snapshot:

| | |
| --- | --- |
| accounts | 773,948 |
| (account, contract) pairs | 857,417 |
| filter | **974,918 bytes** — 1.14 bytes per key |
| the snapshot it was built from | 50,102,272 bytes gzipped |
| ratio | **51× smaller** |

That ratio is the argument. `GET /v1/epochs/{id}/snapshot` is already a private
client-side path — download the whole table, match locally, check both roots against
the chain — and it costs 50 MB. The filter answers the one question a portfolio read
actually asks for under a megabyte, and a reader can rebuild it from the same
snapshot to check that the served one matches:

```bash
evmscan-hint build -index -snapshot https://<host>/v1/epochs/7/snapshot -o index.xorf
```

**Why this is safe:** nothing is written and nothing is trusted. A filter says where
to look; the live read through the lens says what is there. A stale or lying filter
costs a wasted `balanceOf` and can never produce a wrong balance.

### Blinding

Keys can be derived under a secret the reader holds:

```
subkey = keccak256(secret ‖ "evmscan/xorf/v1" ‖ uint64be(chainId) ‖ uint8(kind))
key(x) = uint64be(keccak256(subkey ‖ x)[0:8])
```

A filter can be **tested** but never **enumerated**, and that inverts usefully:

- **You**, holding the secret, read your own filter by walking a public token list and
  testing each entry. The portfolio flow walks that list anyway, so retrieval is free.
  The dictionary attack *is* the read path.
- **Everyone else** holds the same file and the same public list and gets nothing,
  because every key depends on a secret they do not have.

So a blinded `.xorf` is a private watchlist that a dumb, untrusted host can store.
Publish it to GitHub, put it on IPFS, hand it to this daemon; the host sees an opaque
table of fingerprints. The set is encrypted at rest by construction rather than by a
wrapper around it, and the key never reaches the host.

### Where the secret comes from

| provider | strength | portable across deployments |
|---|---|---|
| WebAuthn `prf` (passkey) | 32 bytes of authenticator entropy | **no** — bound to the origin |
| `personal_sign` of a fixed constant | as strong as the wallet seed | yes |
| password through PBKDF2 | as strong as the password | yes |

**Passkey** is the strongest and the least portable. WebAuthn binds a credential to an
RP ID, so a watchlist blinded on a hosted UI cannot be opened on `localhost` or on
your own daemon. For a project whose premise is self-hosting that is a real limit, and
the UI has to present it where the reader chooses rather than after they have built
something they cannot reopen.

**Wallet** is the portable one. Ledger and Trezor both sign with RFC 6979, so a fixed
message yields the same bytes every time and `keccak256(signature)` is a stable
secret. The message is a bare constant — `evmscan/xorf/v1`, no origin, no nonce — and
that is deliberate: folding in the origin would stop another site harvesting the same
secret, and would also make a filter built on one deployment unreadable on another.
The reader is deriving a key, not authenticating a session, so replay is not the
threat; portability wins. This also makes BIP-39 passphrases work for free — a hidden
wallet gives a different address, a different signature, and therefore a different
filter, partitioning watchlists with no extra mechanism.

**Password** is compatibility, and the weakest by a distance. See below.

### What is actually wired up

Be clear about the seam between "implemented" and "reachable", because the gap is
where a reader would otherwise assume a protection they do not have:

| piece | state |
|---|---|
| filter format, both structures | shipped, cross-verified Go ↔ JS |
| token filters from config, served and cached | shipped |
| `index-{chain}.xorf` | shipped; built and cross-checked against the hosted mainnet index |
| epoch-bound digest (`epochs.filter_keccak`, `Publisher.Build`) | shipped |
| `GET /v1/epochs/{id}/manifest` | shipped |
| `evmscan-verify -filter` | shipped; needs a node, a registry and a finalized epoch to say anything |
| `text(node, "evmscan.uri")` on HintResolver | shipped; **no deployment to read it from yet** |
| cross-chain sweep over viem's chain registry | shipped; measured below |
| unlisted-contract marking in the account table | shipped |
| `passkeySecret` / `walletSecret` / `passwordSecret` / `buildWatchlist` | shipped, with a
  UI: build from the holdings on screen, open a file back. Both directions pinned by
  `testdata/browser-watch.xorf` (prf) and `testdata/browser-watch-pbkdf2.xorf` (password) |
| private lookup: the index filter tested in the browser instead of `/v1/accounts` | shipped;
  cross-checked against the hosted mainnet index, same contracts, address never sent |

Blinded watchlists are now reachable without devtools, and so is the index filter: the
wallet tab can answer "which indexed contracts has this account touched" from a file it
downloaded, without the daemon learning the address. The passkey path still cannot be
exercised headlessly, so the fixture that pins the format end to end is the password one.

## What this does not solve

- **A published blinded filter is an offline oracle against its own secret.** Guess a
  secret, derive the subkey, test a thousand popular tokens: a hit rate far above the
  filter's 0.4% confirms the guess, in microseconds. Against 32 bytes of authenticator
  entropy that is hopeless. Against a human password it is only as hard as the KDF
  makes it, which is why the password path uses PBKDF2 at 600,000 iterations and why
  even that is the weak option — WebCrypto has no Argon2id and this page ships no
  vendored WASM. **A weak password plus a published file is not private.** Use a
  passkey.

- **Size and shape leak.** `count` is in the header and the file length follows from
  it. A blinded watchlist hides *which* tokens, never *how many*. Padding to a bucket
  would fix it; it is not built.

- **Lose the secret, lose the read.** There is no recovery path and there should not
  be one — a recoverable blinding is not blinding. Passkeys sync through their
  platform keychain; a wallet-derived secret lives as long as the seed does.

- **The digest is only as good as the URI's scheme.** An epoch commits a digest of
  its filter into a manifest, and names that manifest's URI inside the same bonded
  `publishIndex` transaction as the root. Over `ipfs://` the URI *is* the content, so
  the commitment fixes the document. Over `https://` it fixes only the address: the
  publisher committed to naming that URL, not to what it serves, and a deployment
  answering for its own artifact is the circularity the digest existed to break.
  `evmscan-verify -filter` prints which one it followed, every time. Only the IPFS
  form actually closes the loop, and nothing here publishes to IPFS yet.

- **Omission, unchanged from `docs/CLIENT-SIDE.md`.** A publisher can under-populate a
  filter and no client can detect it locally: `Contains` returning false is
  indistinguishable from "was never inserted". `index-{chain}.xorf` is exactly as
  honest as the daemon serving it. The digest is published in `/v1/hints` and as an
  `ETag` so a file can be pinned and compared across mirrors, but nothing yet commits
  it on chain next to the epoch root — until that exists, do not let the word
  *verifiable* drift across from the merkle path, where it is earned.

- **Staleness is a false negative.** A pair indexed after a filter was built is a
  holding the filter will hide. Two filters exist for this reason and they answer
  different questions. The epoch-bound one is fixed at the block a publisher bonded,
  so it is checkable and behind; the rolling one is current and vouched for by
  nobody. `/v1/hints/index-{chain}.xorf` serves the first when a finalized epoch
  exists and the second otherwise, and the header says which (`epochId` is `-1` for
  the rolling one). A reader that does not show "as of block N" cannot tell "you hold
  nothing" from "nothing was indexed yet".

- **It is not an authorization boundary.** It hides a set from a host. It does not
  stop anyone who already knows an address from watching that address on chain.

- **A filter answers about its own set, and nothing else.** The index filter was
  briefly used to narrow the cross-chain sweep, which looked obviously right and was
  badly wrong: the index is bounded by the promoted asset set, so a miss means "not
  indexed", not "no balance". On the hosted mainnet deployment — eight promoted
  assets — that turned 65 real holdings into 1, silently, because a skipped read is
  indistinguishable from a zero. A filter cannot have a false negative about the set
  it was built over; it can only be asked about the wrong set. Narrowing is sound
  where the index is authoritative for the question asked, which is the wallet
  lookup, not a token-list sweep.

- **A cross-chain sweep leaks to every endpoint it touches.** "Elsewhere" reads one
  RPC per chain, and each one sees the address being asked about. Selecting every
  available chain sends it to around twenty endpoints this deployment has no
  relationship with — viem's public defaults. That is a `docs/CLIENT-SIDE.md` Tier 1
  trade, the reader's own read against a node of their choosing, and it is legitimate
  as long as it is not a surprise: the panel states it before the sweep runs, counts
  how many of the selected endpoints are not the reader's, offers a per-chain
  override, and names the host that answered on every row. Nothing about the sweep
  reaches this daemon.

- **The rest of the page still leaks.** Name resolution goes to a public mainnet RPC,
  disclosed inline. `/v1/accounts/{address}` is still the default path. Filters are
  an option a reader turns on, not a property the deployment has.

## Reproducing the artifacts

Everything published here is deterministic — the same input gives byte-identical
output, verified against a live remote list across two independent fetches — so a
reader can rebuild a filter and diff it rather than trusting the digest:

```bash
evmscan-hint build -tokenlist https://tokens.coingecko.com/uniswap/all.json -chain 1 -o tokens.xorf
evmscan-hint inspect -f tokens.xorf          # manifest + digest check
evmscan-hint test -f tokens.xorf 0xA0b8...   # membership, from a shell
```

A file nobody can diff against its inputs is not a public artifact, it is a blob.
