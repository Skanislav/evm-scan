-- pending_events: make the buffer key carry the block hash.
--
-- The old key was (chain_id, block_number, log_index, account, role). A reorg that
-- replaces a log with a different one at the same (block, log_index) — the normal
-- shape of a re-mined block — collides with the row the reorged-out event left, and
-- ON CONFLICT DO NOTHING drops the new event. The buffer then holds an event the
-- canonical chain never had, which PromotePending folds into the rollup.
--
-- block_hash joins the key, so the same position under a different hash is a
-- different row. Idempotence is unaffected: re-scanning the same canonical block
-- still conflicts, because the hash matches. handleReorg clears rows whose hash no
-- longer matches before the follower rescans, so no stale row survives.
ALTER TABLE pending_events DROP CONSTRAINT pending_events_pkey;
ALTER TABLE pending_events
    ADD PRIMARY KEY (chain_id, block_number, block_hash, log_index, account, role);

-- Interactions read paths.
--
-- AssetHolders orders by last_block DESC within one asset, and the graph page ranks
-- an asset's accounts by activity. (chain_id, asset) alone forced a sort of every
-- holder on every read; the extended index serves the order directly.
DROP INDEX IF EXISTS interactions_asset_idx;
CREATE INDEX IF NOT EXISTS interactions_asset_idx
    ON interactions (chain_id, asset, last_block DESC, account);

-- GraphMemberships ranks assets by logs_seen, the counter the follower already
-- maintains. Without this index the ranking sorts the full cursor table on every
-- page load.
CREATE INDEX IF NOT EXISTS asset_cursors_logs_seen_idx
    ON asset_cursors (chain_id, logs_seen DESC);