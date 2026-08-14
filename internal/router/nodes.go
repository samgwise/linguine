package router

import (
	"sync"
	"time"

	"github.com/samgw/linguine/internal/mesh"
)

// nodeEntry is a connected worker's last-known state.
type nodeEntry struct {
	id          string
	pipe        mesh.PipeID
	activeModel string
	lastSeen    time.Time
}

// nodeRegistry tracks online workers by node id and NNG pipe, preserving
// insertion order for deterministic Phase 0 selection (the first non-stale
// node). Real cost-aware selection arrives in a later phase.
type nodeRegistry struct {
	mu          sync.Mutex
	byID        map[string]*nodeEntry
	byPipe      map[mesh.PipeID]*nodeEntry
	order        []string
	staleAfter   time.Duration
}

func newNodeRegistry(staleAfter time.Duration) *nodeRegistry {
	return &nodeRegistry{
		byID:        make(map[string]*nodeEntry),
		byPipe:      make(map[mesh.PipeID]*nodeEntry),
		staleAfter:   staleAfter,
	}
}

// upsert inserts or refreshes a node, keeping the id<->pipe mappings in sync.
func (r *nodeRegistry) upsert(e *nodeEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// If a pipe is now used by a different node id, retire the old id.
	if old, ok := r.byPipe[e.pipe]; ok && old.id != e.id {
		delete(r.byID, old.id)
		r.order = removeString(r.order, old.id)
	}
	if _, exists := r.byID[e.id]; !exists {
		r.order = append(r.order, e.id)
	}
	r.byID[e.id] = e
	r.byPipe[e.pipe] = e
}

// selectAny returns the first non-stale node in insertion order, or false if
// none is online.
func (r *nodeRegistry) selectAny() (*nodeEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for _, id := range r.order {
		e := r.byID[id]
		if now.Sub(e.lastSeen) <= r.staleAfter {
			return e, true
		}
	}
	return nil, false
}

func removeString(xs []string, x string) []string {
	for i, v := range xs {
		if v == x {
			return append(xs[:i], xs[i+1:]...)
		}
	}
	return xs
}
