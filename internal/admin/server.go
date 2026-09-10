// Package admin implements the HTMX admin dashboard: a separate Fiber app on
// a localhost-only listener (default 127.0.0.1:8444) for an operator's reverse
// proxy to terminate TLS in front of. Auth is a signed session cookie issued
// after an admin-role API key is presented at /admin/login.
//
// Templates use the stdlib html/template rather than templ so the build stays
// pure-Go with no external code generator required.
package admin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/samgw/linguine/internal/audit"
	"github.com/samgw/linguine/internal/auth"
	"github.com/samgw/linguine/internal/fleet"
)

// workerKeyTTLChoice is one enrolment-lifetime option on the worker-keys form.
type workerKeyTTLChoice struct {
	Value string        // form value
	Label string        // <option> text
	TTL   time.Duration // 0 = no expiry
}

// workerKeyTTLs are the enrolment-lifetime choices offered on the worker-keys
// form. A token lives exactly as long as the operator chose — the default is
// no expiry, matching the coordinator-invite workflow; the short options are
// there for one-off or fleet-wide batches.
var workerKeyTTLs = []workerKeyTTLChoice{
	{"24h", "1 day", 24 * time.Hour},
	{"72h", "3 days", 72 * time.Hour},
	{"168h", "7 days", 168 * time.Hour},
	{"720h", "1 month", 720 * time.Hour},
	{"2160h", "3 months", 2160 * time.Hour},
	{"none", "No expiry", 0},
}

const (
	cookieName  = "linguine_admin"
	sessionTTL  = 12 * time.Hour
	auditLimit  = 50
	loginLimit  = 5               // max login attempts per IP per window
	loginWindow = 1 * time.Minute // throttle window
)

// Deps holds the admin dashboard's external dependencies.
type Deps struct {
	Keys  *auth.APIKeyRepo
	Audit *audit.Repo
	// Enrollments, when set, enables the worker-keys page for minting and
	// revoking worker enrolment tokens from the dashboard.
	Enrollments *auth.EnrollmentRepo
	Nodes       func() []fleet.NodeView
	// Claims, when set, returns rejected connection attempts (workers
	// knocking with bad or revoked enrollment) to merge into node listings.
	Claims        func() []fleet.NodeView
	Listen        string
	SessionSecret []byte // HMAC key for the session cookie
}

// Server is the admin dashboard.
type Server struct {
	keys          *auth.APIKeyRepo
	enrollments   *auth.EnrollmentRepo
	audit         *audit.Repo
	nodes         func() []fleet.NodeView
	claims        func() []fleet.NodeView
	app           *fiber.App
	listen        string
	sessionSecret []byte
	limiter       *loginLimiter
	nonces        *nonceStore
}

// New constructs an admin dashboard server. Call Start to run it.
func New(deps Deps) *Server {
	s := &Server{
		keys:          deps.Keys,
		enrollments:   deps.Enrollments,
		audit:         deps.Audit,
		nodes:         deps.Nodes,
		claims:        deps.Claims,
		listen:        deps.Listen,
		sessionSecret: deps.SessionSecret,
		app:           fiber.New(),
		limiter:       newLoginLimiter(loginLimit, loginWindow),
		nonces:        newNonceStore(),
	}
	s.registerRoutes()
	return s
}

// App returns the underlying Fiber app (for in-process testing).
func (s *Server) App() *fiber.App { return s.app }

func (s *Server) registerRoutes() {
	// Static assets sit outside the session guard so the login page can load
	// htmx before authentication.
	s.app.Get("/admin/static/htmx.min.js", s.staticJS("htmx.min.js", true))
	s.app.Get("/admin/static/copy.js", s.staticJS("copy.js", false))
	// The Content-Security-Policy header is applied to every admin response,
	// including the login page, so script execution is constrained regardless
	// of session state.
	s.app.Use("/admin", s.cspHeader)
	s.app.Get("/admin/login", s.loginForm)
	s.app.Post("/admin/login", s.loginSubmit)
	s.app.Post("/admin/logout", s.logout)
	s.app.Use("/admin", s.requireSession)
	s.app.Get("/admin", s.home)
	s.app.Get("/admin/nodes", s.nodesPage)
	s.app.Get("/admin/nodes/:id", s.nodeDetailPage)
	s.app.Get("/admin/audit", s.auditPage)
	s.app.Get("/admin/keys", s.keysPage)
	s.app.Post("/admin/keys", s.keysCreate)
	s.app.Get("/admin/keys/created", s.keyCreatedPage)
	s.app.Post("/admin/keys/:id/revoke", s.keyRevoke)
	s.app.Get("/admin/worker-keys", s.workerKeysPage)
	s.app.Post("/admin/worker-keys", s.workerKeysCreate)
	s.app.Get("/admin/worker-keys/created", s.workerKeyCreatedPage)
	s.app.Post("/admin/worker-keys/:id/revoke", s.workerKeyRevoke)
}

// cspHeader sets a strict Content-Security-Policy on admin pages. htmx is
// served from 'self' (vendored), inline styles are allowed because the
// templates embed their CSS (vendoring the stylesheet to drop this exception
// is deferred to a later phase), and connect/form targets are constrained to
// 'self' to block cross-origin exfiltration and form submits.
func (s *Server) cspHeader(c fiber.Ctx) error {
	c.Set(fiber.HeaderContentSecurityPolicy,
		"default-src 'self'; script-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; form-action 'self'")
	// The key-created page carries a single-use nonce in its URL; keep that
	// URL out of Referer headers sent off-site.
	c.Set(fiber.HeaderReferrerPolicy, "same-origin")
	return c.Next()
}

// Start runs the admin HTTP server on a new listener. Non-blocking.
func (s *Server) Start() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return nil, fmt.Errorf("admin: listen: %w", err)
	}
	go func() {
		if err := s.app.Listener(ln); err != nil {
			fmt.Printf("[admin] http serve: %v\n", err)
		}
	}()
	return ln, nil
}

// Shutdown stops the admin HTTP server.
func (s *Server) Shutdown() error {
	return s.app.Shutdown()
}

// requireSession rejects requests without a valid admin session cookie, and
// re-verifies that the cookie's admin key is still active so revoking or
// expiring a key kills its outstanding sessions immediately rather than
// waiting for the cookie's own expiry.
func (s *Server) requireSession(c fiber.Ctx) error {
	cookie := c.Cookies(cookieName)
	akID, ok := s.verifySessionCookie(cookie)
	if !ok {
		return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/login")
	}
	active, err := s.keys.ActiveByID(c.Context(), akID)
	if err != nil || !active {
		return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/login")
	}
	c.Locals("adminKeyID", akID)
	return c.Next()
}

func (s *Server) loginForm(c fiber.Ctx) error {
	// NB: Fiber v3's Type() takes a file extension, not a MIME type; passing
	// "text/html" falls through to application/octet-stream and browsers
	// download the page instead of rendering it.
	return c.Type("html").SendString(loginPage())
}

func (s *Server) loginSubmit(c fiber.Ctx) error {
	ip := c.IP()
	if !s.limiter.Allow(ip) {
		_ = s.audit.RecordAdminEvent(audit.AdminEvent{
			Event:      "login_throttled",
			RemoteIP:   ip,
			StatusCode: fiber.StatusTooManyRequests,
		})
		return c.Status(fiber.StatusTooManyRequests).SendString("too many login attempts")
	}
	token := strings.TrimSpace(c.FormValue("password"))
	if token == "" {
		_ = s.audit.RecordAdminEvent(audit.AdminEvent{
			Event:      "login_failed",
			RemoteIP:   ip,
			StatusCode: fiber.StatusUnauthorized,
		})
		return c.Status(fiber.StatusUnauthorized).SendString("missing api key")
	}
	ak, err := s.keys.Verify(c.Context(), token)
	if err != nil || ak == nil || ak.Role != "admin" {
		var keyID string
		if ak != nil {
			keyID = ak.ID
		}
		_ = s.audit.RecordAdminEvent(audit.AdminEvent{
			Event:      "login_failed",
			APIKeyID:   keyID,
			RemoteIP:   ip,
			StatusCode: fiber.StatusUnauthorized,
		})
		return c.Status(fiber.StatusUnauthorized).SendString("invalid admin key")
	}
	_ = s.audit.RecordAdminEvent(audit.AdminEvent{
		Event:      "login_ok",
		APIKeyID:   ak.ID,
		RemoteIP:   ip,
		StatusCode: fiber.StatusSeeOther,
	})
	cookie := s.issueSessionCookie(ak.ID)
	c.Cookie(&fiber.Cookie{
		Name:     cookieName,
		Value:    cookie,
		Path:     "/admin",
		HTTPOnly: true,
		Secure:   true,
		SameSite: fiber.CookieSameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return c.Redirect().Status(fiber.StatusSeeOther).To("/admin")
}

func (s *Server) logout(c fiber.Ctx) error {
	c.Cookie(&fiber.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/admin",
		HTTPOnly: true,
		Secure:   true,
		SameSite: fiber.CookieSameSiteLaxMode,
		MaxAge:   -1,
	})
	return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/login")
}

// fleetView merges registered nodes with unauthenticated connection claims
// (nodes that are visible but nil-registered, e.g. bad or revoked token),
// giving operators one table that shows every machine trying to join. When
// no Claims func is wired (e.g. tests), only registered nodes appear.
func (s *Server) fleetView() []fleet.NodeView {
	nodes := s.nodes()
	if s.claims == nil {
		return nodes
	}
	registered := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		registered[n.ID] = true
	}
	for _, c := range s.claims() {
		if !registered[c.ID] {
			nodes = append(nodes, c)
		}
	}
	return nodes
}

func (s *Server) home(c fiber.Ctx) error {
	nodes := s.fleetView()
	online := 0
	for _, n := range nodes {
		if n.Status == "online" {
			online++
		}
	}
	return c.Type("html").SendString(homePage(nodes, online))
}

// isFragmentRequest reports whether the request comes from an htmx poll
// expecting a content fragment rather than a full page. Boosted navigation
// clicks also send HX-Request, but they set HX-Boosted and still need the
// full-page chrome to swap the document.
func isFragmentRequest(c fiber.Ctx) bool {
	return c.Get("HX-Request") == "true" && c.Get("HX-Boosted") != "true"
}

func (s *Server) nodesPage(c fiber.Ctx) error {
	if isFragmentRequest(c) {
		// The table polls itself every 5s with hx-swap="outerHTML"; returning
		// the full page here would nest page chrome inside the table on every
		// refresh.
		return c.Type("html").SendString(nodesFragment(s.fleetView()))
	}
	return c.Type("html").SendString(nodesPage(s.fleetView()))
}

func (s *Server) nodeDetailPage(c fiber.Ctx) error {
	id := c.Params("id")
	for _, n := range s.fleetView() {
		if n.ID == id {
			return c.Type("html").SendString(nodeDetailPage(n))
		}
	}
	return c.Status(fiber.StatusNotFound).SendString("node not found")
}

func (s *Server) auditPage(c fiber.Ctx) error {
	entries, err := s.audit.Recent(c.Context(), auditLimit, "")
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("audit query failed")
	}
	adminEvents, err := s.audit.RecentAdminEvents(c.Context(), auditLimit)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("admin audit query failed")
	}
	return c.Type("html").SendString(auditPage(entries, adminEvents))
}

// keysPage renders the API key management page: a create form above a table
// of every key (all roles and statuses). The list is read-only for raw
// material — raw keys are shown exactly once, on the single-use created page.
func (s *Server) keysPage(c fiber.Ctx) error {
	keys, err := s.keys.List(c.Context())
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("key list query failed")
	}
	return c.Type("html").SendString(keysPage(keys, c.Query("error", "")))
}

// keysCreate mints a client API key, records the audit event, and redirects
// to the single-use created page that shows the raw key once.
func (s *Server) keysCreate(c fiber.Ctx) error {
	name := strings.TrimSpace(c.FormValue("name"))
	if name == "" {
		return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/keys?error=" + url.QueryEscape("name is required"))
	}
	raw := auth.GenerateAPIKey()
	ak, err := s.keys.Create(c.Context(), name, raw)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("key create failed")
	}
	_ = s.audit.RecordAdminEvent(audit.AdminEvent{
		Event:      "key_created",
		APIKeyID:   ak.ID,
		RemoteIP:   c.IP(),
		StatusCode: fiber.StatusOK,
	})
	nonce, err := s.nonces.put(raw)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("key create failed")
	}
	return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/keys/created?nonce=" + url.QueryEscape(nonce))
}

// keyCreatedPage shows the raw key exactly once: take() deletes the nonce so
// refreshes and replays redirect back to the list. no-store keeps the key out
// of browser and proxy caches.
func (s *Server) keyCreatedPage(c fiber.Ctx) error {
	raw, ok := s.nonces.take(c.Query("nonce"))
	if !ok {
		return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/keys")
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Type("html").SendString(keyCreatedPage(raw))
}

// keyRevoke flips a key to revoked immediately. The key that issued the
// current session cannot revoke itself here — accidental self-lockout is
// too easy (use the CLI if you really mean it).
func (s *Server) keyRevoke(c fiber.Ctx) error {
	id := c.Params("id")
	if sessionID, _ := c.Locals("adminKeyID").(string); id == sessionID {
		return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/keys?error=" + url.QueryEscape("cannot revoke the key of your own session"))
	}
	if err := s.keys.Revoke(c.Context(), id); err != nil {
		return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/keys?error=" + url.QueryEscape("no such key"))
	}
	_ = s.audit.RecordAdminEvent(audit.AdminEvent{
		Event:      "key_revoked",
		APIKeyID:   id,
		RemoteIP:   c.IP(),
		StatusCode: fiber.StatusOK,
	})
	return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/keys")
}

// workerKeysPage renders the worker enrolment token management page: a mint
// form above a table of every token. Raw tokens are shown exactly once, on the
// single-use created page, exactly like API keys. The Enrollments dep is
// always wired in production; a nil repo (unit tests) renders an empty list.
func (s *Server) workerKeysPage(c fiber.Ctx) error {
	var toks []auth.EnrollmentToken
	if s.enrollments != nil {
		var err error
		toks, err = s.enrollments.List(c.Context())
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString("enrollment token list query failed")
		}
	}
	return c.Type("html").SendString(workerKeysPage(toks, c.Query("error", "")))
}

// workerKeysCreate mints a worker enrolment token, records the audit event,
// and redirects to the single-use created page showing the raw PASETO once.
// The TTL comes from a whitelist of form choices; anything else is rejected
// with an error flash rather than silently reinterpreted.
func (s *Server) workerKeysCreate(c fiber.Ctx) error {
	node := strings.TrimSpace(c.FormValue("node"))
	if node == "" {
		return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/worker-keys?error=" + url.QueryEscape("node name is required"))
	}
	ttl, ok := workerKeyTTL(c.FormValue("ttl"))
	if !ok {
		return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/worker-keys?error=" + url.QueryEscape("unknown expiry choice"))
	}
	et, raw, err := s.enrollments.Create(c.Context(), node, ttl)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("enrollment token create failed")
	}
	// NB: the token id goes in Detail, not APIKeyID — that column is a real
	// foreign key to api_keys, which an enrolment token id is not.
	_ = s.audit.RecordAdminEvent(audit.AdminEvent{
		Event:      "enrollment_created",
		Detail:     et.ID + " (" + et.NodeName + ")",
		RemoteIP:   c.IP(),
		StatusCode: fiber.StatusOK,
	})
	nonce, err := s.nonces.put(raw)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("enrollment token create failed")
	}
	return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/worker-keys/created?node=" + url.QueryEscape(et.NodeName) + "&nonce=" + url.QueryEscape(nonce))
}

// workerKeyCreatedPage shows the raw enrolment token exactly once, with a
// paste-ready worker config snippet. Same mechanics as the API-key reveal:
// take() deletes the nonce so refreshes and replays redirect back to the list,
// and no-store keeps the token out of browser and proxy caches.
func (s *Server) workerKeyCreatedPage(c fiber.Ctx) error {
	raw, ok := s.nonces.take(c.Query("nonce"))
	if !ok {
		return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/worker-keys")
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Type("html").SendString(workerKeyCreatedPage(c.Query("node"), raw))
}

// workerKeyRevoke flips an enrolment token to revoked. The router's
// per-heartbeat IsActive check drops any connected worker within one
// heartbeat interval.
func (s *Server) workerKeyRevoke(c fiber.Ctx) error {
	id := c.Params("id")
	if err := s.enrollments.Revoke(c.Context(), id); err != nil {
		return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/worker-keys?error=" + url.QueryEscape("no such token"))
	}
	_ = s.audit.RecordAdminEvent(audit.AdminEvent{
		Event:      "enrollment_revoked",
		Detail:     id,
		RemoteIP:   c.IP(),
		StatusCode: fiber.StatusOK,
	})
	return c.Redirect().Status(fiber.StatusSeeOther).To("/admin/worker-keys")
}

// workerKeyTTL resolves a form choice to its lifetime. An empty value selects
// the default (no expiry). ok is false for anything outside the whitelist.
func workerKeyTTL(v string) (time.Duration, bool) {
	if v == "" {
		v = "none"
	}
	for _, t := range workerKeyTTLs {
		if t.Value == v {
			return t.TTL, true
		}
	}
	return 0, false
}

// issueSessionCookie returns `keyID|expiresUnix|hmac` for the given admin
// key id. The HMAC is keyed by SessionSecret and covers keyID and expiry.
func (s *Server) issueSessionCookie(keyID string) string {
	expires := time.Now().Add(sessionTTL).Unix()
	payload := keyID + "|" + strconv.FormatInt(expires, 10)
	mac := hmac.New(sha256.New, s.sessionSecret)
	mac.Write([]byte(payload))
	return payload + "|" + hex.EncodeToString(mac.Sum(nil))
}

// verifySessionCookie validates the cookie's HMAC and expiry, returning the
// admin key id it was issued to.
func (s *Server) verifySessionCookie(cookie string) (string, bool) {
	parts := strings.Split(cookie, "|")
	if len(parts) != 3 {
		return "", false
	}
	keyID, expiresStr, gotMAC := parts[0], parts[1], parts[2]
	expires, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil || time.Now().Unix() > expires {
		return "", false
	}
	payload := keyID + "|" + expiresStr
	mac := hmac.New(sha256.New, s.sessionSecret)
	mac.Write([]byte(payload))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(gotMAC), []byte(want)) {
		return "", false
	}
	return keyID, true
}
