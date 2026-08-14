-- linguine migration 002: Phase 1a fleet — node model catalog, request audit
-- log, and active_model on nodes. Source of truth: design/schema.dbml.
-- Statements are split on ';' by the migration runner (comment/string aware).

ALTER TABLE nodes ADD COLUMN active_model TEXT;

CREATE TABLE node_model_catalogs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id          TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    model_name       TEXT NOT NULL,
    file_size_bytes  INTEGER,
    last_scanned_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX idx_node_model_catalogs_unique ON node_model_catalogs(node_id, model_name);

CREATE TABLE request_audit_logs (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    api_key_id         TEXT REFERENCES api_keys(id),
    node_id            TEXT REFERENCES nodes(id),
    model_requested    TEXT NOT NULL,
    model_served       TEXT NOT NULL,
    prompt_tokens      INTEGER DEFAULT 0,
    completion_tokens  INTEGER DEFAULT 0,
    ttft_ms            INTEGER,
    total_duration_ms  INTEGER,
    was_streamed        BOOLEAN NOT NULL DEFAULT FALSE,
    was_model_swapped  BOOLEAN NOT NULL DEFAULT FALSE,
    status_code        INTEGER NOT NULL DEFAULT 200,
    created_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_audit_logs_created ON request_audit_logs(created_at);
CREATE INDEX idx_audit_logs_node ON request_audit_logs(node_id);
