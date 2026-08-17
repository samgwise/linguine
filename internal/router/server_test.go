package router

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/audit"
	"github.com/samgw/linguine/internal/auth"
	"github.com/samgw/linguine/internal/config"
	"github.com/samgw/linguine/internal/engine"
	"github.com/samgw/linguine/internal/mesh"
	"github.com/samgw/linguine/internal/store"
	"github.com/samgw/linguine/internal/testutil"
	"github.com/samgw/linguine/internal/worker"
)

var routerAddrCounter uint64

func uniqueRouterAddr(t *testing.T) string {
	t.Helper()
	n := atomic.AddUint64(&routerAddrCounter, 1)
	return fmt.Sprintf("inproc://linguine-router-%d-%d", n, time.Now().UnixNano())
}

type testHarness struct {
	server      *Server
	signer      *auth.Signer
	keys        *auth.APIKeyRepo
	enrollments *auth.EnrollmentRepo
	db          *sql.DB
	key         string // raw API key
	nngAddr     string
}

func setup(t *testing.T, staleAfter time.Duration) *testHarness {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "router-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db := st.DB()
	signer := auth.NewRandomSigner()
	keys := auth.NewAPIKeyRepo(db)
	enrollments := auth.NewEnrollmentRepo(db, signer)

	nng, err := mesh.NewRouter()
	if err != nil {
		t.Fatalf("new mesh router: %v", err)
	}
	t.Cleanup(func() { _ = nng.Close() })
	addr := uniqueRouterAddr(t)
	if err := nng.Listen(addr, nil); err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := New(Deps{
		NNG:         nng,
		Signer:      signer,
		Keys:        keys,
		Enrollments: enrollments,
		Audit:       audit.NewRepo(db, 256),
		DB:          db,
		HTTPListen:  "127.0.0.1:0",
		TLS:         config.TLSFiles{},
		StaleAfter:  staleAfter,
	})

	raw := auth.GenerateAPIKey()
	if _, err := keys.Create(context.Background(), "test-key", raw); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return &testHarness{
		server: srv, signer: signer, keys: keys, enrollments: enrollments, db: db,
		key: raw, nngAddr: addr,
	}
}

// startDaemon enrolls a worker, points it at the stub engine URL, and runs it
// in the background for the test's lifetime.
func (h *testHarness) startDaemon(t *testing.T, engineURL string) *worker.Daemon {
	t.Helper()
	_, token, err := h.enrollments.Create(context.Background(), "node-test", 0)
	if err != nil {
		t.Fatalf("create enrollment: %v", err)
	}
	eng := engine.NewProxyEngine(engineURL)
	d, err := worker.NewDaemon(h.nngAddr, "node-test", token, eng,
		worker.WithHeartbeatInterval(200*time.Millisecond))
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = d.Run(ctx) }()
	t.Cleanup(func() { cancel(); _ = d.Close() })
	return d
}

// waitForWorker retries the request until the worker has registered (status
// != 503) or the deadline passes, returning the first non-503 response.
func waitForWorker(t *testing.T, h *testHarness, url, body string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	for i := 0; i < 40; i++ {
		req, err := http.NewRequest("POST", url, strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+h.key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		if resp.StatusCode != http.StatusServiceUnavailable {
			return resp
		}
		resp.Body.Close()
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("worker never registered (503 persisted)")
	return nil
}

func TestAuthMissingKey(t *testing.T) {
	h := setup(t, 0)
	req, err := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := h.server.App().Test(req)
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestAuthWrongKey(t *testing.T) {
	h := setup(t, 0)
	req, err := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer sk-mesh-wrongvalue")
	resp, err := h.server.App().Test(req)
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestNoWorkerAvailable(t *testing.T) {
	h := setup(t, 0)
	req, err := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	resp, err := h.server.App().Test(req)
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestStreamingRoundTrip(t *testing.T) {
	h := setup(t, 0)
	lines := testutil.SSELines("Hel", "lo", " world")
	stub := testutil.NewStreamingEngineStub(t, lines)
	h.startDaemon(t, stub.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := h.server.Start(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.server.Shutdown()

	url := "http://" + ln.Addr().String() + "/v1/chat/completions"
	resp := waitForWorker(t, h, url, `{"stream":true}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	want := testutil.JoinLines(lines)
	if string(got) != want {
		t.Errorf("streamed body mismatch:\n got %q\nwant %q", got, want)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type: got %q, want text/event-stream", ct)
	}
}

func TestNonStreamRoundTrip(t *testing.T) {
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
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type: got %q, want application/json", ct)
	}
}
