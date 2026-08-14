package worker

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/engine"
	"github.com/samgw/linguine/internal/mesh"
	"github.com/samgw/linguine/internal/protocol"
	"github.com/samgw/linguine/internal/testutil"
)

var workerAddrCounter uint64

func uniqueWorkerAddr(t *testing.T) string {
	t.Helper()
	n := atomic.AddUint64(&workerAddrCounter, 1)
	return fmt.Sprintf("inproc://linguine-worker-%d-%d", n, time.Now().UnixNano())
}

// waitForHeartbeat receives messages from the router until a worker heartbeat
// arrives, returning that worker's NNG pipe id.
func waitForHeartbeat(t *testing.T, r *mesh.Router) mesh.PipeID {
	t.Helper()
	for i := 0; i < 10; i++ {
		msg, err := r.Recv()
		if err != nil {
			t.Fatalf("recv waiting for heartbeat: %v", err)
		}
		pipe, perr := mesh.PipeFromHeader(msg.Header)
		env, derr := protocol.DecodeEnvelope(msg.Body)
		msg.Free()
		if derr != nil {
			continue
		}
		if perr != nil {
			t.Fatalf("pipe from header: %v", perr)
		}
		if env.ReqID == protocol.HeartbeatReqID {
			return pipe
		}
	}
	t.Fatal("no heartbeat received")
	return 0
}

func TestDaemonProxiesAndStreams(t *testing.T) {
	lines := testutil.SSELines("Hel", "lo", " world")
	srv := testutil.NewStreamingEngineStub(t, lines)

	router, err := mesh.NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer router.Close()
	addr := uniqueWorkerAddr(t)
	if err := router.Listen(addr); err != nil {
		t.Fatalf("listen: %v", err)
	}

	eng := engine.NewProxyEngine(srv.URL)
	d, err := NewDaemon(addr, "node-test", "dummy-token", eng, WithHeartbeatInterval(time.Second))
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	defer d.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Run(ctx) }()

	pipe := waitForHeartbeat(t, router)

	jobEnv := &protocol.Envelope{ReqID: "job-1", Payload: []byte(`{"stream":true}`)}
	if err := router.SendTo(pipe, jobEnv.Encode()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var got []byte
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
		if env.ReqID != "job-1" {
			t.Fatalf("reqID: got %q, want job-1", env.ReqID)
		}
		f, err := protocol.DecodeFrame(env.Payload)
		if err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		switch f.Type {
		case protocol.FrameTypeChunk:
			got = append(got, f.Payload...)
		case protocol.FrameTypeEOF:
			gotEOF = true
		case protocol.FrameTypeError:
			t.Fatalf("error frame: %s", f.Payload)
		default:
			t.Fatalf("unexpected frame type %d", f.Type)
		}
	}

	want := testutil.JoinLines(lines)
	if string(got) != want {
		t.Errorf("streamed body mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestDaemonProxyErrorSendsErrorFrame(t *testing.T) {
	// A non-listening endpoint makes the engine fail, so the daemon should
	// emit an Error frame rather than hanging.
	router, err := mesh.NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer router.Close()
	addr := uniqueWorkerAddr(t)
	if err := router.Listen(addr); err != nil {
		t.Fatalf("listen: %v", err)
	}

	eng := engine.NewProxyEngine("http://127.0.0.1:1/v1/chat/completions")
	d, err := NewDaemon(addr, "node-err", "dummy-token", eng, WithHeartbeatInterval(time.Second))
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	defer d.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Run(ctx) }()

	pipe := waitForHeartbeat(t, router)
	jobEnv := &protocol.Envelope{ReqID: "job-err", Payload: []byte(`{"stream":true}`)}
	if err := router.SendTo(pipe, jobEnv.Encode()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	msg, err := router.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	env, err := protocol.DecodeEnvelope(msg.Body)
	msg.Free()
	if err != nil {
		t.Fatalf("decode env: %v", err)
	}
	if env.ReqID != "job-err" {
		t.Fatalf("reqID: got %q", env.ReqID)
	}
	f, err := protocol.DecodeFrame(env.Payload)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if f.Type != protocol.FrameTypeError {
		t.Errorf("frame type: got %d, want error", f.Type)
	}
}
