-- Durable submission references.
--
-- A commitment used to go straight from `built` to `published` in one call: sign,
-- send, wait for the receipt, record it. If the process died or the receipt wait timed
-- out between send and record, the row stayed `built` and the next auto-publish tick
-- posted the same root again, burning a second bond and leaving an orphaned on-chain
-- epoch nobody tracked.
--
-- Recording the submission reference (tx hash, or whatever the submitter hands back)
-- *before* waiting lets a restart resume the wait instead of resubmitting. `failed`
-- is for submissions that were never confirmed within the stale window.
ALTER TABLE epochs ADD COLUMN IF NOT EXISTS submission_ref BYTEA;
ALTER TABLE epochs ADD COLUMN IF NOT EXISTS submitted_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS epochs_pending_idx ON epochs (chain_id, id) WHERE status = 'submitted';

INSERT INTO schema_migrations (version) VALUES (3) ON CONFLICT DO NOTHING;
