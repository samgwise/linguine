// Package store manages the embedded SQLite control-plane database
// (modernc.org/sqlite, CGO-free): schema migrations, WAL mode and access to
// the api_keys, node_enrollment_tokens and nodes tables.
package store
