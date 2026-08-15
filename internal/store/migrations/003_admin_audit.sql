-- linguine migration 003: admin audit log — records admin dashboard auth
-- events (login attempts, throttling) for intrusion detection. Source of
-- truth: design/schema.dbml. Statements are split on ';' by the migration
-- runner (comment/string aware). Written synchronously: admin login is low
-- frequency, unlike the /v1 hot path which uses the async audit writer.

CREATE TABLE admin_audit_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    event       TEXT NOT NULL,
    api_key_id  TEXT REFERENCES api_keys(id),
    remote_ip   TEXT,
    status_code INTEGER NOT NULL,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_admin_audit_logs_created ON admin_audit_logs(created_at);
