-- linguine migration 004: add a free-form detail column to admin_audit_logs.
-- The api_key_id column is a foreign key to api_keys, so events that reference
-- non-API-key objects (worker enrolment token ids, node names) need their own
-- slot. Source of truth: design/schema.dbml.

ALTER TABLE admin_audit_logs ADD COLUMN detail TEXT;
