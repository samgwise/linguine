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

	"github.com/gofiber/fiber/v3"

	"github.com/samgw/linguine/internal/audit"
	"github.com/samgw/linguine/internal/auth"
	"github.com/samgw/linguine/internal/fleet"
	"github.com/samgw/linguine/internal/store"
)

const testSecret = "test-session-secret"

// newTestServer builds a dashboard backed by a fresh SQLite database with one
// enrolled node, one admin key, and a fixed nodes view. It returns the server,
// the database handle, and the admin key id so tests can issue a session
// cookie that requireSession's ActiveByID check will accept.
func newTestServer(t *testing.T) (*Server, *sql.DB, string) {
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
		Keys:          keys,
		Audit:         auditRepo,
		Nodes:         nodes,
		Listen:        "127.0.0.1:0",
		SessionSecret: []byte(testSecret),
	})
	return srv, db, ak.ID
}

func TestLoginRejectsNonAdminKey(t *testing.T) {
	srv, db, _ := newTestServer(t)
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
	srv, db, _ := newTestServer(t)
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
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("session cookie not set")
	}
	if !cookie.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	if !cookie.Secure {
		t.Error("session cookie must be Secure")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite: got %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/admin" {
		t.Errorf("session cookie Path: got %q, want /admin", cookie.Path)
	}
	if _, ok := srv.verifySessionCookie(cookie.Value); !ok {
		t.Error("issued cookie failed verification")
	}
}

func TestRequiresSessionRedirectsWithoutCookie(t *testing.T) {
	srv, _, _ := newTestServer(t)
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
	srv, _, adminKeyID := newTestServer(t)
	cookie := srv.issueSessionCookie(adminKeyID)
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
	if !strings.Contains(string(body), `hx-trigger="every 5s"`) {
		t.Error("nodes page should include the auto-refresh trigger")
	}
}

func TestDashboardRendersAuditEmpty(t *testing.T) {
	srv, _, adminKeyID := newTestServer(t)
	cookie := srv.issueSessionCookie(adminKeyID)
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

// TestXSSRendering asserts that attacker-controlled node and audit fields are
// HTML-escaped on the dashboard, never injected raw — the stored-XSS fix.
func TestXSSRendering(t *testing.T) {
	srv, db, adminKeyID := newTestServer(t)
	srv.nodes = func() []fleet.NodeView {
		return []fleet.NodeView{{
			ID:          "<script>alert('node')</script>",
			Status:      "online",
			ActiveModel: "<img src=x onerror=alert('model')>",
			Catalog:     []string{"<script>alert('cat')</script>"},
		}}
	}
	if _, err := db.Exec(
		`INSERT INTO request_audit_logs (model_requested, model_served, status_code) VALUES (?, ?, 200)`,
		"<img src=x onerror=alert('audit')>", "",
	); err != nil {
		t.Fatalf("insert audit: %v", err)
	}
	cookie := srv.issueSessionCookie(adminKeyID)

	// Nodes page: the node id, active model, and catalog must be escaped.
	req := httptest.NewRequest("GET", "/admin/nodes", nil)
	req.Header.Set("Cookie", cookieName+"="+cookie)
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("nodes status: got %d, want 200", resp.StatusCode)
	}
	if strings.Contains(string(body), "<script>alert('node')</script>") {
		t.Error("nodes page leaked raw <script> node id (XSS)")
	}
	if strings.Contains(string(body), "onerror=alert('model')") {
		t.Error("nodes page leaked raw onerror payload (XSS)")
	}
	if strings.Contains(string(body), "<script>alert('cat')</script>") {
		t.Error("nodes page leaked raw catalog <script> (XSS)")
	}
	if !strings.Contains(string(body), "&lt;script&gt;") {
		t.Error("nodes page should contain the HTML-escaped script text")
	}

	// Audit page: the model_requested payload must be escaped.
	req2 := httptest.NewRequest("GET", "/admin/audit", nil)
	req2.Header.Set("Cookie", cookieName+"="+cookie)
	resp2, err := srv.App().Test(req2)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if strings.Contains(string(body2), "onerror=alert('audit')") {
		t.Error("audit page leaked raw onerror payload (XSS)")
	}
}

// TestSessionRevokedRedirects asserts that revoking an admin key kills its
// outstanding session immediately (requireSession re-checks ActiveByID).
func TestSessionRevokedRedirects(t *testing.T) {
	srv, db, adminKeyID := newTestServer(t)
	cookie := srv.issueSessionCookie(adminKeyID)

	// Sanity: the dashboard is reachable before revocation.
	req := httptest.NewRequest("GET", "/admin", nil)
	req.Header.Set("Cookie", cookieName+"="+cookie)
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("before revoke: got %d, want 200", resp.StatusCode)
	}

	if _, err := db.Exec(`UPDATE api_keys SET status = 'revoked' WHERE id = ?`, adminKeyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	req2 := httptest.NewRequest("GET", "/admin", nil)
	req2.Header.Set("Cookie", cookieName+"="+cookie)
	resp2, err := srv.App().Test(req2)
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("after revoke: got %d, want %d (redirect to login)", resp2.StatusCode, http.StatusSeeOther)
	}
	if loc := resp2.Header.Get("Location"); loc != "/admin/login" {
		t.Errorf("after revoke location: got %q want /admin/login", loc)
	}
}

// TestSessionCookieTamperingRejected verifies that every mutation of the
// three cookie segments (plus structural and wrong-secret variants) is
// refused by verifySessionCookie.
func TestSessionCookieTamperingRejected(t *testing.T) {
	srv, _, adminKeyID := newTestServer(t)
	good := srv.issueSessionCookie(adminKeyID)
	parts := strings.Split(good, "|")
	if len(parts) != 3 {
		t.Fatalf("cookie segments: got %d, want 3", len(parts))
	}
	keyID, exp, mac := parts[0], parts[1], parts[2]
	cases := map[string]string{
		"empty":         "",
		"one-segment":   keyID,
		"two-segments":  keyID + "|" + exp,
		"four-segments": good + "|extra",
		"bad-expiry":    keyID + "|notanumber|" + mac,
		"past-expiry":   keyID + "|1|" + mac,
		"tampered-key":  "x" + keyID + "|" + exp + "|" + mac,
		"tampered-mac":  keyID + "|" + exp + "|" + "0" + mac,
		"truncated-mac": keyID + "|" + exp + "|" + mac[:len(mac)-8],
	}
	for name, c := range cases {
		if _, ok := srv.verifySessionCookie(c); ok {
			t.Errorf("cookie %q should be rejected but was accepted", name)
		}
	}
	// A cookie signed with a different secret must be rejected.
	other := &Server{sessionSecret: []byte("a-different-secret")}
	if _, ok := other.verifySessionCookie(good); ok {
		t.Error("cookie signed with a different secret should be rejected")
	}
}

// TestLoginThrottle asserts that repeated failed logins from one IP are
// throttled with 429 once the per-window limit is exceeded.
func TestLoginThrottle(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// The first `loginLimit` empty-password attempts fail with 401.
	for i := 1; i <= loginLimit; i++ {
		req := httptest.NewRequest("POST", "/admin/login", strings.NewReader("password="))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := srv.App().Test(req)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want %d", i, resp.StatusCode, http.StatusUnauthorized)
		}
	}
	// The next attempt is refused with 429 before the credential check.
	req := httptest.NewRequest("POST", "/admin/login", strings.NewReader("password="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("throttled attempt: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("throttled attempt: got %d, want %d", resp.StatusCode, http.StatusTooManyRequests)
	}
}

// TestFailedLoginAudited asserts that a bad admin login produces an
// admin_audit_logs row and that the audit page surfaces admin auth events.
func TestFailedLoginAudited(t *testing.T) {
	srv, db, adminKeyID := newTestServer(t)
	req := httptest.NewRequest("POST", "/admin/login", strings.NewReader("password=sk-mesh-wrongvalue"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login status: got %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM admin_audit_logs WHERE event = 'login_failed'`).Scan(&n); err != nil {
		t.Fatalf("count admin events: %v", err)
	}
	if n < 1 {
		t.Error("expected a login_failed admin audit row")
	}

	// The audit page must render the admin auth events section.
	cookie := srv.issueSessionCookie(adminKeyID)
	req2 := httptest.NewRequest("GET", "/admin/audit", nil)
	req2.Header.Set("Cookie", cookieName+"="+cookie)
	resp2, err := srv.App().Test(req2)
	if err != nil {
		t.Fatalf("audit page: %v", err)
	}
	defer resp2.Body.Close()
	body, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(body), "Admin auth events") {
		t.Error("audit page should render the admin auth events section")
	}
	if !strings.Contains(string(body), "login_failed") {
		t.Error("audit page should list the login_failed event")
	}
	_ = adminKeyID
}

// keep fiber referenced for the cookie SameSite constant used above.
var _ = fiber.CookieSameSiteLaxMode
