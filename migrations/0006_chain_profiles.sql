-- Chains become runtime state.
--
-- `chains` was a head tracker: a name, a block number, a history floor. Which
-- chains exist lived in the YAML, which meant adding a network was a file edit and
-- a restart. This turns the table into the record of what the deployment runs, so a
-- chain can be added while it is up and still be there after it is not.
--
-- Two of these columns carry more weight than the rest.
--
--  * `node_url` is a secret. It is where a provider's API key lives, and it is
--    stored only so a restart can bring the chain back — never returned unredacted,
--    and still overridable by EVMSCAN_NODE_<chain id> so a key can be rotated
--    without touching the database.
--
--  * `trust` decides whether this chain's data may back a bonded commitment. It is
--    derived on creation (loopback or a verifying light client is trusted, a remote
--    RPC is not) and an operator may raise it deliberately, which is recorded in
--    trust_set_at/by because "why is this chain trusted" deserves an answer later.
--    Resolving a chain's name through ENS says nothing about this: a name registry
--    identifies a chain, it does not vouch for whoever is serving its logs.

ALTER TABLE chains ADD COLUMN IF NOT EXISTS enabled     BOOLEAN NOT NULL DEFAULT TRUE;
-- config: named in the YAML, and authoritative there.
-- api:    added over /v1/chains at runtime.
ALTER TABLE chains ADD COLUMN IF NOT EXISTS source      TEXT    NOT NULL DEFAULT 'config';
ALTER TABLE chains ADD COLUMN IF NOT EXISTS trust       TEXT    NOT NULL DEFAULT 'unverified';
ALTER TABLE chains ADD COLUMN IF NOT EXISTS node_url    TEXT;

-- The gas asset. Without these a portfolio reports a native balance with no symbol
-- and no decimals, which is unhelpful on Ethereum and wrong anywhere else.
ALTER TABLE chains ADD COLUMN IF NOT EXISTS native_symbol   TEXT;
ALTER TABLE chains ADD COLUMN IF NOT EXISTS native_decimals SMALLINT NOT NULL DEFAULT 18;
ALTER TABLE chains ADD COLUMN IF NOT EXISTS wrapped_native  BYTEA;

-- Confirmations, windows, intervals and discovery thresholds: the whole of what a
-- config.Chain tunes. One JSON blob rather than fifteen columns because it is
-- written and read whole and never queried into.
ALTER TABLE chains ADD COLUMN IF NOT EXISTS profile JSONB NOT NULL DEFAULT '{}'::jsonb;

-- How the chain was identified. Null means somebody typed the id by hand, which is
-- an ordinary way in: most chains are not in the registry.
ALTER TABLE chains ADD COLUMN IF NOT EXISTS ens_name        TEXT;
ALTER TABLE chains ADD COLUMN IF NOT EXISTS ens_resolved_at TIMESTAMPTZ;

ALTER TABLE chains ADD COLUMN IF NOT EXISTS trust_set_at TIMESTAMPTZ;
ALTER TABLE chains ADD COLUMN IF NOT EXISTS trust_set_by TEXT;

-- Why a chain is not running, when it is not.
ALTER TABLE chains ADD COLUMN IF NOT EXISTS last_error    TEXT;
ALTER TABLE chains ADD COLUMN IF NOT EXISTS last_error_at TIMESTAMPTZ;

ALTER TABLE chains ADD CONSTRAINT chains_trust_check
    CHECK (trust IN ('verified', 'unverified', 'quarantined'));
ALTER TABLE chains ADD CONSTRAINT chains_source_check
    CHECK (source IN ('config', 'api', 'demand'));
ALTER TABLE chains ADD CONSTRAINT chains_wrapped_native_len
    CHECK (wrapped_native IS NULL OR octet_length(wrapped_native) = 20);

INSERT INTO schema_migrations (version) VALUES (6) ON CONFLICT DO NOTHING;
