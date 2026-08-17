package protocol

import (
	"encoding/json"
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
