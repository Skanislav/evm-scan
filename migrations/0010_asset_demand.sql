-- Demand: who wants a contract indexed.
--
-- A reader who looks a wallet up can vote for the contracts it holds that the index
-- does not keep. Votes are a priority signal for promotion and nothing else: they
-- decide what gets a backfill first, never what is true, and a spam verdict still
-- drops a contract out of the promotable set.
--
-- Two sources, one total:
--   asset_demand          one row per (asset, voter) through this deployment's API.
--                         The voter column is keccak256(salt || chainId || account),
--                         with the salt generated once below, so repeat votes from one
--                         account count once and yet the table is not a walkable list
--                         of who holds what — the same reasoning as a blinded filter.
--   asset_demand_onchain  the registry's own counter per asset, mirrored as an
--                         aggregate; the contract deduplicates voters itself.
-- asset_demand_totals is the sum, which is what promotion and the queue read.

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
INSERT INTO settings (key, value) VALUES ('demand_salt', gen_random_uuid()::text);

CREATE TABLE asset_demand (
    chain_id BIGINT      NOT NULL,
    address  BYTEA       NOT NULL,
    voter    BYTEA       NOT NULL,
    votes    BIGINT      NOT NULL DEFAULT 1,
    first_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, address, voter)
);
CREATE INDEX asset_demand_asset_idx ON asset_demand (chain_id, address);

CREATE TABLE asset_demand_onchain (
    chain_id  BIGINT      NOT NULL,
    address   BYTEA       NOT NULL,
    voters    BIGINT      NOT NULL,
    synced_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, address)
);

CREATE VIEW asset_demand_totals AS
SELECT chain_id, address, SUM(voters)::BIGINT AS voters
FROM (
    SELECT chain_id, address, COUNT(*)::BIGINT AS voters FROM asset_demand GROUP BY chain_id, address
    UNION ALL
    SELECT chain_id, address, voters FROM asset_demand_onchain
) t
GROUP BY chain_id, address;
