-- Verdicts: a reader's signed split of their own holdings.
--
-- A vote through /v1/demand only ever counted for a contract. A verdict counts
-- either way: the reader looks their wallet over, sorts it into recognized and not,
-- and signs one EIP-712 message over the whole split (docs/SHIP.md §4). Each row
-- in asset_demand now carries the direction as `weight`, +1 or -1; a contract the
-- reader left out of the verdict has no row, which is what 0 looks like.
--
-- account_verdicts is the replay guard. The signature carries a deadline, and the
-- deadline stored here only ever rises for a (chain, voter), so an old signature
-- cannot put an old split back. The voter is the same blinded key as in
-- asset_demand, so this table is no more walkable than that one.
--
-- asset_demand_totals is rebuilt to expose both directions. The on-chain counter
-- only counts for, so it lands in `voters` with nothing against. Postgres cannot
-- add a column to a view in place through CREATE OR REPLACE when the column list
-- changes shape, so the view is dropped and made again; nothing in the schema
-- depends on it, only queries do.

ALTER TABLE asset_demand
    ADD COLUMN weight SMALLINT NOT NULL DEFAULT 1 CHECK (weight IN (-1, 1));

CREATE TABLE account_verdicts (
    chain_id  BIGINT      NOT NULL,
    voter     BYTEA       NOT NULL,   -- keccak256(salt || chainId || account), same as asset_demand
    deadline  NUMERIC(78) NOT NULL,   -- monotonic per (chain, voter): replay guard
    signed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, voter)
);

DROP VIEW asset_demand_totals;
CREATE VIEW asset_demand_totals AS
SELECT chain_id, address, SUM(voters)::BIGINT AS voters, SUM(against)::BIGINT AS against
FROM (
    SELECT chain_id, address,
           COUNT(*) FILTER (WHERE weight > 0)::BIGINT AS voters,
           COUNT(*) FILTER (WHERE weight < 0)::BIGINT AS against
    FROM asset_demand GROUP BY chain_id, address
    UNION ALL
    SELECT chain_id, address, voters, 0::BIGINT FROM asset_demand_onchain
) t
GROUP BY chain_id, address;
