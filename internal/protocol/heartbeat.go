package protocol

// HeartbeatReqID is the envelope reqID used for worker→router heartbeat
// control messages. It never collides with a dispatch reqID because the
// router generates dispatch reqIDs as UUIDs.
const HeartbeatReqID = "hb"

// Heartbeat is the control payload a worker sends to the router to announce
// itself, authenticate via its enrollment token, and refresh its liveness.
// The router verifies the EnrollmentToken (PASETO v4) and records the node
// along with the NNG pipe id it arrived on.
type Heartbeat struct {
	NodeID          string `json:"node_id"`
	EnrollmentToken string `json:"enrollment_token"`
	ActiveModel     string `json:"active_model,omitempty"`
}
