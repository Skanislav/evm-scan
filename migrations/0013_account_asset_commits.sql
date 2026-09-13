-- A reader's signed exact asset list.
--
-- Unlike account_hints, this is enumerable: it records the exact (chain, contract)
-- pairs a wallet sweep found. The account explicitly signs that disclosure so a later
-- lookup can query those pairs first instead of spending calls rediscovering them.
-- A commit replaces the prior list atomically; its deadline only rises, preventing an
-- older signature from restoring stale holdings.

CREATE TABLE account_asset_commits (
    account   BYTEA       PRIMARY KEY,
    digest    BYTEA       NOT NULL, -- keccak256(sorted(chain_id || address) pairs), what the wallet signed
    deadline  BIGINT      NOT NULL, -- unix seconds; monotonic per account
    signed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE account_asset_commit_items (
    account  BYTEA   NOT NULL REFERENCES account_asset_commits(account) ON DELETE CASCADE,
    chain_id BIGINT  NOT NULL,
    asset    BYTEA   NOT NULL,
    PRIMARY KEY (account, chain_id, asset)
);

CREATE INDEX account_asset_commit_items_account_chain_idx
    ON account_asset_commit_items (account, chain_id);
