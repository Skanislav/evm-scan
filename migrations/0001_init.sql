-- evm-scan schema.
--
-- Design notes that the table shapes depend on:
--
--  * Addresses are stored as 20-byte BYTEA, not hex text. Half the width, and it
--    makes the (chain_id, account) index actually compact.
--
--  * `interactions` is a rollup, not an event log. Storing every event for a hot
--    token would defeat the point of an allowlisted index, so raw events are kept
--    only for the unconfirmed window (`pending_events`) and folded into the rollup
--    once they are deep enough to be final. A reorg can therefore only ever touch
--    `pending_events`, which is safe to delete and rescan; the rollup is never wrong.
--    A reorg deeper than the configured confirmation lag is out of scope, as it is
--    for every indexer of this shape.

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INT PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Chains
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS chains (
    chain_id    BIGINT PRIMARY KEY,
    name        TEXT        NOT NULL,
    head_block  BIGINT      NOT NULL DEFAULT 0,
    head_hash   BYTEA,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Asset hints (the allowlist that bounds all scanning)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS assets (
    chain_id        BIGINT      NOT NULL,
    address         BYTEA       NOT NULL,
    standard        SMALLINT    NOT NULL DEFAULT 0,   -- evmlog.Standard
    symbol          TEXT,
    name            TEXT,
    decimals        SMALLINT,
    -- Lower bound for the backfill. Registrant-supplied and untrusted; the scan
    -- simply stops here rather than walking to genesis.
    hint_from_block BIGINT      NOT NULL DEFAULT 0,
    registrant      BYTEA,
    registry_key    BYTEA,                            -- keccak256(chainId, token)
    source          TEXT        NOT NULL DEFAULT 'local',  -- 'onchain' | 'local'
    status          TEXT        NOT NULL DEFAULT 'pending', -- pending|scanning|live|revoked
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, address),
    CONSTRAINT assets_address_len CHECK (octet_length(address) = 20)
);

CREATE INDEX IF NOT EXISTS assets_status_idx ON assets (chain_id, status);

-- Two-pointer scan progress: forward from the registration anchor (fresh data
-- immediately) and backward toward the deploy block (history, eventually).
CREATE TABLE IF NOT EXISTS asset_cursors (
    chain_id       BIGINT   NOT NULL,
    address        BYTEA    NOT NULL,
    anchor_block   BIGINT   NOT NULL,
    backfill_next  BIGINT   NOT NULL,   -- highest block not yet scanned, walking down
    backfill_done  BOOLEAN  NOT NULL DEFAULT FALSE,
    tail_block     BIGINT   NOT NULL,   -- highest block folded into the rollup
    logs_seen      BIGINT   NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, address),
    FOREIGN KEY (chain_id, address) REFERENCES assets (chain_id, address) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS asset_cursors_backfill_idx
    ON asset_cursors (chain_id, backfill_done)
    WHERE backfill_done = FALSE;

-- ---------------------------------------------------------------------------
-- The discovery index
-- ---------------------------------------------------------------------------

-- One row per (chain, account, asset): "this account touched this contract".
-- This is the whole product: a wallet reads it to learn which contracts are worth
-- pulling history for, then verifies against the chain itself.
CREATE TABLE IF NOT EXISTS interactions (
    chain_id    BIGINT   NOT NULL,
    account     BYTEA    NOT NULL,
    asset       BYTEA    NOT NULL,
    first_block BIGINT   NOT NULL,
    last_block  BIGINT   NOT NULL,
    event_count BIGINT   NOT NULL DEFAULT 0,
    roles       INT      NOT NULL DEFAULT 0,  -- bitmask over evmlog.Role
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, account, asset),
    CONSTRAINT interactions_account_len CHECK (octet_length(account) = 20),
    CONSTRAINT interactions_asset_len   CHECK (octet_length(asset) = 20)
);

CREATE INDEX IF NOT EXISTS interactions_account_idx
    ON interactions (chain_id, account, last_block DESC);
CREATE INDEX IF NOT EXISTS interactions_asset_idx
    ON interactions (chain_id, asset);

-- Raw events awaiting confirmation. Bounded by the confirmation lag, so this stays
-- small even for hot contracts.
CREATE TABLE IF NOT EXISTS pending_events (
    chain_id     BIGINT   NOT NULL,
    block_number BIGINT   NOT NULL,
    block_hash   BYTEA    NOT NULL,
    log_index    INT      NOT NULL,
    asset        BYTEA    NOT NULL,
    account      BYTEA    NOT NULL,
    role         SMALLINT NOT NULL,
    PRIMARY KEY (chain_id, block_number, log_index, account, role)
);

CREATE INDEX IF NOT EXISTS pending_events_block_idx
    ON pending_events (chain_id, block_number);

-- ---------------------------------------------------------------------------
-- Optimistic index commitments
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS epochs (
    id          BIGSERIAL PRIMARY KEY,
    chain_id    BIGINT   NOT NULL,
    from_block  BIGINT   NOT NULL,
    to_block    BIGINT   NOT NULL,
    merkle_root BYTEA    NOT NULL,
    leaf_count  BIGINT   NOT NULL,
    uri         TEXT     NOT NULL DEFAULT '',
    onchain_id  BIGINT,                         -- epoch id inside HintRegistry
    tx_hash     BYTEA,
    status      TEXT     NOT NULL DEFAULT 'built', -- built|published|finalized|rejected
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS epochs_chain_idx ON epochs (chain_id, id DESC);

-- Leaves are retained so the API can serve inclusion proofs without recomputing
-- the whole index at the epoch's block range.
CREATE TABLE IF NOT EXISTS epoch_leaves (
    epoch_id    BIGINT NOT NULL REFERENCES epochs (id) ON DELETE CASCADE,
    idx         INT    NOT NULL,
    account     BYTEA  NOT NULL,
    assets_hash BYTEA  NOT NULL,
    leaf        BYTEA  NOT NULL,
    PRIMARY KEY (epoch_id, idx)
);

CREATE INDEX IF NOT EXISTS epoch_leaves_account_idx ON epoch_leaves (epoch_id, account);

-- ---------------------------------------------------------------------------
-- HintRegistry mirror progress
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS registry_sync (
    registry_chain_id BIGINT NOT NULL,
    registry_address  BYTEA  NOT NULL,
    last_block        BIGINT NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (registry_chain_id, registry_address)
);

INSERT INTO schema_migrations (version) VALUES (1) ON CONFLICT DO NOTHING;
