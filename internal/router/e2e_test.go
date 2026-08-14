package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/engine"
	"github.com/samgw/linguine/internal/testutil"
	"github.com/samgw/linguine/internal/worker"
)

// markerStreamStub returns an httptest server that emits one SSE event
// carrying the worker's marker, then data: [DONE].
func markerStreamStub(t *testing.T, marker string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		io.WriteString(w, "data: {\"marker\":\""+marker+"\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// startMarkerDaemon enrolls a worker named nodeID, points it at the stub URL,
// and runs it with a fast heartbeat so staleness can be exercised quickly.
func (h *testHarness) startMarkerDaemon(t *testing.T, nodeID, engineURL string) *worker.Daemon {
	t.Helper()
	_, token, err := h.enrollments.Create(context.Background(), nodeID, 0)
	if err != nil {
		t.Fatalf("create enrollment: %v", err)
	}
	eng := engine.NewProxyEngine(engineURL)
	d, err := worker.NewDaemon(h.nngAddr, nodeID, token, eng,
		worker.WithHeartbeatInterval(100*time.Millisecond))
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = d.Run(ctx) }()
	t.Cleanup(func() { cancel(); _ = d.Close() })
	return d
}

func postStream(t *testing.T, h *testHarness, url string) (string, int) {
	t.Helper()
	resp := waitForWorker(t, h, url, `{"stream":true}`)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body), resp.StatusCode
}

// TestE2ERetarget exercises two workers and proves the router serves via the
// first-registered worker, then retargets to the second once the first goes
// stale — the multi-machine robustness path.
func TestE2ERetarget(t *testing.T) {
	const stale = 400 * time.Millisecond
	h := setup(t, stale)

	stubA := markerStreamStub(t, "A")
	stubB := markerStreamStub(t, "B")
	dA := h.startMarkerDaemon(t, "node-a", stubA.URL)
	h.startMarkerDaemon(t, "node-b", stubB.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := h.server.Start(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.server.Shutdown()

	url := "http://" + ln.Addr().String() + "/v1/chat/completions"

	// Both workers registered; the first-inserted (node-a) is selected.
	body, status := postStream(t, h, url)
	if status != http.StatusOK {
		t.Fatalf("request 1 status: got %d, want %d", status, http.StatusOK)
	}
	if !strings.Contains(body, "\"marker\":\"A\"") {
		t.Errorf("request 1 should be served by worker A:\n got %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("request 1 should end with data: [DONE]:\n got %q", body)
	}

	// Stop worker A and wait for it to go stale so the router retargets to B.
	dA.Close()
	time.Sleep(stale + 250*time.Millisecond)

	body, status = postStream(t, h, url)
	if status != http.StatusOK {
		t.Fatalf("request 2 status: got %d, want %d", status, http.StatusOK)
	}
	if !strings.Contains(body, "\"marker\":\"B\"") {
		t.Errorf("request 2 should be served by worker B after A went stale:\n got %q", body)
	}
}

// TestE2ENonStream proves a buffered (stream=false) completion round-trips
// through a real worker.
func TestE2ENonStream(t *testing.T) {
	h := setup(t, 0)
	upstream := `{"id":"chat-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`
	stub := testutil.NewNonStreamingEngineStub(t, upstream)
	h.startDaemon(t, stub.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := h.server.Start(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.server.Shutdown()

	url := "http://" + ln.Addr().String() + "/v1/chat/completions"
	resp := waitForWorker(t, h, url, `{"stream":false}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != upstream {
		t.Errorf("buffered body mismatch:\n got %q\nwant %q", got, upstream)
	}
}

// TestE2EErrorFrame proves a worker whose engine is unreachable yields a
// clean termination (an SSE error event for stream, 502 JSON for non-stream),
// not a hang.
func TestE2EErrorFrame(t *testing.T) {
	h := setup(t, 0)
	// A non-listening engine URL makes the worker fail to proxy.
	h.startDaemon(t, "http://127.0.0.1:1/v1/chat/completions")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := h.server.Start(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.server.Shutdown()

	url := "http://" + ln.Addr().String() + "/v1/chat/completions"

	// Streaming: expect a clean SSE error event, not a hang.
	resp := waitForWorker(t, h, url, `{"stream":true}`)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(got), "error") {
		t.Errorf("stream error should contain an error event:\n got %q", got)
	}

	// Non-streaming: expect 502 JSON, not a hang.
	resp2 := waitForWorker(t, h, url, `{"stream":false}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadGateway {
		t.Errorf("non-stream error status: got %d, want %d", resp2.StatusCode, http.StatusBadGateway)
	}
}
