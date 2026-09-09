-- Keep the asset list a leaf was built from, not just its hash.
--
-- The CCIP gateway has to return the exact list that hashes to the committed leaf,
-- because contractsOfCallback recomputes assetsHash over it. Re-deriving the list
-- from the live rollup does not give that back: a backfill walking an asset toward
-- its floor keeps inserting interactions whose first_block is inside an already
-- committed range, so the list grows after the commitment and no longer matches.
-- Storing it makes the gateway's answer correct by construction.
ALTER TABLE epoch_leaves ADD COLUMN IF NOT EXISTS assets BYTEA[];

INSERT INTO schema_migrations (version) VALUES (5) ON CONFLICT DO NOTHING;
