package protocol

// HeartbeatReqID is the envelope reqID used for worker→router heartbeat
// control messages. It never collides with a dispatch reqID because the
// router generates dispatch reqIDs as UUIDs.
const HeartbeatReqID = "hb"

// Heartbeat is the control payload a worker sends to the router to announce
// itself, authenticate via its enrollment token, and refresh its liveness and
// load. The router verifies the EnrollmentToken (PASETO v4) and records the
// node along with the NNG pipe id it arrived on.
//
// Telemetry fields (Catalog, VRAM, ActiveRequests, EstimatedTPS) are populated
// from Phase 1a. The KV-cache fields (ActiveConversations, CachedTokens,
// PinnedSessions) are carried from Phase 1a but stay zero/nil until Phase 2
// session affinity activates them.
type Heartbeat struct {
	NodeID              string   `json:"node_id"`
	EnrollmentToken     string   `json:"enrollment_token"`
	ActiveModel         string   `json:"active_model,omitempty"`
	Catalog             []string `json:"catalog,omitempty"`
	VRAMTotalMB         uint64   `json:"vram_total_mb,omitempty"`
	VRAMFreeMB          uint64   `json:"vram_free_mb,omitempty"`
	ActiveRequests      int      `json:"active_requests,omitempty"`
	EstimatedTPS        float64  `json:"estimated_tps,omitempty"`
	ActiveConversations int      `json:"active_conversations,omitempty"` // Phase 2: zero in 1a
	CachedTokens        int      `json:"cached_tokens,omitempty"`        // Phase 2: zero in 1a
	PinnedSessions      []string `json:"pinned_sessions,omitempty"`      // Phase 2: nil in 1a
}
