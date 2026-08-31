package router

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/samgw/linguine/internal/fleet"
	"github.com/samgw/linguine/internal/mesh"
	"github.com/samgw/linguine/internal/protocol"
)

// nodeEntry is a connected worker's last-known state. Volatile telemetry
// fields (VRAM, TPS, active_requests) live in-memory only; active_model and
// last_heartbeat persist to the nodes table so the dashboard has last-known
// state after a router restart.
type nodeEntry struct {
	id      string
	tokenID string
	pipe    mesh.PipeID
	// connectionState is the worker's self-reported state from its latest
	// heartbeat (connecting/registered/degraded; empty from older workers).
	connectionState string
	activeModel     string
	catalog         []string

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

// claimEntry is a rejected-but-persisting connection attempt: a worker whose
// heartbeats are being refused (bad token, revoked enrollment, router-side
// lookup failure). Claims are in-memory only — this is unauthenticated,
// worker-supplied data and must never reach SQLite — and expire shortly
// after the worker stops knocking so the dashboard doesn't fill with ghosts.
type claimEntry struct {
	nodeID      string
	pipe        mesh.PipeID
	reason      string
	detail      string
	lastAttempt time.Time
}

const (
	claimMaxEntries = 64 // cap against a flood of bogus claims
	claimExpiry     = 60 * time.Second
)

// nodeRegistry tracks online workers by node id and NNG pipe, plus a
// side-table of unauthenticated connection attempts (claims). Selection is
// least-connections (smallest active_requests, tie-break by insertion order)
// — the direct stepping stone to the cost-aware scorer in Phase 1b.
type nodeRegistry struct {
	mu         sync.Mutex
	byID       map[string]*nodeEntry
	byPipe     map[mesh.PipeID]*nodeEntry
	order      []string
	claims     map[string]*claimEntry
	staleAfter time.Duration
	db         *sql.DB
}

func newNodeRegistry(staleAfter time.Duration, db *sql.DB) *nodeRegistry {
	return &nodeRegistry{
		byID:       make(map[string]*nodeEntry),
		byPipe:     make(map[mesh.PipeID]*nodeEntry),
		claims:     make(map[string]*claimEntry),
		staleAfter: staleAfter,
		db:         db,
	}
}

// recordClaim notes a rejected heartbeat so operators can see a worker that
// is trying (and failing) to connect. Claims expire silently; a worker whose
// enrollment succeeds later simply disappears from the claims view when it
// registers as a node.
func (r *nodeRegistry) recordClaim(nodeID, reason, detail string, pipe mesh.PipeID) {
	if nodeID == "" {
		return // nothing identifiable to claim under
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Expire stale claims opportunistically, then evict the oldest if the
	// table is full (a flood of bogus node names must not grow it).
	now := time.Now()
	for id, c := range r.claims {
		if now.Sub(c.lastAttempt) > claimExpiry {
			delete(r.claims, id)
		}
	}
	if _, exists := r.claims[nodeID]; !exists && len(r.claims) >= claimMaxEntries {
		oldestID := ""
		var oldest time.Time
		for id, c := range r.claims {
			if oldestID == "" || c.lastAttempt.Before(oldest) {
				oldestID, oldest = id, c.lastAttempt
			}
		}
		delete(r.claims, oldestID)
	}
	r.claims[nodeID] = &claimEntry{
		nodeID:      nodeID,
		pipe:        pipe,
		reason:      reason,
		detail:      detail,
		lastAttempt: now,
	}
}

// ClaimsSnapshot returns the current connection claims for the dashboard.
func (r *nodeRegistry) ClaimsSnapshot() []claimEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	out := make([]claimEntry, 0, len(r.claims))
	for _, c := range r.claims {
		if now.Sub(c.lastAttempt) <= claimExpiry {
			out = append(out, *c)
		}
	}
	return out
}

// upsert inserts or refreshes a node, keeping the id<->pipe mappings in sync,
// then persists active_model + last_heartbeat to the nodes table and syncs the
// model catalog. Heartbeat persist is synchronous: heartbeats are infrequent
// (every few seconds) and SQLite WAL handles the write rate easily for a
// small fleet; the /v1 hot path (audit logging) is what must stay async.
func (r *nodeRegistry) upsert(e *nodeEntry) {
	r.mu.Lock()
	// A successful registration retires any outstanding claim for this node.
	delete(r.claims, e.id)
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
		switch {
		case !stale:
			status = "stale"
		case e.connectionState == protocol.ConnStateDegraded:
			// The worker itself reports ack silence; surface it above a
			// plain online so operators see the degradation.
			status = "degraded"
		}
		out = append(out, fleet.NodeView{
			ID:              e.id,
			Status:          status,
			ConnectionState: e.connectionState,
			ActiveModel:     e.activeModel,
			Catalog:         e.catalog,
			VRAMTotalMB:     e.vramTotalMB,
			VRAMFreeMB:      e.vramFreeMB,
			ActiveRequests:  e.activeRequests,
			EstimatedTPS:    e.estimatedTPS,
			LastHeartbeat:   e.lastSeen,
		})
	}
	return out
}

// ClaimsSnapshot returns a public view of rejected connection attempts
// (workers knocking with invalid, revoked, or unverifiable enrollment). The
// router tracks these in memory only; they expire shortly after the worker
// stops trying.
func (s *Server) ClaimsSnapshot() []fleet.NodeView {
	claims := s.nodes.ClaimsSnapshot()
	out := make([]fleet.NodeView, 0, len(claims))
	for _, c := range claims {
		out = append(out, fleet.NodeView{
			ID:              c.nodeID,
			Status:          "connecting",
			ConnectionState: protocol.ConnStateConnecting,
			ClaimReason:     c.reason,
			LastHeartbeat:   c.lastAttempt,
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
