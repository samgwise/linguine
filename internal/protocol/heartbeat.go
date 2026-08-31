package protocol

// HeartbeatReqID is the envelope reqID used for worker→router heartbeat
// control messages. HeartbeatAckReqID is the router→worker reply. Neither
// collides with a dispatch reqID because the router generates dispatch
// reqIDs as UUIDs.
const (
	HeartbeatReqID    = "hb"
	HeartbeatAckReqID = "hba"
)

// Connection states a worker self-reports in its heartbeat. Connecting means
// the worker is dialling/heartbeating but has not yet received a router ack;
// registered means an authenticated heartbeat has been acknowledged;
// degraded means acks used to arrive and have stopped (socket loss, router
// restart, or auth reversal) within the ack-timeout window.
const (
	ConnStateConnecting = "connecting"
	ConnStateRegistered = "registered"
	ConnStateDegraded   = "degraded"
)

// Ack rejection reasons carried in a HeartbeatAck.
const (
	AckReasonAuthFailed = "auth_failed" // token failed signature/claims verification
	AckReasonInactive   = "inactive"    // token valid but revoked or expired in the store
	AckReasonInternal   = "internal"    // router-side storage or enrolment lookup failure
)

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
	ConnectionState     string   `json:"connection_state,omitempty"`
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

// HeartbeatAck is the router's reply to a heartbeat, sent on the same pipe the
// heartbeat arrived on. OK true means the heartbeat authenticated and the node
// is registered (NodeID is the router-verified node name from the token's
// subject claim). OK false means the heartbeat was rejected: Reason is one of
// the AckReason* constants and Detail carries a human-readable explanation
// (never the token itself).
type HeartbeatAck struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	NodeID string `json:"node_id,omitempty"`
}
