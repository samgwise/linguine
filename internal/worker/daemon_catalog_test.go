package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/catalog"
	"github.com/samgw/linguine/internal/engine"
	"github.com/samgw/linguine/internal/mesh"
	"github.com/samgw/linguine/internal/protocol"
)

func TestDaemonAdvertisesCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"llama-3.1-8b"},{"id":"mistral-7b"}]}`))
	}))
	defer srv.Close()

	router, err := mesh.NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer router.Close()
	addr := uniqueWorkerAddr(t)
	if err := router.Listen(addr); err != nil {
		t.Fatalf("listen: %v", err)
	}

	probe := catalog.NewProbe(srv.URL, catalog.WithRefreshInterval(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Prime the probe synchronously so the catalog is ready before the
	// daemon's first heartbeat fires (avoids a startup race).
	if err := probe.Refresh(ctx); err != nil {
		t.Fatalf("prime probe: %v", err)
	}
	go probe.Run(ctx)

	eng := engine.NewProxyEngine(srv.URL)
	d, err := NewDaemon(addr, "node-cat", "tok", eng,
		WithHeartbeatInterval(100*time.Millisecond),
		WithProbe(probe))
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	defer d.Close()
	go func() { _ = d.Run(ctx) }()

	// Collect a heartbeat and confirm it carries the catalog.
	msg, err := router.Recv()
	if err != nil {
		t.Fatalf("recv heartbeat: %v", err)
	}
	pipe, err := mesh.PipeFromHeader(msg.Header)
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	env, err := protocol.DecodeEnvelope(msg.Body)
	msg.Free()
	if err != nil {
		t.Fatalf("decode env: %v", err)
	}
	if env.ReqID != protocol.HeartbeatReqID {
		t.Fatalf("reqID: got %q want %q", env.ReqID, protocol.HeartbeatReqID)
	}
	var hb protocol.Heartbeat
	if err := json.Unmarshal(env.Payload, &hb); err != nil {
		t.Fatalf("decode heartbeat: %v", err)
	}
	if hb.NodeID != "node-cat" {
		t.Errorf("node_id: got %q want node-cat", hb.NodeID)
	}
	if hb.ActiveModel != "llama-3.1-8b" {
		t.Errorf("active_model: got %q want llama-3.1-8b (first catalog entry)", hb.ActiveModel)
	}
	want := []string{"llama-3.1-8b", "mistral-7b"}
	if len(hb.Catalog) != len(want) {
		t.Fatalf("catalog: got %v want %v", hb.Catalog, want)
	}
	for i := range want {
		if hb.Catalog[i] != want[i] {
			t.Errorf("catalog[%d]: got %q want %q", i, hb.Catalog[i], want[i])
		}
	}
	_ = pipe
}
