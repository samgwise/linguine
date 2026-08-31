package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHeartbeatJSONRoundTrip(t *testing.T) {
	hb := Heartbeat{
		NodeID:          "node-gpu-sydney",
		EnrollmentToken: "v4.public.token",
		ActiveModel:     "llama-3.1-8b-instruct",
		Catalog:         []string{"llama-3.1-8b-instruct", "mistral-7b-v0.3", "deepseek-coder-6.7b"},
		VRAMTotalMB:     24576,
		VRAMFreeMB:      18200,
		ActiveRequests:  2,
		EstimatedTPS:    42.5,
	}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Heartbeat
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.NodeID != hb.NodeID {
		t.Errorf("node_id: got %q want %q", got.NodeID, hb.NodeID)
	}
	if got.ActiveModel != hb.ActiveModel {
		t.Errorf("active_model: got %q want %q", got.ActiveModel, hb.ActiveModel)
	}
	if len(got.Catalog) != len(hb.Catalog) {
		t.Fatalf("catalog: got %d items want %d", len(got.Catalog), len(hb.Catalog))
	}
	for i := range hb.Catalog {
		if got.Catalog[i] != hb.Catalog[i] {
			t.Errorf("catalog[%d]: got %q want %q", i, got.Catalog[i], hb.Catalog[i])
		}
	}
	if got.VRAMTotalMB != hb.VRAMTotalMB || got.VRAMFreeMB != hb.VRAMFreeMB {
		t.Errorf("vram: got %d/%d want %d/%d", got.VRAMTotalMB, got.VRAMFreeMB, hb.VRAMTotalMB, hb.VRAMFreeMB)
	}
	if got.ActiveRequests != hb.ActiveRequests {
		t.Errorf("active_requests: got %d want %d", got.ActiveRequests, hb.ActiveRequests)
	}
	if got.EstimatedTPS != hb.EstimatedTPS {
		t.Errorf("estimated_tps: got %v want %v", got.EstimatedTPS, hb.EstimatedTPS)
	}
}

func TestHeartbeatAckJSONRoundTrip(t *testing.T) {
	ack := HeartbeatAck{
		OK:     true,
		NodeID: "node-gpu-sydney",
	}
	data, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got HeartbeatAck
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.OK || got.NodeID != ack.NodeID || got.Reason != "" || got.Detail != "" {
		t.Errorf("accept ack round-trip mismatch: %+v", got)
	}

	rej := HeartbeatAck{
		OK:     false,
		Reason: AckReasonAuthFailed,
		Detail: "verify enrollment token: bad signature",
	}
	data, err = json.Marshal(rej)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = HeartbeatAck{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.OK || got.Reason != AckReasonAuthFailed || got.Detail != rej.Detail {
		t.Errorf("reject ack round-trip mismatch: %+v", got)
	}
}

func TestHeartbeatConnectionStateRoundTrip(t *testing.T) {
	hb := Heartbeat{
		NodeID:          "node-x",
		EnrollmentToken: "tok",
		ConnectionState: ConnStateDegraded,
	}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"connection_state":"degraded"`) {
		t.Errorf("connection_state missing from wire format: %s", data)
	}
	var got Heartbeat
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ConnectionState != ConnStateDegraded {
		t.Errorf("connection_state: got %q want %q", got.ConnectionState, ConnStateDegraded)
	}
	// An empty state (older worker) must serialise away entirely.
	hb.ConnectionState = ""
	data, _ = json.Marshal(hb)
	if strings.Contains(string(data), "connection_state") {
		t.Errorf("empty connection_state should be omitted: %s", data)
	}
}

func TestHeartbeatPhase2FieldsOmitEmpty(t *testing.T) {
	// When unset, the Phase 2 KV-cache fields must serialise away (omitempty)
	// and decode as zero values, so a Phase 1a heartbeat is identical to the
	// old shape on the wire.
	hb := Heartbeat{
		NodeID:          "node-x",
		EnrollmentToken: "tok",
		ActiveModel:     "llama-3.1-8b-instruct",
	}
	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Heartbeat
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ActiveConversations != 0 {
		t.Errorf("active_conversations: got %d want 0", got.ActiveConversations)
	}
	if got.CachedTokens != 0 {
		t.Errorf("cached_tokens: got %d want 0", got.CachedTokens)
	}
	if got.PinnedSessions != nil {
		t.Errorf("pinned_sessions: got %v want nil", got.PinnedSessions)
	}
}
