package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/admin"
	"github.com/samgw/linguine/internal/auth"
)

// blockingEngineStub holds a request open until release is closed, simulating an
// in-flight job that keeps the worker's activeRequests counter at 1 so the
// router's least-connections selection sees it as loaded.
func blockingEngineStub(t *testing.T, marker string, release <-chan struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// Write the first chunk before blocking so the worker forwards it,
		// the router flushes (sending response headers), and the client
		// receives headers without deadlocking on release.
		io.WriteString(w, "data: {\"marker\":\""+marker+"\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		<-release // hold the stream open until released
		io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPhase1aIntegration exercises the three new Phase 1a capabilities together:
// least-connections selection routes around a loaded node, the audit log
// records the request, and the admin dashboard (logged in via an admin key)
// renders both nodes and the recent request.
func TestPhase1aIntegration(t *testing.T) {
	const stale = time.Hour // keep both nodes non-stale throughout
	h := setup(t, stale)

	releaseA := make(chan struct{})
	var once sync.Once
	stubA := blockingEngineStub(t, "A", releaseA)
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

	// Request 1 goes to node-a (tie on 0, first-inserted wins) and holds it
	// open, keeping node-a's activeRequests at 1.
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req1, _ := http.NewRequest("POST", url, strings.NewReader(`{"stream":true}`))
	req1.Header.Set("Authorization", "Bearer "+h.key)
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("request 1: %v", err)
	}
	defer resp1.Body.Close()

	// Wait for node-a's heartbeat to carry activeRequests=1 to the router so
	// least-connections sees node-b (0) as less loaded.
	time.Sleep(300 * time.Millisecond)

	// Request 2 must route to node-b (fewer active requests).
	body2, status2 := postStream(t, h, url)
	if status2 != http.StatusOK {
		t.Fatalf("request 2 status: got %d, want %d", status2, http.StatusOK)
	}
	if !strings.Contains(body2, "\"marker\":\"B\"") {
		t.Errorf("request 2 should route to the less-loaded node-b:\n got %q", body2)
	}

	// Release request 1 and confirm it completes from node-a.
	once.Do(func() { close(releaseA) })
	body1, _ := io.ReadAll(resp1.Body)
	if !strings.Contains(string(body1), "\"marker\":\"A\"") {
		t.Errorf("request 1 should be served by node-a:\n got %q", body1)
	}
	_ = dA // keep the daemon reference alive

	// Wait for the async audit writer to flush both requests.
	time.Sleep(800 * time.Millisecond)
	entries, err := h.server.AuditRepo().Recent(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("audit recent: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("audit entries: got %d want at least 2", len(entries))
	}
	// Both requests should be recorded with their serving node and a 200.
	nodes := map[string]bool{}
	for _, e := range entries {
		if e.StatusCode != http.StatusOK {
			t.Errorf("audit status: got %d want 200", e.StatusCode)
		}
		nodes[e.NodeID] = true
	}
	if !nodes["node-a"] || !nodes["node-b"] {
		t.Errorf("audit should record both nodes, got %v", nodes)
	}

	// Log into the dashboard with an admin key and confirm it renders both
	// nodes and the recent request.
	adminKey := auth.GenerateAPIKey()
	ak, err := h.keys.Create(context.Background(), "dashboard-admin", adminKey)
	if err != nil {
		t.Fatalf("create admin key: %v", err)
	}
	if _, err := h.db.Exec(`UPDATE api_keys SET role = 'admin' WHERE id = ?`, ak.ID); err != nil {
		t.Fatalf("set admin role: %v", err)
	}
	secret := []byte("phase1a-test-secret")
	adminSrv := admin.New(admin.Deps{
		Keys:         h.keys,
		Audit:        h.server.AuditRepo(),
		Nodes:        h.server.NodesSnapshot,
		Listen:       "127.0.0.1:0",
		SessionSecret: secret,
	})
	adminLn, err := adminSrv.Start()
	if err != nil {
		t.Fatalf("start admin: %v", err)
	}
	defer adminSrv.Shutdown()

	dashURL := "http://" + adminLn.Addr().String()
	// Sign in to get the session cookie.
	loginReq, _ := http.NewRequest("POST", dashURL+"/admin/login", strings.NewReader("password="+adminKey))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResp, err := client.Do(loginReq)
	if err != nil {
		t.Fatalf("admin login: %v", err)
	}
	if loginResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("admin login status: got %d, want %d", loginResp.StatusCode, http.StatusSeeOther)
	}
	var cookie string
	for _, c := range loginResp.Cookies() {
		if c.Name == "linguine_admin" {
			cookie = c.Value
		}
	}
	if cookie == "" {
		t.Fatal("admin session cookie not set")
	}
	loginResp.Body.Close()

	// Fetch the nodes page and confirm both nodes render.
	nodesReq, _ := http.NewRequest("GET", dashURL+"/admin/nodes", nil)
	nodesReq.Header.Set("Cookie", "linguine_admin="+cookie)
	nodesResp, err := client.Do(nodesReq)
	if err != nil {
		t.Fatalf("admin nodes: %v", err)
	}
	nodesBody, _ := io.ReadAll(nodesResp.Body)
	nodesResp.Body.Close()
	if nodesResp.StatusCode != http.StatusOK {
		t.Fatalf("admin nodes status: got %d, want %d", nodesResp.StatusCode, http.StatusOK)
	}
	if !strings.Contains(string(nodesBody), "node-a") || !strings.Contains(string(nodesBody), "node-b") {
		t.Error("dashboard should render both nodes")
	}

	// Fetch the audit page and confirm the recent requests render.
	auditReq, _ := http.NewRequest("GET", dashURL+"/admin/audit", nil)
	auditReq.Header.Set("Cookie", "linguine_admin="+cookie)
	auditResp, err := client.Do(auditReq)
	if err != nil {
		t.Fatalf("admin audit: %v", err)
	}
	auditBody, _ := io.ReadAll(auditResp.Body)
	auditResp.Body.Close()
	if auditResp.StatusCode != http.StatusOK {
		t.Fatalf("admin audit status: got %d, want %d", auditResp.StatusCode, http.StatusOK)
	}
	if strings.Contains(string(auditBody), "No requests recorded yet") {
		t.Error("dashboard audit page should show the recorded requests")
	}
}

// (AuditRepo is exported on Server; this test-local alias keeps the call site tidy.)
var _ = (*Server)(nil).AuditRepo
