package admin

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
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
		Enrollments:   auth.NewEnrollmentRepo(db, auth.NewRandomSigner()),
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

// TestNodesPageHtmxFragment asserts that the every-5s htmx poll of
// /admin/nodes receives only the table fragment — a full page response would
// be swapped inside the old table via outerHTML, nesting page chrome (header,
// main) into the content on every refresh. Plain GETs and boosted navigation
// clicks still receive the full page.
func TestNodesPageHtmxFragment(t *testing.T) {
	srv, _, adminKeyID := newTestServer(t)
	cookie := srv.issueSessionCookie(adminKeyID)

	// Plain GET: full page with chrome.
	req := httptest.NewRequest("GET", "/admin/nodes", nil)
	req.Header.Set("Cookie", cookieName+"="+cookie)
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("plain get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "<!DOCTYPE html>") {
		t.Error("plain GET should return the full page")
	}

	// htmx poll: table fragment only, no chrome.
	req2 := httptest.NewRequest("GET", "/admin/nodes", nil)
	req2.Header.Set("Cookie", cookieName+"="+cookie)
	req2.Header.Set("HX-Request", "true")
	resp2, err := srv.App().Test(req2)
	if err != nil {
		t.Fatalf("htmx poll: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if strings.Contains(string(body2), "<!DOCTYPE html>") || strings.Contains(string(body2), "<header") || strings.Contains(string(body2), "<html") {
		t.Error("htmx poll must receive a fragment, not the page chrome")
	}
	if !strings.Contains(string(body2), `hx-trigger="every 5s"`) {
		t.Error("fragment should be the polling table itself")
	}
	if !strings.Contains(string(body2), "node-1") {
		t.Error("fragment should render node rows")
	}

	// Boosted navigation (hx-boost on <body>): full page again.
	req3 := httptest.NewRequest("GET", "/admin/nodes", nil)
	req3.Header.Set("Cookie", cookieName+"="+cookie)
	req3.Header.Set("HX-Request", "true")
	req3.Header.Set("HX-Boosted", "true")
	resp3, err := srv.App().Test(req3)
	if err != nil {
		t.Fatalf("boosted nav: %v", err)
	}
	body3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if !strings.Contains(string(body3), "<!DOCTYPE html>") {
		t.Error("boosted navigation should still receive the full page")
	}
}

// TestKeysPageRequiresSession asserts the keys page sits behind the session
// guard like every other dashboard page.
func TestKeysPageRequiresSession(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/admin/keys", nil)
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

// TestKeysPageRendersRows checks the list shows existing keys with their
// status, and that the create form is present.
func TestKeysPageRendersRows(t *testing.T) {
	srv, _, adminKeyID := newTestServer(t)
	cookie := srv.issueSessionCookie(adminKeyID)
	req := httptest.NewRequest("GET", "/admin/keys", nil)
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
	if !strings.Contains(string(body), "admin-key") {
		t.Error("keys page should render the admin key row")
	}
	if !strings.Contains(string(body), `class="status active"`) {
		t.Error("keys page should mark the key active")
	}
	if !strings.Contains(string(body), `action="/admin/keys"`) {
		t.Error("keys page should include the create form")
	}
}

// TestKeysCreateFlow walks the full create path: POST mints a key, the nonce
// page reveals the raw key exactly once, the raw key never appears on the
// list page, and a replayed nonce link reveals nothing.
func TestKeysCreateFlow(t *testing.T) {
	srv, db, adminKeyID := newTestServer(t)
	cookie := srv.issueSessionCookie(adminKeyID)

	req := httptest.NewRequest("POST", "/admin/keys", strings.NewReader("name=smoke-ui-key"))
	req.Header.Set("Cookie", cookieName+"="+cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create: got %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/admin/keys/created?nonce=") {
		t.Fatalf("create redirect: got %q", loc)
	}

	// First view: the raw key, exactly once.
	req2 := httptest.NewRequest("GET", loc, nil)
	req2.Header.Set("Cookie", cookieName+"="+cookie)
	resp2, err := srv.App().Test(req2)
	if err != nil {
		t.Fatalf("created page: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("created page: got %d, want %d", resp2.StatusCode, http.StatusOK)
	}
	if cc := resp2.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("created page Cache-Control: got %q, want no-store", cc)
	}
	m := regexp.MustCompile(`sk-mesh-[A-Za-z0-9_-]+`).FindString(string(body2))
	if m == "" {
		t.Fatal("created page did not reveal the raw key")
	}
	if strings.Count(string(body2), m) != 1 {
		t.Error("created page should reveal the raw key exactly once")
	}

	// Replay: same URL reveals nothing.
	req3 := httptest.NewRequest("GET", loc, nil)
	req3.Header.Set("Cookie", cookieName+"="+cookie)
	resp3, err := srv.App().Test(req3)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusSeeOther || resp3.Header.Get("Location") != "/admin/keys" {
		t.Errorf("replay: got %d (location %q), want 303 to /admin/keys", resp3.StatusCode, resp3.Header.Get("Location"))
	}

	// The list page must never show the raw key, and the key verifies.
	req4 := httptest.NewRequest("GET", "/admin/keys", nil)
	req4.Header.Set("Cookie", cookieName+"="+cookie)
	resp4, err := srv.App().Test(req4)
	if err != nil {
		t.Fatalf("list after create: %v", err)
	}
	body4, _ := io.ReadAll(resp4.Body)
	resp4.Body.Close()
	if strings.Contains(string(body4), m) {
		t.Error("raw key must never appear on the list page")
	}
	if _, err := auth.NewAPIKeyRepo(db).Verify(context.Background(), m); err != nil {
		t.Errorf("created key should verify: %v", err)
	}
	// Audit event recorded.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM admin_audit_logs WHERE event = 'key_created'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("key_created audit rows: got %d, err %v; want 1", n, err)
	}
}

// TestKeysCreateEmptyName redirects back to the list with an error flash.
func TestKeysCreateEmptyName(t *testing.T) {
	srv, _, adminKeyID := newTestServer(t)
	cookie := srv.issueSessionCookie(adminKeyID)
	req := httptest.NewRequest("POST", "/admin/keys", strings.NewReader("name="))
	req.Header.Set("Cookie", cookieName+"="+cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/keys?error=name+is+required" {
		t.Errorf("location: got %q", loc)
	}
}

// TestKeysRevokeFlow checks that revoking from the UI takes effect
// immediately, records an audit event, and surfaces unknown ids safely.
func TestKeysRevokeFlow(t *testing.T) {
	srv, db, adminKeyID := newTestServer(t)
	cookie := srv.issueSessionCookie(adminKeyID)
	keys := auth.NewAPIKeyRepo(db)
	raw := auth.GenerateAPIKey()
	victim, err := keys.Create(context.Background(), "victim", raw)
	if err != nil {
		t.Fatalf("create victim: %v", err)
	}

	req := httptest.NewRequest("POST", "/admin/keys/"+victim.ID+"/revoke", nil)
	req.Header.Set("Cookie", cookieName+"="+cookie)
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/keys" {
		t.Errorf("revoke: got %d (location %q), want 303 to /admin/keys", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, err := keys.Verify(context.Background(), raw); err == nil {
		t.Error("revoked key should no longer verify")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM admin_audit_logs WHERE event = 'key_revoked'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("key_revoked audit rows: got %d, err %v; want 1", n, err)
	}

	// Unknown id: redirected back with an error flash, no audit row.
	req2 := httptest.NewRequest("POST", "/admin/keys/nope/revoke", nil)
	req2.Header.Set("Cookie", cookieName+"="+cookie)
	resp2, err := srv.App().Test(req2)
	if err != nil {
		t.Fatalf("revoke unknown: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("revoke unknown: got %d, want %d", resp2.StatusCode, http.StatusSeeOther)
	}
	if loc := resp2.Header.Get("Location"); loc != "/admin/keys?error=no+such+key" {
		t.Errorf("revoke unknown location: got %q", loc)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM admin_audit_logs WHERE event = 'key_revoked'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("key_revoked audit rows after unknown id: got %d, err %v; want 1", n, err)
	}
}

// TestKeysRevokeSelfRejected guards against accidentally revoking the key of
// the current session from the UI.
func TestKeysRevokeSelfRejected(t *testing.T) {
	srv, db, adminKeyID := newTestServer(t)
	cookie := srv.issueSessionCookie(adminKeyID)
	req := httptest.NewRequest("POST", "/admin/keys/"+adminKeyID+"/revoke", nil)
	req.Header.Set("Cookie", cookieName+"="+cookie)
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("self revoke: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/keys?error=cannot+revoke+the+key+of+your+own+session" {
		t.Errorf("location: got %q", loc)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM api_keys WHERE id = ?`, adminKeyID).Scan(&status); err != nil || status != "active" {
		t.Errorf("own key status: got %q, err %v; want active", status, err)
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

// TestPagesServeTextHTML guards against the Fiber v3 Type() footgun: Type()
// takes a file extension ("html"), not a MIME type ("text/html"), and an
// unknown extension falls back to application/octet-stream, which makes
// browsers download the page instead of rendering it.
func TestPagesServeTextHTML(t *testing.T) {
	srv, _, adminKeyID := newTestServer(t)
	cookie := srv.issueSessionCookie(adminKeyID)

	cases := []struct {
		name        string
		path        string
		withSession bool
	}{
		{"login", "/admin/login", false},
		{"home", "/admin", true},
		{"nodes", "/admin/nodes", true},
		{"node detail", "/admin/nodes/node-1", true},
		{"audit", "/admin/audit", true},
		{"worker keys", "/admin/worker-keys", true},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("GET", tc.path, nil)
		if tc.withSession {
			req.Header.Set("Cookie", cookieName+"="+cookie)
		}
		resp, err := srv.App().Test(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s page Content-Type: got %q, want text/html (browsers will download the page)", tc.name, ct)
		}
	}
}

// TestWorkerKeysPageRequiresSession asserts the worker-keys page sits behind
// the session guard like every other dashboard page.
func TestWorkerKeysPageRequiresSession(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/admin/worker-keys", nil)
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

// TestWorkerKeysCreateFlow walks the full enrolment mint path: POST creates a
// token row, the nonce page reveals the raw PASETO exactly once alongside a
// paste-ready config snippet, a replayed link reveals nothing, the raw token
// never appears on the list page.
func TestWorkerKeysCreateFlow(t *testing.T) {
	srv, db, adminKeyID := newTestServer(t)
	cookie := srv.issueSessionCookie(adminKeyID)

	req := httptest.NewRequest("POST", "/admin/worker-keys", strings.NewReader("node=gpu-loopback&ttl=24h"))
	req.Header.Set("Cookie", cookieName+"="+cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create: got %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/admin/worker-keys/created?node=gpu-loopback&nonce=") {
		t.Fatalf("create redirect: got %q", loc)
	}

	// First view: the raw token and the config snippet, exactly once.
	req2 := httptest.NewRequest("GET", loc, nil)
	req2.Header.Set("Cookie", cookieName+"="+cookie)
	resp2, err := srv.App().Test(req2)
	if err != nil {
		t.Fatalf("created page: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("created page: got %d, want %d", resp2.StatusCode, http.StatusOK)
	}
	if cc := resp2.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("created page Cache-Control: got %q, want no-store", cc)
	}
	m := regexp.MustCompile(`v4\.public\.[A-Za-z0-9_-]+`).FindString(string(body2))
	if m == "" {
		t.Fatal("created page did not reveal the raw enrolment token")
	}
	if got := strings.Count(string(body2), m); got != 2 {
		t.Errorf("raw token should appear exactly twice (reveal + snippet), got %d", got)
	}
	if !strings.Contains(string(body2), `node_id = "gpu-loopback"`) {
		t.Error("created page should show a config snippet with the node id")
	}
	if !strings.Contains(string(body2), `enrollment_token = "`) {
		t.Error("created page should show a config snippet with the token slot")
	}

	// Replay: same URL reveals nothing.
	req3 := httptest.NewRequest("GET", loc, nil)
	req3.Header.Set("Cookie", cookieName+"="+cookie)
	resp3, err := srv.App().Test(req3)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusSeeOther || resp3.Header.Get("Location") != "/admin/worker-keys" {
		t.Errorf("replay: got %d (location %q), want 303 to /admin/worker-keys", resp3.StatusCode, resp3.Header.Get("Location"))
	}

	// The list page shows the token row but never the raw PASETO.
	req4 := httptest.NewRequest("GET", "/admin/worker-keys", nil)
	req4.Header.Set("Cookie", cookieName+"="+cookie)
	resp4, err := srv.App().Test(req4)
	if err != nil {
		t.Fatalf("list after create: %v", err)
	}
	body4, _ := io.ReadAll(resp4.Body)
	resp4.Body.Close()
	if !strings.Contains(string(body4), "gpu-loopback") {
		t.Error("worker keys page should render the token row")
	}
	if strings.Contains(string(body4), m) {
		t.Error("raw enrolment token must never appear on the list page")
	}
	var etID string
	if err := db.QueryRow(`SELECT id FROM node_enrollment_tokens WHERE node_name = 'gpu-loopback'`).Scan(&etID); err != nil {
		t.Fatalf("query enrollment row: %v", err)
	}
	// Audit event recorded with the token id + node name as detail.
	var detail string
	if err := db.QueryRow(`SELECT detail FROM admin_audit_logs WHERE event = 'enrollment_created'`).Scan(&detail); err != nil || !strings.Contains(detail, etID) || !strings.Contains(detail, "gpu-loopback") {
		t.Errorf("enrollment_created audit rows: got detail %q, err %v; want id %s + node name", detail, err, etID)
	}
}

// TestWorkerKeysCreateDefaultTTL checks that submitting the form with no ttl
// value (the browser's default selection) yields a token whose expiry is the
// far-future stand-in for "no expiry".
func TestWorkerKeysCreateDefaultTTL(t *testing.T) {
	srv, db, adminKeyID := newTestServer(t)
	cookie := srv.issueSessionCookie(adminKeyID)

	req := httptest.NewRequest("POST", "/admin/worker-keys", strings.NewReader("node=gpu-default"))
	req.Header.Set("Cookie", cookieName+"="+cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create: got %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	var expires time.Time
	if err := db.QueryRow(`SELECT expires_at FROM node_enrollment_tokens WHERE node_name = 'gpu-default'`).Scan(&expires); err != nil {
		t.Fatalf("query enrollment row: %v", err)
	}
	if want := time.Now().AddDate(99, 0, 0); expires.Before(want) {
		t.Errorf("default ttl: expires_at %v should be the far-future no-expiry stand-in (>= %v)", expires, want)
	}
}

// TestWorkerKeysCreateValidation covers the two error-flash paths: a missing
// node name and a ttl value outside the whitelist.
func TestWorkerKeysCreateValidation(t *testing.T) {
	srv, db, adminKeyID := newTestServer(t)
	cookie := srv.issueSessionCookie(adminKeyID)
	// Baseline token so the row-count assertion proves rejections minted
	// nothing new rather than assuming an empty table (rows from other tests
	// sharing the DB would otherwise flake the count).
	var before int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_enrollment_tokens`).Scan(&before); err != nil {
		t.Fatalf("baseline count: %v", err)
	}

	req := httptest.NewRequest("POST", "/admin/worker-keys", strings.NewReader("node=&ttl=24h"))
	req.Header.Set("Cookie", cookieName+"="+cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("empty node: %v", err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/admin/worker-keys?error=node+name+is+required" {
		t.Errorf("empty node location: got %q", loc)
	}

	req2 := httptest.NewRequest("POST", "/admin/worker-keys", strings.NewReader("node=gpu-x&ttl=forever"))
	req2.Header.Set("Cookie", cookieName+"="+cookie)
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp2, err := srv.App().Test(req2)
	if err != nil {
		t.Fatalf("bad ttl: %v", err)
	}
	resp2.Body.Close()
	if loc := resp2.Header.Get("Location"); loc != "/admin/worker-keys?error=unknown+expiry+choice" {
		t.Errorf("bad ttl location: got %q", loc)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_enrollment_tokens`).Scan(&n); err != nil || n != before {
		t.Errorf("rejected creates must not mint rows: got %d rows (baseline %d), err %v", n, before, err)
	}
}

// TestWorkerKeysRevokeFlow checks that revoking from the UI flips the token
// inactive (so the router drops the worker on its next heartbeat), records
// an audit event with the token id, and surfaces unknown ids safely.
func TestWorkerKeysRevokeFlow(t *testing.T) {
	srv, db, adminKeyID := newTestServer(t)
	cookie := srv.issueSessionCookie(adminKeyID)
	signer := auth.NewRandomSigner()
	repo := auth.NewEnrollmentRepo(db, signer)
	et, _, err := repo.Create(context.Background(), "gpu-victim", 0)
	if err != nil {
		t.Fatalf("create victim: %v", err)
	}
	if ok, err := repo.IsActive(context.Background(), et.ID); err != nil || !ok {
		t.Fatalf("precondition: fresh token active: %v, %v", ok, err)
	}

	req := httptest.NewRequest("POST", "/admin/worker-keys/"+et.ID+"/revoke", nil)
	req.Header.Set("Cookie", cookieName+"="+cookie)
	resp, err := srv.App().Test(req)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/worker-keys" {
		t.Errorf("revoke: got %d (location %q), want 303 to /admin/worker-keys", resp.StatusCode, resp.Header.Get("Location"))
	}
	if ok, err := repo.IsActive(context.Background(), et.ID); err != nil || ok {
		t.Errorf("revoked token IsActive: got %v, %v; want false, nil", ok, err)
	}
	var detail string
	if err := db.QueryRow(`SELECT detail FROM admin_audit_logs WHERE event = 'enrollment_revoked'`).Scan(&detail); err != nil || detail != et.ID {
		t.Errorf("enrollment_revoked audit rows: got detail %q, err %v; want %q", detail, err, et.ID)
	}

	// Unknown id: redirected back with an error flash, no audit row.
	req2 := httptest.NewRequest("POST", "/admin/worker-keys/nope/revoke", nil)
	req2.Header.Set("Cookie", cookieName+"="+cookie)
	resp2, err := srv.App().Test(req2)
	if err != nil {
		t.Fatalf("revoke unknown: %v", err)
	}
	resp2.Body.Close()
	if loc := resp2.Header.Get("Location"); loc != "/admin/worker-keys?error=no+such+token" {
		t.Errorf("revoke unknown location: got %q", loc)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM admin_audit_logs WHERE event = 'enrollment_revoked'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("enrollment_revoked audit rows after unknown id: got %d, err %v; want 1", n, err)
	}
}

// keep fiber referenced for the cookie SameSite constant used above.
var _ = fiber.CookieSameSiteLaxMode
