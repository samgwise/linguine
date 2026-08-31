package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/engine"
	"github.com/samgw/linguine/internal/mesh"
	"github.com/samgw/linguine/internal/protocol"
	"github.com/samgw/linguine/internal/testutil"
)

// sendAck sends a heartbeat ack to the worker pipe the way the router does.
func sendAck(t *testing.T, r *mesh.Router, pipe mesh.PipeID, ack *protocol.HeartbeatAck) {
	t.Helper()
	payload, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshal ack: %v", err)
	}
	env := &protocol.Envelope{ReqID: protocol.HeartbeatAckReqID, Payload: payload}
	if err := r.SendTo(pipe, env.Encode()); err != nil {
		t.Fatalf("send ack: %v", err)
	}
}

// waitForState polls d.State until it equals want or the deadline passes.
func waitForState(t *testing.T, d *Daemon, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d.State() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state never reached %q (stuck at %q)", want, d.State())
}

// newAckTestDaemon starts a daemon against a fresh inproc router and returns
// both, plus the worker's pipe id learned from its first heartbeat.
func newAckTestDaemon(t *testing.T, nodeID string, interval time.Duration) (*mesh.Router, *Daemon, mesh.PipeID) {
	t.Helper()
	router, err := mesh.NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	addr := uniqueWorkerAddr(t)
	if err := router.Listen(addr, nil); err != nil {
		t.Fatalf("listen: %v", err)
	}
	// A non-listening engine keeps the job path inert; ack tests never dispatch.
	eng := engine.NewProxyEngine("http://127.0.0.1:1/v1/chat/completions")
	d, err := NewDaemon(addr, nodeID, "dummy-token", eng, WithHeartbeatInterval(interval))
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = d.Close(); _ = router.Close() })
	go func() { _ = d.Run(ctx) }()

	pipe := waitForHeartbeat(t, router)
	return router, d, pipe
}

func TestDaemonRegistersOnAcceptAck(t *testing.T) {
	router, d, pipe := newAckTestDaemon(t, "node-ack", time.Hour)
	if d.State() != protocol.ConnStateConnecting {
		t.Fatalf("pre-ack state: got %q want connecting", d.State())
	}
	sendAck(t, router, pipe, &protocol.HeartbeatAck{OK: true, NodeID: "node-ack"})
	waitForState(t, d, protocol.ConnStateRegistered)
}

func TestDaemonRejectAckKeepsConnecting(t *testing.T) {
	router, d, pipe := newAckTestDaemon(t, "node-reject", time.Hour)
	sendAck(t, router, pipe, &protocol.HeartbeatAck{OK: false, Reason: protocol.AckReasonAuthFailed, Detail: "bad signature"})
	// Rejected: the worker must NOT claim registered, and must remember why.
	waitForState(t, d, protocol.ConnStateConnecting)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d.lastRejectReason() == protocol.AckReasonAuthFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := d.lastRejectReason(); got != protocol.AckReasonAuthFailed {
		t.Fatalf("reject reason: got %q want auth_failed", got)
	}

	// A later acceptance must still register it (fix-the-token flow).
	sendAck(t, router, pipe, &protocol.HeartbeatAck{OK: true, NodeID: "node-reject"})
	waitForState(t, d, protocol.ConnStateRegistered)
}

func TestDaemonDegradedWithoutAcks(t *testing.T) {
	router, d, pipe := newAckTestDaemon(t, "node-degraded", 50*time.Millisecond)
	sendAck(t, router, pipe, &protocol.HeartbeatAck{OK: true, NodeID: "node-degraded"})
	waitForState(t, d, protocol.ConnStateRegistered)
	// Stop acking; degradedAfter with a 50ms interval is 150ms, so the
	// daemon should demote well within the deadline.
	waitForState(t, d, protocol.ConnStateDegraded)
	// And recovery on the next ack restores registration.
	sendAck(t, router, pipe, &protocol.HeartbeatAck{OK: true, NodeID: "node-degraded"})
	waitForState(t, d, protocol.ConnStateRegistered)
}

func TestDaemonAckTrafficDoesNotDisturbJobs(t *testing.T) {
	lines := testutil.SSELines("ok")
	srv := testutil.NewStreamingEngineStub(t, lines)
	router, err := mesh.NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	addr := uniqueWorkerAddr(t)
	if err := router.Listen(addr, nil); err != nil {
		t.Fatalf("listen: %v", err)
	}
	eng := engine.NewProxyEngine(srv.URL)
	d, err := NewDaemon(addr, "node-jobs", "dummy-token", eng, WithHeartbeatInterval(time.Second))
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = d.Close(); _ = router.Close() })
	go func() { _ = d.Run(ctx) }()

	pipe := waitForHeartbeat(t, router)
	// Interleave acks and a job; the job must still stream back intact.
	sendAck(t, router, pipe, &protocol.HeartbeatAck{OK: false, Reason: protocol.AckReasonInactive})
	jobEnv := &protocol.Envelope{ReqID: "job-ack-mix", Payload: []byte(`{"stream":true}`)}
	if err := router.SendTo(pipe, jobEnv.Encode()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	gotEOF := false
	for !gotEOF {
		msg, err := router.Recv()
		if err != nil {
			t.Fatalf("recv reply: %v", err)
		}
		env, err := protocol.DecodeEnvelope(msg.Body)
		msg.Free()
		if err != nil {
			t.Fatalf("decode env: %v", err)
		}
		if env.ReqID != "job-ack-mix" {
			continue // heartbeat or other control traffic
		}
		f, err := protocol.DecodeFrame(env.Payload)
		if err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		switch f.Type {
		case protocol.FrameTypeChunk, protocol.FrameTypeEOF:
			if f.Type == protocol.FrameTypeEOF {
				gotEOF = true
			}
		default:
			t.Fatalf("unexpected frame type %d", f.Type)
		}
	}
}
