package router

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/mesh"
	"github.com/samgw/linguine/internal/protocol"
)

// encodeJSON marshals v or fails the test.
func encodeJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// decodeAck unmarshals a HeartbeatAck payload or fails the test.
func decodeAck(t *testing.T, payload []byte) *protocol.HeartbeatAck {
	t.Helper()
	var ack protocol.HeartbeatAck
	if err := json.Unmarshal(payload, &ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	return &ack
}

// sendHeartbeat dials a raw worker to the harness router, sends hb, and
// returns the worker socket.
func sendHeartbeat(t *testing.T, h *testHarness, hb protocol.Heartbeat) *mesh.Worker {
	t.Helper()
	worker, err := mesh.NewWorker()
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	t.Cleanup(func() { _ = worker.Close() })
	if err := worker.Dial(h.nngAddr, nil, ""); err != nil {
		t.Fatalf("dial: %v", err)
	}
	env := &protocol.Envelope{ReqID: protocol.HeartbeatReqID, Payload: encodeJSON(t, hb)}
	if err := worker.Send(env.Encode()); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}
	return worker
}

// recvAck receives from the worker until a heartbeat ack envelope arrives
// (skipping other traffic), or fails after a deadline. The worker socket's
// 2s recv deadline makes each attempt bounded.
func recvAck(t *testing.T, worker *mesh.Worker) *protocol.HeartbeatAck {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := worker.Recv()
		if err != nil {
			continue // recv timeout; retry until the deadline
		}
		env, derr := protocol.DecodeEnvelope(msg.Body)
		msg.Free()
		if derr != nil {
			continue
		}
		if env.ReqID == protocol.HeartbeatAckReqID {
			return decodeAck(t, env.Payload)
		}
	}
	t.Fatal("no heartbeat ack received")
	return nil
}

// TestHeartbeatRejectAckAndClaim sends a heartbeat with a garbage token
// through the live server recv path and verifies the reject ack is delivered
// to the worker and the claim is recorded for the dashboard.
func TestHeartbeatRejectAckAndClaim(t *testing.T) {
	h := setup(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := h.server.Start(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.server.Shutdown()
	_ = ln // HTTP listener; the mesh path is what this test drives

	worker := sendHeartbeat(t, h, protocol.Heartbeat{NodeID: "rogue-node", EnrollmentToken: "not-a-paseto"})
	ack := recvAck(t, worker)
	if ack.OK || ack.Reason != protocol.AckReasonAuthFailed {
		t.Errorf("ack: got %+v want rejected auth_failed", ack)
	}

	// The claim must appear for the dashboard.
	claims := h.server.ClaimsSnapshot()
	if len(claims) != 1 {
		t.Fatalf("claims: got %d want 1", len(claims))
	}
	if claims[0].ID != "rogue-node" || claims[0].ClaimReason != protocol.AckReasonAuthFailed {
		t.Errorf("claim: got %+v want rogue-node/auth_failed", claims[0])
	}

	// The rejected worker must not appear in the registered-node snapshot.
	for _, n := range h.server.NodesSnapshot() {
		if n.ID == "rogue-node" {
			t.Errorf("rejected node leaked into registry: %+v", n)
		}
	}
}

// TestHeartbeatSuccessAckCarriesSubject proves the accept ack names the
// router-verified node (token subject), and that registration retires any
// earlier claim for that node id.
func TestHeartbeatSuccessAckCarriesSubject(t *testing.T) {
	h := setup(t, 0)
	// Seed a claim as if this node had previously knocked with a bad token.
	h.server.nodes.recordClaim("node-ack", protocol.AckReasonAuthFailed, "stale claim", 77)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := h.server.Start(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.server.Shutdown()
	_ = ln

	_, token, err := h.enrollments.Create(context.Background(), "node-ack", 0)
	if err != nil {
		t.Fatalf("create enrollment: %v", err)
	}
	worker := sendHeartbeat(t, h, protocol.Heartbeat{
		NodeID:          "node-ack",
		EnrollmentToken: token,
		ConnectionState: protocol.ConnStateConnecting,
	})
	ack := recvAck(t, worker)
	if !ack.OK || ack.NodeID != "node-ack" {
		t.Errorf("ack: got %+v want ok with node-ack", ack)
	}

	// The node registers...
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.server.NodesSnapshot()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// ...and the earlier claim retires.
	if remaining := h.server.ClaimsSnapshot(); len(remaining) != 0 {
		t.Errorf("claim not retired on registration: %+v", remaining)
	}
	if nodes := h.server.NodesSnapshot(); len(nodes) != 1 || nodes[0].ID != "node-ack" {
		t.Errorf("registry: got %+v want single node-ack", nodes)
	}
}
