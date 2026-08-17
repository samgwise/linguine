package router

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/samgw/linguine/internal/fleet"
	"github.com/samgw/linguine/internal/mesh"
)

// nodeEntry is a connected worker's last-known state. Volatile telemetry
// fields (VRAM, TPS, active_requests) live in-memory only; active_model and
// last_heartbeat persist to the nodes table so the dashboard has last-known
// state after a router restart.
type nodeEntry struct {
	id          string
	tokenID     string
	pipe        mesh.PipeID
	activeModel string
	catalog     []string

	// Volatile telemetry — in-memory only.
	vramTotalMB         uint64
	vramFreeMB          uint64
	activeRequests      int
	estimatedTPS        float64
	activeConversations int      // Phase 2: zero in 1a
	cachedTokens        int      // Phase 2: zero in 1a
	pinnedSessions      []string // Phase 2: nil in 1a

	lastSeen      time.Time
	syncedCatalog []string // last catalog written to node_model_catalogs
}

// nodeRegistry tracks online workers by node id and NNG pipe. Selection is
// least-connections (smallest active_requests, tie-break by insertion order)
// — the direct stepping stone to the cost-aware scorer in Phase 1b.
type nodeRegistry struct {
	mu         sync.Mutex
	byID       map[string]*nodeEntry
	byPipe     map[mesh.PipeID]*nodeEntry
	order      []string
	staleAfter time.Duration
	db         *sql.DB
}

func newNodeRegistry(staleAfter time.Duration, db *sql.DB) *nodeRegistry {
	return &nodeRegistry{
		byID:       make(map[string]*nodeEntry),
		byPipe:     make(map[mesh.PipeID]*nodeEntry),
		staleAfter: staleAfter,
		db:         db,
	}
}

// upsert inserts or refreshes a node, keeping the id<->pipe mappings in sync,
// then persists active_model + last_heartbeat to the nodes table and syncs the
// model catalog. Heartbeat persist is synchronous: heartbeats are infrequent
// (every few seconds) and SQLite WAL handles the write rate easily for a
// small fleet; the /v1 hot path (audit logging) is what must stay async.
func (r *nodeRegistry) upsert(e *nodeEntry) {
	r.mu.Lock()
	// If a pipe is now used by a different node id, retire the old id.
	if old, ok := r.byPipe[e.pipe]; ok && old.id != e.id {
		delete(r.byID, old.id)
		r.order = removeString(r.order, old.id)
	}
	if existing, exists := r.byID[e.id]; exists {
		// Preserve the last-synced catalog so we only rewrite the catalog
		// table when it actually changes, not on every heartbeat.
		e.syncedCatalog = existing.syncedCatalog
	} else {
		r.order = append(r.order, e.id)
	}
	r.byID[e.id] = e
	r.byPipe[e.pipe] = e
	r.mu.Unlock()

	if r.db != nil {
		r.persist(e)
	}
}

// persist writes the node row and syncs the catalog table. It runs outside the
// registry mutex so a slow write doesn't block selection.
func (r *nodeRegistry) persist(e *nodeEntry) {
	now := time.Now().UTC()
	if _, err := r.db.Exec(
		`INSERT INTO nodes (id, token_id, status, active_model, last_heartbeat, updated_at)
		 VALUES (?, ?, 'online', ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET
		     status = 'online',
		     active_model = excluded.active_model,
		     last_heartbeat = excluded.last_heartbeat,
		     updated_at = CURRENT_TIMESTAMP`,
		e.id, e.tokenID, e.activeModel, now,
	); err != nil {
		// Persistence is best-effort for dashboard state; a failure logs but
		// doesn't break request routing, which relies on in-memory state.
		fmt.Printf("[router] persist node %s: %v\n", e.id, err)
		return
	}
	if catalogChanged(e.catalog, e.syncedCatalog) {
		r.syncCatalog(e)
	}
}

func catalogChanged(a, b []string) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i] != b[i] {
			return true
		}
	}
	return false
}

// syncCatalog replaces the node's rows in node_model_catalogs and records the
// synced set so we only rewrite when the catalog actually changes.
func (r *nodeRegistry) syncCatalog(e *nodeEntry) {
	tx, err := r.db.Begin()
	if err != nil {
		fmt.Printf("[router] sync catalog tx: %v\n", err)
		return
	}
	if _, err := tx.Exec(`DELETE FROM node_model_catalogs WHERE node_id = ?`, e.id); err != nil {
		_ = tx.Rollback()
		fmt.Printf("[router] sync catalog delete: %v\n", err)
		return
	}
	for _, m := range e.catalog {
		if _, err := tx.Exec(
			`INSERT INTO node_model_catalogs (node_id, model_name) VALUES (?, ?)`,
			e.id, m,
		); err != nil {
			_ = tx.Rollback()
			fmt.Printf("[router] sync catalog insert: %v\n", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fmt.Printf("[router] sync catalog commit: %v\n", err)
		return
	}
	e.syncedCatalog = append([]string(nil), e.catalog...)
}

// selectLeastConnections returns the non-stale node with the smallest
// active_requests, tie-broken by insertion order. Returns false if no node is
// online. This is the direct stepping stone to the §3 cost-aware scorer
// (which generalises "fewest active" to "shortest wait").
func (r *nodeRegistry) selectLeastConnections() (*nodeEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	var best *nodeEntry
	for _, id := range r.order {
		e := r.byID[id]
		if now.Sub(e.lastSeen) > r.staleAfter {
			continue
		}
		if best == nil || e.activeRequests < best.activeRequests {
			best = e
		}
	}
	return best, best != nil
}

// snapshot returns a slice copy of all current node entries for the dashboard.
func (r *nodeRegistry) snapshot() []nodeEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]nodeEntry, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, *r.byID[id])
	}
	return out
}

// NodesSnapshot returns a public view of all current nodes for the admin
// dashboard. The slice is ordered by insertion.
func (s *Server) NodesSnapshot() []fleet.NodeView {
	entries := s.nodes.snapshot()
	out := make([]fleet.NodeView, 0, len(entries))
	for _, e := range entries {
		stale := time.Since(e.lastSeen) <= s.nodes.staleAfter
		status := "online"
		if !stale {
			status = "stale"
		}
		out = append(out, fleet.NodeView{
			ID:             e.id,
			Status:         status,
			ActiveModel:    e.activeModel,
			Catalog:        e.catalog,
			VRAMTotalMB:    e.vramTotalMB,
			VRAMFreeMB:     e.vramFreeMB,
			ActiveRequests: e.activeRequests,
			EstimatedTPS:   e.estimatedTPS,
			LastHeartbeat:  e.lastSeen,
		})
	}
	return out
}

func removeString(xs []string, x string) []string {
	for i, v := range xs {
		if v == x {
			return append(xs[:i], xs[i+1:]...)
		}
	}
	return xs
}
