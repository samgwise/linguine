package admin

import (
	"embed"
	"net/http"

	"github.com/gofiber/fiber/v3"
)

// htmx is vendored rather than loaded from a CDN so the dashboard never
// executes third-party-served script in the admin's session. The file is
// embedded into the binary so the router stays a single self-contained
// artefact.
//
//go:embed static/htmx.min.js
var staticFS embed.FS

// staticHtmx serves the vendored htmx bundle. It is registered outside the
// session middleware so an unauthenticated browser can load it for the login
// page. A long immutable cache is safe: the path is version-pinned.
func (s *Server) staticHtmx(c fiber.Ctx) error {
	data, err := staticFS.ReadFile("static/htmx.min.js")
	if err != nil {
		return c.Status(http.StatusNotFound).SendString("not found")
	}
	c.Set(fiber.HeaderContentType, "application/javascript; charset=utf-8")
	c.Set(fiber.HeaderCacheControl, "public, max-age=31536000, immutable")
	return c.Send(data)
}
