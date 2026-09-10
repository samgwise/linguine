package admin

import (
	"embed"
	"net/http"

	"github.com/gofiber/fiber/v3"
)

// htmx and copy.js are vendored rather than loaded from a CDN so the
// dashboard never executes third-party-served script in the admin's session.
// The files are embedded into the binary so the router stays a single
// self-contained artefact.
//
//go:embed static/htmx.min.js static/copy.js
var staticFS embed.FS

// staticJS serves a vendored script from internal/admin/static. Both routes
// are registered outside the session middleware so an unauthenticated browser
// can load htmx for the login page. htmx is version-pinned and cached
// immutably; copy.js carries no version, so browsers revalidate it instead.
func (s *Server) staticJS(name string, immutable bool) func(fiber.Ctx) error {
	path := "static/" + name
	return func(c fiber.Ctx) error {
		data, err := staticFS.ReadFile(path)
		if err != nil {
			return c.Status(http.StatusNotFound).SendString("not found")
		}
		c.Set(fiber.HeaderContentType, "application/javascript; charset=utf-8")
		if immutable {
			c.Set(fiber.HeaderCacheControl, "public, max-age=31536000, immutable")
		} else {
			c.Set(fiber.HeaderCacheControl, "no-cache")
		}
		return c.Send(data)
	}
}
