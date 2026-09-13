CREATE TABLE user_state_nodes (
 hash BYTEA PRIMARY KEY CHECK (octet_length(hash)=32), data BYTEA NOT NULL
);
CREATE TABLE user_state_revisions (
 id BYTEA PRIMARY KEY CHECK (octet_length(id)=32),
 account BYTEA NOT NULL CHECK (octet_length(account)=20),
 snapshot JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX user_state_revision_account ON user_state_revisions(account);
CREATE TABLE user_state_heads (
 account BYTEA PRIMARY KEY CHECK (octet_length(account)=20),
 id BYTEA NOT NULL REFERENCES user_state_revisions(id),
 generation BIGINT NOT NULL DEFAULT 0,
 legacy_changed BOOLEAN NOT NULL DEFAULT false
);
CREATE TABLE user_state_checkpoints (
 root BYTEA PRIMARY KEY CHECK (octet_length(root)=32),
 manifest JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE user_state_publications (
 id BIGSERIAL PRIMARY KEY,
 root BYTEA NOT NULL REFERENCES user_state_checkpoints(root),
 resolver BYTEA NOT NULL, node BYTEA NOT NULL,
 raw_tx BYTEA NOT NULL, tx_hash BYTEA NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','confirmed','failed')),
 error TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX user_state_one_pending ON user_state_publications ((true)) WHERE status='pending';
