-- Record the digest of the membership filter an epoch commits to.
--
-- A .xorf filter says which (account, contract) pairs the index holds, and it is
-- the artifact a reader downloads to narrow a portfolio read without naming their
-- address. Served over HTTP it is only as honest as the host: the same daemon
-- serves the file and the digest, so a lying one lies about both.
--
-- Binding it to an epoch breaks that circle. The filter is built from the same
-- SnapshotIndex call that produced the merkle root, at the same toBlock, so the
-- publisher's bonded commitment fixes the block it is true as of; the digest goes
-- into the manifest the epoch's URI points at, and the URI is set in the same
-- transaction as the root.
--
-- Only the digest is stored. The filter itself rebuilds deterministically from
-- interactions as of to_block, so keeping the bytes here would be a second copy of
-- something already durable — and one that could silently disagree with it.
ALTER TABLE epochs ADD COLUMN IF NOT EXISTS filter_keccak BYTEA;

ALTER TABLE epochs DROP CONSTRAINT IF EXISTS epochs_filter_keccak_len;
ALTER TABLE epochs ADD CONSTRAINT epochs_filter_keccak_len
    CHECK (filter_keccak IS NULL OR octet_length(filter_keccak) = 32);
