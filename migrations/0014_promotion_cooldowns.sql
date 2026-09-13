-- An address without code may acquire a contract later. Defer automatic retries
-- without inventing a candidate, deleting demand, or marking it as spam.
CREATE TABLE promotion_cooldowns (
    chain_id BIGINT NOT NULL,
    address BYTEA NOT NULL CHECK (octet_length(address) = 20),
    retry_after TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (chain_id, address)
);
