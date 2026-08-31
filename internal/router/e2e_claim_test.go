package router

import (
	"context"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/engine"
	"github.com/samgw/linguine/internal/protocol"
	"github.com/samgw/linguine/internal/worker"
)

// TestE2EBadTokenClaimNotRegistered mirrors the original production bug: a
// worker configured with a wrong enrollment token (e.g. an API key pasted
// where a v4.public enrollment token belongs) must NOT register — but it
// must surface on the dashboard as a connecting claim with the rejection
// reason, instead of being invisible while claiming to be connected.
func TestE2EBadTokenClaimNotRegistered(t *testing.T) {
	h := setup(t, 0)
	eng := engine.NewProxyEngine("http://127.0.0.1:1/v1/chat/completions")
	d, err := worker.NewDaemon(h.nngAddr, "gpu-supernumeraryII", "sk-mesh-wrong-kind-of-token", eng,
		worker.WithHeartbeatInterval(100*time.Millisecond))
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = d.Close() })
	go func() { _ = d.Run(ctx) }()

	serverCtx, stop := context.WithCancel(context.Background())
	defer stop()
	ln, err := h.server.Start(serverCtx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.server.Shutdown()
	_ = ln

	// The worker must stay unregistered...
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.server.NodesSnapshot()) > 0 {
			t.Fatalf("worker with bad token registered: %+v", h.server.NodesSnapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ...but appear as a claim with the auth failure reason.
	claims := h.server.ClaimsSnapshot()
	if len(claims) != 1 {
		t.Fatalf("claims: got %d want 1", len(claims))
	}
	if claims[0].ID != "gpu-supernumeraryII" || claims[0].ClaimReason != protocol.AckReasonAuthFailed {
		t.Errorf("claim: got %+v want gpu-supernumeraryII/auth_failed", claims[0])
	}
	// The dashboard merge must render it as connecting, never online.
	view := claims[0]
	if view.Status != "connecting" || view.ConnectionState != protocol.ConnStateConnecting {
		t.Errorf("dashboard view: got %+v want connecting/connecting", view)
	}
}

// TestE2EGoodTokenRegistersOnline is the counterfactual: the same worker
// shape with a real enrollment token registers and reports online.
func TestE2EGoodTokenRegistersOnline(t *testing.T) {
	h := setup(t, 0)
	stub := markerStreamStub(t, "ok")
	_, token, err := h.enrollments.Create(context.Background(), "gpu-supernumeraryII", 0)
	if err != nil {
		t.Fatalf("create enrollment: %v", err)
	}
	eng := engine.NewProxyEngine(stub.URL)
	d, err := worker.NewDaemon(h.nngAddr, "gpu-supernumeraryII", token, eng,
		worker.WithHeartbeatInterval(100*time.Millisecond))
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = d.Close() })
	go func() { _ = d.Run(ctx) }()

	serverCtx, stop := context.WithCancel(context.Background())
	defer stop()
	ln, err := h.server.Start(serverCtx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.server.Shutdown()
	_ = ln

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		nodes := h.server.NodesSnapshot()
		if len(nodes) == 1 && nodes[0].ID == "gpu-supernumeraryII" && nodes[0].Status == "online" {
			// Registered, online, and no lingering claim.
			if claims := h.server.ClaimsSnapshot(); len(claims) != 0 {
				t.Fatalf("claims after registration: %+v", claims)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("worker never registered online: %+v", h.server.NodesSnapshot())
}
