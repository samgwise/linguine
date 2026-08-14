package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	// CGO-free SQLite driver registered as "sqlite".
	_ "modernc.org/sqlite"
)

//go:embed migrations
var migrationFS embed.FS

// Store wraps the embedded SQLite control-plane database.
type Store struct {
	db *sql.DB
}

// Open connects to the SQLite database at path, enables WAL mode and foreign
// keys, and applies any pending migrations. The path may be ":memory:" for an
// in-process database (used by tests) or a filesystem path for production.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// A single writer is the safest concurrency model for SQLite; the router
	// is the only writer and reads are served from WAL snapshots.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.pragmas(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// DB exposes the underlying connection for packages that query the schema
// directly (auth, router). The schema (design/schema.dbml) is the shared
// contract.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database connection.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

// pragmas sets durable, concurrent-friendly SQLite modes.
func (s *Store) pragmas(ctx context.Context) error {
	for _, p := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := s.db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("store: pragma %q: %w", p, err)
		}
	}
	return nil
}

// migrate applies every embedded migration not yet recorded in
// schema_migrations, each in its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    name        TEXT PRIMARY KEY NOT NULL,
    applied_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied, err := s.appliedMigrations(ctx)
	if err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("store: read migrations dir: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if applied[name] {
			continue
		}
		raw, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", name, err)
		}
		if err := s.applyMigration(ctx, name, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs each ;-separated statement in its own transactional step
// and records the migration as applied on success.
func (s *Store) applyMigration(ctx context.Context, name, raw string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin tx for %s: %w", name, err)
	}
	for _, stmt := range statements(raw) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: apply %s: %w", name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: record %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit %s: %w", name, err)
	}
	return nil
}

func (s *Store) appliedMigrations(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: query schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: scan migration name: %w", err)
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate schema_migrations: %w", err)
	}
	return applied, nil
}

// statements splits a SQL script into individual statements, ignoring
// semicolons inside single-quoted string literals (with '' escapes) and
// inside -- line comments. This lets migrations carry comments and string
// literals that contain semicolons without confusing the runner.
func statements(raw string) []string {
	var (
		out      []string
		buf      strings.Builder
		inString bool
	)
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			buf.WriteByte(c)
			if c == '\'' {
				// A doubled single quote is an escaped quote inside a string.
				if i+1 < len(raw) && raw[i+1] == '\'' {
					buf.WriteByte(raw[i+1])
					i++
					continue
				}
				inString = false
			}
			continue
		}
		if c == '\'' {
			buf.WriteByte(c)
			inString = true
			continue
		}
		// -- line comment: drop everything to end of line.
		if c == '-' && i+1 < len(raw) && raw[i+1] == '-' {
			for i < len(raw) && raw[i] != '\n' {
				i++
			}
			continue
		}
		if c == ';' {
			if stmt := strings.TrimSpace(buf.String()); stmt != "" {
				out = append(out, stmt)
			}
			buf.Reset()
			continue
		}
		buf.WriteByte(c)
	}
	if stmt := strings.TrimSpace(buf.String()); stmt != "" {
		out = append(out, stmt)
	}
	return out
}
