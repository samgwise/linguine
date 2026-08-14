// Package fleet defines the read-only view types shared between the router
// and the admin dashboard, so neither package needs to import the other.
package fleet

import "time"

// NodeView is a public, read-only view of a connected worker for the admin
// dashboard. It carries the live telemetry the router's node registry holds
// in-memory.
type NodeView struct {
	ID            string
	Status        string
	ActiveModel   string
	Catalog       []string
	VRAMTotalMB   uint64
	VRAMFreeMB    uint64
	ActiveRequests int
	EstimatedTPS  float64
	LastHeartbeat time.Time
}
