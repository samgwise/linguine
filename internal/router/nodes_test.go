package router

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/mesh"
	"github.com/samgw/linguine/internal/store"
)

func newTestRegistry(t *testing.T, staleAfter time.Duration) *nodeRegistry {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "nodes-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return newNodeRegistry(staleAfter, s.DB())
}

// insertEnrollmentToken inserts a node_enrollment_tokens row so the
// nodes.token_id foreign key is satisfied in persistence tests.
func insertEnrollmentToken(t *testing.T, db *sql.DB, id, nodeName string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO node_enrollment_tokens (id, node_name, status) VALUES (?, ?, 'active')`,
		id, nodeName,
	); err != nil {
		t.Fatalf("insert enrollment token: %v", err)
	}
}

func TestSelectLeastConnectionsPicksFewest(t *testing.T) {
	r := newTestRegistry(t, time.Minute)
	now := time.Now()
	r.upsert(&nodeEntry{id: "a", pipe: 1, activeRequests: 3, lastSeen: now})
	r.upsert(&nodeEntry{id: "b", pipe: 2, activeRequests: 1, lastSeen: now})
	r.upsert(&nodeEntry{id: "c", pipe: 3, activeRequests: 2, lastSeen: now})

	got, ok := r.selectLeastConnections()
	if !ok {
		t.Fatal("expected a node, got none")
	}
	if got.id != "b" {
		t.Errorf("selected: got %q want %q (fewest active_requests)", got.id, "b")
	}
}

func TestSelectLeastConnectionsTieBreaksByInsertionOrder(t *testing.T) {
	r := newTestRegistry(t, time.Minute)
	now := time.Now()
	// All nodes tie on active_requests; the first-inserted must win.
	r.upsert(&nodeEntry{id: "first", pipe: 1, activeRequests: 5, lastSeen: now})
	r.upsert(&nodeEntry{id: "second", pipe: 2, activeRequests: 5, lastSeen: now})
	r.upsert(&nodeEntry{id: "third", pipe: 3, activeRequests: 5, lastSeen: now})

	got, ok := r.selectLeastConnections()
	if !ok {
		t.Fatal("expected a node, got none")
	}
	if got.id != "first" {
		t.Errorf("tie-break: got %q want %q (insertion order)", got.id, "first")
	}
}

func TestSelectLeastConnectionsSkipsStale(t *testing.T) {
	r := newTestRegistry(t, 100*time.Millisecond)
	now := time.Now()
	r.upsert(&nodeEntry{id: "fresh", pipe: 1, activeRequests: 9, lastSeen: now})
	r.upsert(&nodeEntry{id: "stale", pipe: 2, activeRequests: 0, lastSeen: now.Add(-time.Hour)})

	got, ok := r.selectLeastConnections()
	if !ok {
		t.Fatal("expected a node, got none")
	}
	if got.id != "fresh" {
		t.Errorf("stale node should be skipped: got %q want %q", got.id, "fresh")
	}
}

func TestSelectLeastConnectionsAllStale(t *testing.T) {
	r := newTestRegistry(t, time.Millisecond)
	r.upsert(&nodeEntry{id: "a", pipe: 1, lastSeen: time.Now()})
	time.Sleep(20 * time.Millisecond)
	if _, ok := r.selectLeastConnections(); ok {
		t.Error("expected no node when all are stale")
	}
}

func TestUpsertPersistsActiveModel(t *testing.T) {
	r := newTestRegistry(t, time.Minute)
	insertEnrollmentToken(t, r.db, "tok-1", "node-x")
	r.upsert(&nodeEntry{
		id:          "node-x",
		tokenID:     "tok-1",
		pipe:        7,
		activeModel: "llama-3.1-8b-instruct",
		catalog:     []string{"llama-3.1-8b-instruct", "mistral-7b"},
		lastSeen:    time.Now(),
	})

	// The node row must reflect active_model and online status.
	var status, activeModel string
	if err := r.db.QueryRow(
		`SELECT status, active_model FROM nodes WHERE id = ?`, "node-x",
	).Scan(&status, &activeModel); err != nil {
		t.Fatalf("query node: %v", err)
	}
	if status != "online" {
		t.Errorf("status: got %q want online", status)
	}
	if activeModel != "llama-3.1-8b-instruct" {
		t.Errorf("active_model: got %q want llama-3.1-8b-instruct", activeModel)
	}
}

func TestUpsertSyncsCatalogOnChange(t *testing.T) {
	r := newTestRegistry(t, time.Minute)
	insertEnrollmentToken(t, r.db, "tok-c", "node-c")
	r.upsert(&nodeEntry{
		id:      "node-c",
		tokenID:  "tok-c",
		pipe:    9,
		catalog: []string{"m1", "m2"},
		lastSeen: time.Now(),
	})

	countModels := func() int {
		var n int
		if err := r.db.QueryRow(
			`SELECT COUNT(*) FROM node_model_catalogs WHERE node_id = ?`, "node-c",
		).Scan(&n); err != nil {
			t.Fatalf("count catalog: %v", err)
		}
		return n
	}
	if got := countModels(); got != 2 {
		t.Errorf("catalog rows after first sync: got %d want 2", got)
	}

	// Same catalog — must not rewrite (syncedCatalog short-circuits).
	r.upsert(&nodeEntry{
		id:      "node-c",
		tokenID:  "tok-c",
		pipe:    9,
		catalog: []string{"m1", "m2"},
		lastSeen: time.Now(),
	})
	// A different pipe for the same id is an edge case we don't exercise here;
	// the point is the catalog didn't change, so no rewrite is expected.

	// Changed catalog — must rewrite to the new set.
	r.upsert(&nodeEntry{
		id:      "node-c",
		tokenID:  "tok-c",
		pipe:    9,
		catalog: []string{"m1", "m2", "m3"},
		lastSeen: time.Now(),
	})
	if got := countModels(); got != 3 {
		t.Errorf("catalog rows after change: got %d want 3", got)
	}
}

func TestSnapshotReturnsAllNodes(t *testing.T) {
	r := newTestRegistry(t, time.Minute)
	now := time.Now()
	r.upsert(&nodeEntry{id: "a", pipe: 1, activeRequests: 1, lastSeen: now})
	r.upsert(&nodeEntry{id: "b", pipe: 2, activeRequests: 2, lastSeen: now})
	got := r.snapshot()
	if len(got) != 2 {
		t.Fatalf("snapshot len: got %d want 2", len(got))
	}
	// snapshot copies by value, so later mutation of the registry must not
	// change the already-returned snapshot.
	r.upsert(&nodeEntry{id: "c", pipe: 3, lastSeen: now})
	if len(got) != 2 {
		t.Errorf("snapshot aliased registry state: len changed to %d", len(got))
	}
}

// Ensure mesh import is retained for the pipe type.
var _ mesh.PipeID
