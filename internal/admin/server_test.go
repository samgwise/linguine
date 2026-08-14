package admin

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/audit"
	"github.com/samgw/linguine/internal/auth"
	"github.com/samgw/linguine/internal/fleet"
	"github.com/samgw/linguine/internal/store"
)

const testSecret = "test-session-secret"

func newTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "admin-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	db := s.DB()
	// Satisfy FKs for the audit log + a node for the dashboard.
	if _, err := db.Exec(
		`INSERT INTO node_enrollment_tokens (id, node_name, status) VALUES ('tok-n', 'node-1', 'active')`); err != nil {
		t.Fatalf("insert enrollment: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO nodes (id, token_id, status) VALUES ('node-1', 'tok-n', 'online')`); err != nil {
		t.Fatalf("insert node: %v", err)
	}
	keys := auth.NewAPIKeyRepo(db)
	raw := auth.GenerateAPIKey()
	ak, err := keys.Create(context.Background(), "admin-key", raw)
	if err != nil {
		t.Fatalf("create admin key: %v", err)
	}
	if _, err := db.Exec(`UPDATE api_keys SET role = 'admin' WHERE id = ?`, ak.ID); err != nil {
		t.Fatalf("set admin role: %v", err)
	}
	auditRepo := audit.NewRepo(db, 64)
	t.Cleanup(func() { _ = auditRepo.Close() })

	nodes := func() []fleet.NodeView {
		return []fleet.NodeView{{
			ID: "node-1", Status: "online", ActiveModel: "llama-3.1-8b",
			Catalog: []string{"llama-3.1-8b", "mistral-7b"}, VRAMTotalMB: 24576,
			VRAMFreeMB: 18200, ActiveRequests: 2, EstimatedTPS: 42.5,
			LastHeartbeat: time.Now(),
		}}
	}
	srv := New(Deps{
		Keys:         keys,
		Audit:        auditRepo,
		Nodes:        nodes,
		Listen:       "127.0.0.1:0",
		SessionSecret: []byte(testSecret),
	})
	return srv, db
}

func TestLoginRejectsNonAdminKey(t *testing.T) {
	srv, db := newTestServer(t)
	// Create a client-role key and try to log in with it.
	keys := auth.NewAPIKeyRepo(db)
	raw := auth.GenerateAPIKey()
	if _, err := keys.Create(context.Background(), "client-key", raw); err != nil {
		t.Fatalf("create client key: %v", err)
	}
	req := httptest.NewRequest("POST", "/admin/login", strings.NewReader("password="+raw))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want %d (client key must not log in)", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestLoginAcceptsAdminKeyAndSetsCookie(t *testing.T) {
	srv, db := newTestServer(t)
	keys := auth.NewAPIKeyRepo(db)
	raw := auth.GenerateAPIKey()
	ak, err := keys.Create(context.Background(), "k", raw)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if _, err := db.Exec(`UPDATE api_keys SET role = 'admin' WHERE id = ?`, ak.ID); err != nil {
		t.Fatalf("set admin role: %v", err)
	}
	req := httptest.NewRequest("POST", "/admin/login", strings.NewReader("password="+raw))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	var cookie string
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			cookie = c.Value
		}
	}
	if cookie == "" {
		t.Fatal("session cookie not set")
	}
	if _, ok := srv.verifySessionCookie(cookie); !ok {
		t.Error("issued cookie failed verification")
	}
}

func TestRequiresSessionRedirectsWithoutCookie(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/admin", nil)
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/login" {
		t.Errorf("location: got %q want /admin/login", loc)
	}
}

func TestDashboardRendersNodes(t *testing.T) {
	srv, _ := newTestServer(t)
	// Issue a session cookie and reuse it.
	cookie := srv.issueSessionCookie("admin-key-id")
	req := httptest.NewRequest("GET", "/admin/nodes", nil)
	req.Header.Set("Cookie", cookieName+"="+cookie)
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if !strings.Contains(string(body), "node-1") {
		t.Error("nodes page should render node-1")
}
	if !strings.Contains(string(body), "llama-3.1-8b") {
		t.Error("nodes page should render the active model")
	}
	if !strings.Contains(string(body), "hx-trigger=\"every 5s\"") {
		t.Error("nodes page should include the auto-refresh trigger")
	}
}

func TestDashboardRendersAuditEmpty(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := srv.issueSessionCookie("admin-key-id")
	req := httptest.NewRequest("GET", "/admin/audit", nil)
	req.Header.Set("Cookie", cookieName+"="+cookie)
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "No requests recorded yet") {
		t.Error("empty audit page should show the empty placeholder")
	}
}
