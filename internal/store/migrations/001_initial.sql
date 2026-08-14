-- linguine migration 001: initial Phase 0 schema.
-- Source of truth: design/schema.dbml. Statements are split on ';' by the
-- migration runner and applied in one transaction.

CREATE TABLE api_keys (
    id          TEXT PRIMARY KEY NOT NULL,
    name        TEXT NOT NULL,
    token_hash  TEXT NOT NULL UNIQUE,
    prefix      TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT 'client',
    status      TEXT NOT NULL DEFAULT 'active',
    expires_at  DATETIME,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE node_enrollment_tokens (
    id          TEXT PRIMARY KEY NOT NULL,
    node_name   TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'active',
    expires_at  DATETIME,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE nodes (
    id             TEXT PRIMARY KEY NOT NULL,
    token_id       TEXT NOT NULL REFERENCES node_enrollment_tokens(id),
    status         TEXT NOT NULL DEFAULT 'offline',
    remote_addr    TEXT,
    last_heartbeat DATETIME,
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_nodes_status ON nodes(status);
CREATE INDEX idx_api_keys_status ON api_keys(status);
