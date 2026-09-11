-- Reports against an indexed asset, for scan ordering only.
--
-- The spam verdict on `candidates` cannot cover this. A candidate is something
-- discovery found and nobody has paid for; a verdict on one decides whether it gets
-- indexed at all. But an asset someone paid to register through requestIndexing
-- never was a candidate — it enters the index without passing through discovery —
-- so today a bought-in scam carries no verdict of any kind, and nothing orders it
-- below the contracts people actually hold.
--
-- The rule these columns encode is that money buys indexing and does not buy
-- position. An operator cannot un-index a funded asset: the funding was spent on a
-- backfill and the coverage is committed to a root. What they can do is decline to
-- put it first, and that is all this is.
--
-- Which is why one report is enough to sink something. Ordering is not adjudication:
-- getting it wrong costs a contract its place in a list and a reader one extra
-- balance read, where getting it wrong the other way puts a scam token at the top of
-- somebody's wallet. The asymmetry is the whole argument for a low bar here and a
-- high one on `candidates.spam_at`.
ALTER TABLE assets
    ADD COLUMN IF NOT EXISTS reports      INTEGER     NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS reported_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS report_reason TEXT;

-- Reported assets sort last, so the partial index is over the ones that are not.
CREATE INDEX IF NOT EXISTS assets_reported_idx
    ON assets (chain_id)
    WHERE reports = 0;
