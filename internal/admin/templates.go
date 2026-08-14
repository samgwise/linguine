package admin

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/samgw/linguine/internal/audit"
	"github.com/samgw/linguine/internal/fleet"
)

// tmpl is the parsed, embedded HTML template set. Pages are rendered with
// html/template (stdlib) so no external code generator is needed.
var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"fmtTime":  func(t time.Time) string { return t.Format(time.RFC3339) },
	"join":    func(xs []string) string { return strings.Join(xs, ", ") },
	"lower":   strings.ToLower,
}).Parse(`{{define "base"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>{{.Title}} — linguine admin</title>
<script src="https://cdn.jsdelivr.net/npm/htmx.org@2.0.8/dist/htmx.min.js"></script>
<style>
body { font: 14px/1.5 -apple-system, "Segoe UI", Roboto, sans-serif; margin: 0; color: #1f2328; background: #f6f8fa; }
header { background: #24292f; color: #fff; padding: 12px 24px; display: flex; gap: 20px; align-items: center; }
header a { color: #8b949e; text-decoration: none; }
header a:hover { color: #fff; }
header .brand { font-weight: 600; margin-right: auto; }
main { max-width: 1100px; margin: 0 auto; padding: 24px; }
h1 { margin: 0 0 16px; font-size: 22px; }
h2 { margin: 0 0 12px; font-size: 18px; }
table { border-collapse: collapse; width: 100%; background: #fff; box-shadow: 0 1px 3px rgba(0,0,0,.08); border-radius: 8px; overflow: hidden; }
th, td { padding: 10px 14px; text-align: left; border-bottom: 1px solid #d0d7de; }
th { background: #f6f8fa; font-size: 12px; text-transform: uppercase; letter-spacing: .04em; color: #57606a; }
td { font-variant-numeric: tabular-nums; }
.status { padding: 2px 8px; border-radius: 999px; font-size: 12px; font-weight: 600; }
.status.online { background: #dafbe1; color: #1a7f37; }
.status.stale { background: #fff8c5; color: #9a6700; }
.status.offline { background: #ffebe9; color: #cf222e; }
.card { background: #fff; border: 1px solid #d0d7de; border-radius: 8px; padding: 20px; margin-bottom: 20px; }
.metrics { display: flex; gap: 20px; }
.metric { background: #fff; border: 1px solid #d0d7de; border-radius: 8px; padding: 16px 20px; flex: 1; }
.metric .v { font-size: 28px; font-weight: 700; }
.metric .l { color: #57606a; font-size: 12px; text-transform: uppercase; letter-spacing: .04em; }
form { display: flex; flex-direction: column; gap: 12px; max-width: 320px; }
input { padding: 8px 10px; border: 1px solid #d0d7de; border-radius: 6px; font: inherit; }
button { padding: 8px 16px; border: 0; border-radius: 6px; background: #1f6feb; color: #fff; font: inherit; font-weight: 600; cursor: pointer; }
.muted { color: #57606a; }
a { color: #0969da; }
</style>
</head>
<body hx-boost="true">
<header>
<span class="brand">linguine</span>
<a href="/admin">Dashboard</a>
<a href="/admin/nodes">Nodes</a>
<a href="/admin/audit">Audit log</a>
<form method="post" action="/admin/logout" style="display:inline"><button type="submit" style="background:#636c76">Sign out</button></form>
</header>
<main>
{{.Body}}
</main>
</body>
</html>{{end}}`))

func renderPage(title, body string) string {
	var b strings.Builder
	if err := tmpl.ExecuteTemplate(&b, "base", map[string]any{"Title": title, "Body": template.HTML(body)}); err != nil {
		return "admin: render error: " + err.Error()
	}
	return b.String()
}

func loginPage() string {
	body := `<h1>Sign in</h1>
<form method="post" action="/admin/login">
<label>Admin API key <input type="password" name="password" placeholder="sk-mesh-…" autofocus/></label>
<button type="submit">Sign in</button>
</form>
<p class="muted">Use <code>linguine admin create-key --name &lt;label&gt; --role admin</code> to create an admin key.</p>`
	return renderPage("Sign in", body)
}

func homePage(nodes []fleet.NodeView, online int) string {
	body := fmt.Sprintf(`<div class="metrics">
<div class="metric"><div class="v">%d</div><div class="l">Nodes total</div></div>
<div class="metric"><div class="v">%d</div><div class="l">Online</div></div>
<div class="metric"><div class="v">%d</div><div class="l">Offline / stale</div></div>
</div>
<h2>Fleet</h2>
<table><thead><tr><th>Node</th><th>Status</th><th>Active model</th><th>Active requests</th><th>Last heartbeat</th></tr></thead><tbody>`,
		len(nodes), online, len(nodes)-online)
	if len(nodes) == 0 {
		body += `<tr><td colspan="5" class="muted">No workers enrolled.</td></tr>`
	}
	for _, n := range nodes {
		body += fmt.Sprintf(`<tr><td><a href="/admin/nodes/%s">%s</a></td><td><span class="status %s">%s</span></td><td>%s</td><td>%d</td><td>%s</td></tr>`,
			n.ID, n.ID, strings.ToLower(n.Status), n.Status, orDash(n.ActiveModel), n.ActiveRequests, n.LastHeartbeat.Format(time.RFC3339))
	}
	body += `</tbody></table>`
	return renderPage("Dashboard", body)
}

func nodesPage(nodes []fleet.NodeView) string {
	body := `<h1>Node inventory</h1>
<table hx-trigger="every 5s" hx-get="/admin/nodes" hx-target="this" hx-swap="outerHTML"><thead><tr><th>Node</th><th>Status</th><th>Active model</th><th>VRAM free / total (MB)</th><th>Active reqs</th><th>Est. TPS</th><th>Catalog</th><th>Last heartbeat</th></tr></thead><tbody>`
	if len(nodes) == 0 {
		body += `<tr><td colspan="8" class="muted">No workers enrolled.</td></tr>`
	}
	for _, n := range nodes {
		body += fmt.Sprintf(`<tr><td><a href="/admin/nodes/%s">%s</a></td><td><span class="status %s">%s</span></td><td>%s</td><td>%d / %d</td><td>%d</td><td>%.1f</td><td>%s</td><td>%s</td></tr>`,
			n.ID, n.ID, strings.ToLower(n.Status), n.Status, orDash(n.ActiveModel),
			n.VRAMFreeMB, n.VRAMTotalMB, n.ActiveRequests, n.EstimatedTPS,
			strings.Join(n.Catalog, ", "), n.LastHeartbeat.Format(time.RFC3339))
	}
	body += `</tbody></table>`
	return renderPage("Nodes", body)
}

func nodeDetailPage(n fleet.NodeView) string {
	body := fmt.Sprintf(`<h1>%s</h1>
<div class="card"><h2>State</h2>
<table><tbody>
<tr><th>Status</th><td><span class="status %s">%s</span></td></tr>
<tr><th>Active model</th><td>%s</td></tr>
<tr><th>VRAM free / total (MB)</th><td>%d / %d</td></tr>
<tr><th>Active requests</th><td>%d</td></tr>
<tr><th>Estimated TPS</th><td>%.1f</td></tr>
<tr><th>Last heartbeat</th><td>%s</td></tr>
</tbody></table></div>
<div class="card"><h2>Catalog</h2><p>%s</p></div>`,
		n.ID, strings.ToLower(n.Status), n.Status, orDash(n.ActiveModel),
		n.VRAMFreeMB, n.VRAMTotalMB, n.ActiveRequests, n.EstimatedTPS,
			n.LastHeartbeat.Format(time.RFC3339), strings.Join(n.Catalog, ", "))
	return renderPage(n.ID, body)
}

func auditPage(entries []audit.Entry) string {
	body := `<h1>Request audit log</h1>
<table><thead><tr><th>Time</th><th>API key</th><th>Node</th><th>Model req / served</th><th>Streamed</th><th>Status</th><th>Duration (ms)</th></tr></thead><tbody>`
	if len(entries) == 0 {
		body += `<tr><td colspan="7" class="muted">No requests recorded yet.</td></tr>`
	}
	for _, e := range entries {
		body += fmt.Sprintf(`<tr><td>%s</td><td class="muted">%s</td><td>%s</td><td>%s / %s</td><td>%v</td><td>%d</td><td>%d</td></tr>`,
			e.CreatedAt.Format(time.RFC3339),
			shortID(e.APIKeyID), orDash(e.NodeID), orDash(e.ModelRequested), orDash(e.ModelServed),
			e.WasStreamed, e.StatusCode, e.TotalDurationMs)
	}
	body += `</tbody></table>`
	return renderPage("Audit log", body)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8] + "…"
	}
	return s
}
