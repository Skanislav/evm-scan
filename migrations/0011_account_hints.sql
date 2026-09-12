-- A reader's own cross-chain hint.
--
-- The page builds a bloom filter over every (chain, contract) pair a cross-chain
-- sweep confirmed the account holds, keyed by the ERC-7930 pair so one filter spans
-- every chain, and the account's wallet signs its digest. The daemon keeps the
-- signed bytes here and serves them as the evmscan.hint record under the account's
-- ENS name, so any client can order a sweep by it; when no reader has written one,
-- the daemon builds a bloom from the index instead and stores nothing.
--
-- Be clear about what this row is. It is the first per-account row the daemon keeps
-- outside the index: enumerable by address, unblinded, and testable against any
-- token dictionary by anyone who fetches it. That is the opposite of the salted
-- voter in asset_demand, and it is stored only because the account signed for it.
-- `deadline` is the signature's own expiry and only ever rises, so an old signature
-- cannot put an old hint back.

CREATE TABLE account_hints (
    account   BYTEA       PRIMARY KEY,
    bytes     BYTEA       NOT NULL,   -- the whole .xorf, a KindInterop bloom of at most 4096 bits
    digest    BYTEA       NOT NULL,   -- keccak256(bytes), what the wallet signed
    deadline  BIGINT      NOT NULL,   -- unix seconds; monotonic per account
    signed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
