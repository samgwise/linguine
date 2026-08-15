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
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/samgw/linguine/internal/audit"
	"github.com/samgw/linguine/internal/auth"
	"github.com/samgw/linguine/internal/fleet"
)

const (
	cookieName    = "linguine_admin"
	sessionTTL    = 12 * time.Hour
	auditLimit    = 50
	loginLimit    = 5             // max login attempts per IP per window
	loginWindow   = 1 * time.Minute // throttle window
)

// Deps holds the admin dashboard's external dependencies.
type Deps struct {
	Keys          *auth.APIKeyRepo
	Audit         *audit.Repo
	Nodes         func() []fleet.NodeView
	Listen        string
	SessionSecret []byte // HMAC key for the session cookie
}

// Server is the admin dashboard.
type Server struct {
	keys          *auth.APIKeyRepo
	audit         *audit.Repo
	nodes         func() []fleet.NodeView
	app           *fiber.App
	listen        string
	sessionSecret []byte
	limiter       *loginLimiter
}

// New constructs an admin dashboard server. Call Start to run it.
func New(deps Deps) *Server {
	s := &Server{
		keys:          deps.Keys,
		audit:         deps.Audit,
		nodes:         deps.Nodes,
		listen:        deps.Listen,
		sessionSecret: deps.SessionSecret,
		app:           fiber.New(),
		limiter:       newLoginLimiter(loginLimit, loginWindow),
	}
	s.registerRoutes()
	return s
}

// App returns the underlying Fiber app (for in-process testing).
func (s *Server) App() *fiber.App { return s.app }

func (s *Server) registerRoutes() {
	// Static assets sit outside the session guard so the login page can load
	// htmx before authentication.
	s.app.Get("/admin/static/htmx.min.js", s.staticHtmx)
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
}

// cspHeader sets a strict Content-Security-Policy on admin pages. htmx is
// served from 'self' (vendored), inline styles are allowed because the
// templates embed their CSS (vendoring the stylesheet to drop this exception
// is deferred to a later phase), and connect/form targets are constrained to
// 'self' to block cross-origin exfiltration and form submits.
func (s *Server) cspHeader(c fiber.Ctx) error {
	c.Set(fiber.HeaderContentSecurityPolicy,
		"default-src 'self'; script-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; form-action 'self'")
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
	return c.Type("text/html").SendString(loginPage())
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

func (s *Server) home(c fiber.Ctx) error {
	nodes := s.nodes()
	online := 0
	for _, n := range nodes {
		if n.Status == "online" {
			online++
		}
	}
	return c.Type("text/html").SendString(homePage(nodes, online))
}

func (s *Server) nodesPage(c fiber.Ctx) error {
	return c.Type("text/html").SendString(nodesPage(s.nodes()))
}

func (s *Server) nodeDetailPage(c fiber.Ctx) error {
	id := c.Params("id")
	for _, n := range s.nodes() {
		if n.ID == id {
			return c.Type("text/html").SendString(nodeDetailPage(n))
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
	return c.Type("text/html").SendString(auditPage(entries, adminEvents))
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

// extractBearer pulls the token out of a "Bearer <token>" header value.
func extractBearer(authz string) string {
	const prefix = "Bearer "
	if len(authz) > len(prefix) && strings.EqualFold(authz[:len(prefix)], prefix) {
		return authz[len(prefix):]
	}
	return ""
}
