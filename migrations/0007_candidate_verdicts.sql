-- A verdict on a candidate, not just a promotion.
--
-- Discovery ranks by raw activity, and the loudest contract on any chain is usually an
-- airdrop spraying a worthless token at every address it can find. Marking one spam is
-- the operator saying "never offer this again": it drops out of the ranking, out of
-- auto-promote, and into the decision ledger as a recorded call rather than a silent
-- skip. The counters keep accumulating — discovery still watches every contract, and a
-- verdict is a judgement about what to index, not a filter on what to observe.
ALTER TABLE candidates ADD COLUMN IF NOT EXISTS spam_at     TIMESTAMPTZ;
ALTER TABLE candidates ADD COLUMN IF NOT EXISTS spam_reason TEXT;

-- The promotable ranking must not surface a contract someone has already judged. This
-- clause is the whole of it: auto_promote reads PromotableCandidates and nothing else,
-- so the rule lives in SQL and has no Go-side twin to fall out of step with.
DROP INDEX IF EXISTS candidates_rank_idx;
CREATE INDEX IF NOT EXISTS candidates_rank_idx
    ON candidates (chain_id, event_count DESC, blocks_seen DESC)
    WHERE promoted_at IS NULL AND spam_at IS NULL;

-- The decision ledger reads newest verdict first over the judged rows only. A candidate
-- carries at most one live verdict (promotion clears a spam mark), so COALESCE is a
-- total ordering key rather than a tie-break — which is what lets /v1/decisions be one
-- ordered scan instead of a union of two branches.
CREATE INDEX IF NOT EXISTS candidates_verdict_idx
    ON candidates (chain_id, COALESCE(promoted_at, spam_at) DESC)
    WHERE promoted_at IS NOT NULL OR spam_at IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (7) ON CONFLICT DO NOTHING;
