-- Runtime discovery.
--
-- The original design assumed an indexer is told which contracts to scan and then
-- walks their history to genesis. Both halves of that are wrong in practice:
--
--  * A snap-synced node is not guaranteed to hold history to genesis, and requiring
--    it to would reintroduce the archive-node dependency this project exists to avoid.
--    So a chain now records the oldest block whose logs its node can actually serve,
--    and backfills stop there rather than at block 0.
--
--  * Contracts are not known up front. They are found by watching the head: any
--    contract emitting identity-carrying token events becomes a *candidate*, cheaply
--    counted and nothing more. Only once a candidate is judged worth indexing is it
--    promoted, and only then does its history get walked.
--
-- Candidates deliberately hold aggregate counters and no per-account rows. That is
-- what keeps watching every contract affordable while the expensive per-account index
-- stays bounded to the promoted set.

-- The oldest block this chain's node can serve logs for. 0 means "not probed yet";
-- a probed node with full history also reports 0, which is the same instruction.
ALTER TABLE chains ADD COLUMN IF NOT EXISTS history_floor BIGINT NOT NULL DEFAULT 0;
ALTER TABLE chains ADD COLUMN IF NOT EXISTS history_probed_at TIMESTAMPTZ;

-- Contracts seen at the head that we have not committed to indexing.
CREATE TABLE IF NOT EXISTS candidates (
    chain_id         BIGINT   NOT NULL,
    address          BYTEA    NOT NULL,
    standard         SMALLINT NOT NULL DEFAULT 0,
    first_seen_block BIGINT   NOT NULL,
    last_seen_block  BIGINT   NOT NULL,
    event_count      BIGINT   NOT NULL DEFAULT 0,
    -- Distinct blocks this contract was active in. A contract busy across many blocks
    -- is a different signal from one that emitted a burst in a single block, which is
    -- what a spam airdrop looks like.
    blocks_seen      BIGINT   NOT NULL DEFAULT 0,
    promoted_at      TIMESTAMPTZ,
    promotion_reason TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, address),
    CONSTRAINT candidates_address_len CHECK (octet_length(address) = 20)
);

-- Ranking for the "what should we index next" query.
CREATE INDEX IF NOT EXISTS candidates_rank_idx
    ON candidates (chain_id, event_count DESC, blocks_seen DESC)
    WHERE promoted_at IS NULL;

-- How far the discovery sweep has read. Separate from the per-asset tail cursors:
-- discovery watches every contract, the tail scan watches only promoted ones.
CREATE TABLE IF NOT EXISTS discovery_cursor (
    chain_id   BIGINT PRIMARY KEY,
    last_block BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Where an asset came from, so the API can distinguish a hint someone paid to
-- register from one the indexer promoted on its own.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS promoted_from_candidate BOOLEAN NOT NULL DEFAULT FALSE;

-- The floor a backfill actually stopped at. When this is above the asset's requested
-- from_block, history is incomplete because the node no longer has it, and responses
-- must say so rather than implying full coverage.
ALTER TABLE asset_cursors ADD COLUMN IF NOT EXISTS backfill_floor BIGINT NOT NULL DEFAULT 0;

INSERT INTO schema_migrations (version) VALUES (2) ON CONFLICT DO NOTHING;
