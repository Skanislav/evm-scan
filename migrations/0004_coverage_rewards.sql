-- Coverage commitments and per-asset rewards.
--
-- A commitment now carries a second root over the per-asset block ranges it stands
-- behind. The registry pays the publisher per newly covered block of each funded
-- asset, claimed after finalization against that root, so the leaves have to be kept
-- to build the claim proofs after a restart.
ALTER TABLE epochs ADD COLUMN IF NOT EXISTS coverage_root BYTEA;
-- What the registry said the coverage was worth when the epoch was built, so the
-- decision to post it can be audited later.
ALTER TABLE epochs ADD COLUMN IF NOT EXISTS expected_reward_wei TEXT;
ALTER TABLE epochs ADD COLUMN IF NOT EXISTS claim_tx BYTEA;
ALTER TABLE epochs ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ;
ALTER TABLE epochs ADD COLUMN IF NOT EXISTS reward_wei TEXT;

CREATE TABLE IF NOT EXISTS epoch_coverage (
    epoch_id     BIGINT NOT NULL REFERENCES epochs (id) ON DELETE CASCADE,
    idx          INT    NOT NULL,
    asset        BYTEA  NOT NULL,
    registry_key BYTEA  NOT NULL,
    from_block   BIGINT NOT NULL,
    to_block     BIGINT NOT NULL,
    leaf         BYTEA  NOT NULL,
    PRIMARY KEY (epoch_id, idx)
);

CREATE INDEX IF NOT EXISTS epochs_unclaimed_idx
    ON epochs (chain_id, id) WHERE status = 'finalized' AND claimed_at IS NULL;

INSERT INTO schema_migrations (version) VALUES (4) ON CONFLICT DO NOTHING;
