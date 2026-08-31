// Package fleet defines the read-only view types shared between the router
// and the admin dashboard, so neither package needs to import the other.
package fleet

import "time"

// NodeView is a public, read-only view of a connected worker for the admin
// dashboard. It carries the live telemetry the router's node registry holds
// in-memory.
type NodeView struct {
	ID string
	// Status is the router's rollup for display: online, stale, degraded, or
	// connecting (a rejected/unacknowledged connection attempt).
	Status string
	// ConnectionState is the worker's self-reported state (connecting/
	// registered/degraded); empty for claims and pre-ack workers.
	ConnectionState string
	// ClaimReason is set only for connection claims (rejected attempts):
	// auth_failed, inactive, or internal.
	ClaimReason    string
	ActiveModel    string
	Catalog        []string
	VRAMTotalMB    uint64
	VRAMFreeMB     uint64
	ActiveRequests int
	EstimatedTPS   float64
	LastHeartbeat  time.Time
}
