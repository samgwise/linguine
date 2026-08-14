package audit

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/store"
)

func newTestRepo(t *testing.T) (*Repo, *sql.DB, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "audit-test.db")
	s, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	db := s.DB()
	// Insert a node + api key + enrollment token so FKs are satisfiable.
	if _, err := db.Exec(
		`INSERT INTO node_enrollment_tokens (id, node_name, status) VALUES ('tok-n', 'node-1', 'active')`); err != nil {
		t.Fatalf("insert enrollment: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO nodes (id, token_id, status) VALUES ('node-1', 'tok-n', 'offline')`); err != nil {
		t.Fatalf("insert node: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO api_keys (id, name, token_hash, prefix, role, status) VALUES ('key-1', 'k', 'hash', 'sk-mesh-', 'client', 'active')`); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	return NewRepo(db, 256), db, dbPath
}

func TestRepoInsertAndRecentRoundTrip(t *testing.T) {
	repo, _, _ := newTestRepo(t)
	defer repo.Close()

	repo.Record(Entry{
		APIKeyID:        "key-1",
		NodeID:          "node-1",
		ModelRequested:  "llama-3.1-8b",
		ModelServed:     "llama-3.1-8b",
		PromptTokens:     120,
		CompletionTokens: 45,
		TotalDurationMs:  850,
		WasStreamed:      true,
		StatusCode:       200,
	})

	// Allow the background writer to flush.
	time.Sleep(700 * time.Millisecond)

	got, err := repo.Recent(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("records: got %d want 1", len(got))
	}
	r := got[0]
	if r.APIKeyID != "key-1" || r.NodeID != "node-1" {
		t.Errorf("ids: got key=%s node=%s want key-1 node-1", r.APIKeyID, r.NodeID)
	}
	if r.ModelRequested != "llama-3.1-8b" || r.ModelServed != "llama-3.1-8b" {
		t.Errorf("models: got %q/%q", r.ModelRequested, r.ModelServed)
	}
	if r.PromptTokens != 120 || r.CompletionTokens != 45 {
		t.Errorf("tokens: got %d/%d want 120/45", r.PromptTokens, r.CompletionTokens)
	}
	if !r.WasStreamed || r.StatusCode != 200 {
		t.Errorf("streamed/status: got %v/%d want true/200", r.WasStreamed, r.StatusCode)
	}
}

func TestRepoRecentFiltersByNode(t *testing.T) {
	repo, _, _ := newTestRepo(t)
	defer repo.Close()

	repo.Record(Entry{NodeID: "node-1", ModelRequested: "m-a", ModelServed: "m-a", StatusCode: 200})
	repo.Record(Entry{NodeID: "node-1", ModelRequested: "m-b", ModelServed: "m-b", StatusCode: 200})
	time.Sleep(700 * time.Millisecond)

	got, err := repo.Recent(context.Background(), 10, "node-1")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("node-1 records: got %d want 2", len(got))
	}
	// Newest first.
	if got[0].ModelRequested != "m-b" {
		t.Errorf("order: got[0]=%q want m-b (newest first)", got[0].ModelRequested)
	}
}

func TestRepoAsyncBatchesMany(t *testing.T) {
	repo, _, _ := newTestRepo(t)
	defer repo.Close()

	for i := 0; i < 150; i++ {
		repo.Record(Entry{NodeID: "node-1", ModelRequested: "m", ModelServed: "m", StatusCode: 200})
	}
	time.Sleep(700 * time.Millisecond)

	got, err := repo.Recent(context.Background(), 200, "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 150 {
		t.Errorf("records: got %d want 150 (async batching)", len(got))
	}
}

func TestRepoCloseFlushesFinalBatch(t *testing.T) {
	repo, _, dbPath := newTestRepo(t)
	repo.Record(Entry{NodeID: "node-1", ModelRequested: "m", ModelServed: "m", StatusCode: 200})
	// Close immediately — the entry may still be in the channel, not yet flushed
	// by the ticker. Close must flush it before returning.
	_ = repo.Close()

	// Re-open the same DB file and confirm the entry landed despite no ticker flush.
	s2, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer s2.Close()
	repo2 := NewRepo(s2.DB(), 4)
	defer repo2.Close()
	got, err := repo2.Recent(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("records after close flush: got %d want 1", len(got))
	}
}
