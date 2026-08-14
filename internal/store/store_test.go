package store

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), filepath.Join(dir, "linguine-test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStatementsHandlesCommentsAndStrings(t *testing.T) {
	raw := `-- a comment with a ; semicolon and a ';' quote
CREATE TABLE t (id INTEGER, note TEXT DEFAULT 'has ; semicolon');
-- trailing comment
CREATE INDEX i ON t(id);`
	got := statements(raw)
	want := []string{
		"CREATE TABLE t (id INTEGER, note TEXT DEFAULT 'has ; semicolon')",
		"CREATE INDEX i ON t(id)",
	}
	if len(got) != len(want) {
		t.Fatalf("statement count: got %d, want %d (%q)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statement %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

func TestMigrationsCreateExpectedTables(t *testing.T) {
	s := openTestStore(t)
	want := []string{"api_keys", "node_enrollment_tokens", "nodes", "schema_migrations"}

	rows, err := s.DB().QueryContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, name)
	}
	sort.Strings(got)
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing table %q; have %v", w, got)
		}
	}
}

func TestMigrationsSetWALMode(t *testing.T) {
	s := openTestStore(t)
	var mode string
	err := s.DB().QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&mode)
	if err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode: got %q, want wal", mode)
	}
}

func TestMigrationsIdempotent(t *testing.T) {
	// Re-opening an already-migrated database must not error and must not
	// re-apply or duplicate migrations.
	dir := t.TempDir()
	path := filepath.Join(dir, "linguine-idempotent.db")

	first, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	var count int
	if err := first.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count after first open: %v", err)
	}
	if count != 1 {
		t.Fatalf("migration count after first open: got %d, want 1", count)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()
	if err := second.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count after second open: %v", err)
	}
	if count != 1 {
		t.Errorf("migration count after second open: got %d, want 1 (re-applied)", count)
	}
}

func TestMigrationsExpectedColumns(t *testing.T) {
	s := openTestStore(t)
	cases := []struct {
		table string
		cols  []string
	}{
		{"api_keys", []string{"id", "name", "token_hash", "prefix", "role", "status", "expires_at", "created_at", "updated_at"}},
		{"node_enrollment_tokens", []string{"id", "node_name", "status", "expires_at", "created_at"}},
		{"nodes", []string{"id", "token_id", "status", "remote_addr", "last_heartbeat", "created_at", "updated_at"}},
	}
	for _, tc := range cases {
		t.Run(tc.table, func(t *testing.T) {
			rows, err := s.DB().QueryContext(context.Background(), `PRAGMA table_info(`+tc.table+`)`)
			if err != nil {
				t.Fatalf("table_info %s: %v", tc.table, err)
			}
			defer rows.Close()
			got := map[string]bool{}
			for rows.Next() {
				var cid int
				var name, ctype string
				var notnull, pk int
				var dflt interface{}
				if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got[name] = true
			}
			for _, c := range tc.cols {
				if !got[c] {
					t.Errorf("missing column %q on %s", c, tc.table)
				}
			}
		})
	}
}
