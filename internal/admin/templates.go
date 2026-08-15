package admin

import (
	"html/template"
	"strings"
	"time"

	"github.com/samgw/linguine/internal/audit"
	"github.com/samgw/linguine/internal/fleet"
)

// All dynamic dashboard pages render through html/template with structured
// data so the templating engine applies context-aware auto-escaping. The
// earlier design built page bodies with fmt.Sprintf and injected them via
// template.HTML, which disabled escaping and let attacker-controlled fields
// (a client's requested model on the audit page; a worker's active model or
// catalog on the node pages) execute script in the admin's session. This
// file is the fix for that stored-XSS vector.
//
// Templates use the stdlib html/template rather than templ so the build stays
// pure-Go with no external code generator required.

var funcMap = template.FuncMap{
	"fmtTime": func(t time.Time) string { return t.Format(time.RFC3339) },
	"join":    func(xs []string) string { return strings.Join(xs, ", ") },
	"lower":   strings.ToLower,
	"orDash":  orDash,
	"shortID": shortID,
}

// baseSource is the shared page chrome. Each page defines a "content" block
// that the chrome renders via {{template "content" .}}. htmx is vendored and
// served locally (see static.go) so the page never loads third-party script.
const baseSource = `{{define "base"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>{{.Title}} — linguine admin</title>
<script src="/admin/static/htmx.min.js"></script>
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
{{template "content" .}}
</main>
</body>
</html>{{end}}`

// base is the parsed chrome. Pages clone it and add their own "content" block.
var base = template.Must(template.New("base").Funcs(funcMap).Parse(baseSource))

// mustPage clones the chrome and parses a page's content block, panicking on a
// parse error (page sources are compile-time constants).
func mustPage(contentSrc string) *template.Template {
	return template.Must(template.Must(base.Clone()).Parse(contentSrc))
}

var (
	homeTmpl       = mustPage(homeSource)
	nodesTmpl      = mustPage(nodesSource)
	nodeDetailTmpl = mustPage(nodeDetailSource)
	auditTmpl      = mustPage(auditSource)
	loginTmpl      = mustPage(loginSource)
)

func renderPage(tmpl *template.Template, data any) string {
	var b strings.Builder
	if err := tmpl.ExecuteTemplate(&b, "base", data); err != nil {
		return "admin: render error: " + err.Error()
	}
	return b.String()
}

// pageData carries the title shared by every page's chrome.
type pageData struct {
	Title string
}

func loginPage() string {
	return renderPage(loginTmpl, pageData{Title: "Sign in"})
}

// homeData is the structured payload for the dashboard home page.
type homeData struct {
	pageData
	Nodes   []fleet.NodeView
	Online  int
	Total   int
	Offline int
}

func homePage(nodes []fleet.NodeView, online int) string {
	return renderPage(homeTmpl, homeData{
		pageData: pageData{Title: "Dashboard"},
		Nodes:    nodes,
		Online:   online,
		Total:    len(nodes),
		Offline:  len(nodes) - online,
	})
}

// nodesData is the structured payload for the node inventory page.
type nodesData struct {
	pageData
	Nodes []fleet.NodeView
}

func nodesPage(nodes []fleet.NodeView) string {
	return renderPage(nodesTmpl, nodesData{pageData: pageData{Title: "Nodes"}, Nodes: nodes})
}

// nodeDetailData is the structured payload for a single node's detail page.
type nodeDetailData struct {
	pageData
	N fleet.NodeView
}

func nodeDetailPage(n fleet.NodeView) string {
	return renderPage(nodeDetailTmpl, nodeDetailData{pageData: pageData{Title: n.ID}, N: n})
}

// auditData is the structured payload for the audit page: recent request
// audit log entries plus recent admin auth events.
type auditData struct {
	pageData
	Entries     []audit.Entry
	AdminEvents []audit.AdminEvent
}

func auditPage(entries []audit.Entry, adminEvents []audit.AdminEvent) string {
	return renderPage(auditTmpl, auditData{
		pageData:    pageData{Title: "Audit log"},
		Entries:     entries,
		AdminEvents: adminEvents,
	})
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

const loginSource = `{{define "content"}}
<h1>Sign in</h1>
<form method="post" action="/admin/login">
<label>Admin API key <input type="password" name="password" placeholder="sk-mesh-…" autofocus/></label>
<button type="submit">Sign in</button>
</form>
<p class="muted">Use <code>linguine admin create-key --name &lt;label&gt; --role admin</code> to create an admin key.</p>
{{end}}`

const homeSource = `{{define "content"}}
<div class="metrics">
<div class="metric"><div class="v">{{.Total}}</div><div class="l">Nodes total</div></div>
<div class="metric"><div class="v">{{.Online}}</div><div class="l">Online</div></div>
<div class="metric"><div class="v">{{.Offline}}</div><div class="l">Offline / stale</div></div>
</div>
<h2>Fleet</h2>
<table><thead><tr><th>Node</th><th>Status</th><th>Active model</th><th>Active requests</th><th>Last heartbeat</th></tr></thead><tbody>
{{- if not .Nodes}}
<tr><td colspan="5" class="muted">No workers enrolled.</td></tr>
{{- end}}
{{range .Nodes}}
<tr><td><a href="/admin/nodes/{{.ID}}">{{.ID}}</a></td><td><span class="status {{.Status | lower}}">{{.Status}}</span></td><td>{{.ActiveModel | orDash}}</td><td>{{.ActiveRequests}}</td><td>{{.LastHeartbeat | fmtTime}}</td></tr>
{{- end}}
</tbody></table>
{{end}}`

const nodesSource = `{{define "content"}}
<h1>Node inventory</h1>
<table hx-trigger="every 5s" hx-get="/admin/nodes" hx-target="this" hx-swap="outerHTML"><thead><tr><th>Node</th><th>Status</th><th>Active model</th><th>VRAM free / total (MB)</th><th>Active reqs</th><th>Est. TPS</th><th>Catalog</th><th>Last heartbeat</th></tr></thead><tbody>
{{- if not .Nodes}}
<tr><td colspan="8" class="muted">No workers enrolled.</td></tr>
{{- end}}
{{range .Nodes}}
<tr><td><a href="/admin/nodes/{{.ID}}">{{.ID}}</a></td><td><span class="status {{.Status | lower}}">{{.Status}}</span></td><td>{{.ActiveModel | orDash}}</td><td>{{.VRAMFreeMB}} / {{.VRAMTotalMB}}</td><td>{{.ActiveRequests}}</td><td>{{printf "%.1f" .EstimatedTPS}}</td><td>{{.Catalog | join}}</td><td>{{.LastHeartbeat | fmtTime}}</td></tr>
{{- end}}
</tbody></table>
{{end}}`

const nodeDetailSource = `{{define "content"}}
<h1>{{.N.ID}}</h1>
<div class="card"><h2>State</h2>
<table><tbody>
<tr><th>Status</th><td><span class="status {{.N.Status | lower}}">{{.N.Status}}</span></td></tr>
<tr><th>Active model</th><td>{{.N.ActiveModel | orDash}}</td></tr>
<tr><th>VRAM free / total (MB)</th><td>{{.N.VRAMFreeMB}} / {{.N.VRAMTotalMB}}</td></tr>
<tr><th>Active requests</th><td>{{.N.ActiveRequests}}</td></tr>
<tr><th>Estimated TPS</th><td>{{printf "%.1f" .N.EstimatedTPS}}</td></tr>
<tr><th>Last heartbeat</th><td>{{.N.LastHeartbeat | fmtTime}}</td></tr>
</tbody></table></div>
<div class="card"><h2>Catalog</h2><p>{{.N.Catalog | join}}</p></div>
{{end}}`

const auditSource = `{{define "content"}}
<h1>Request audit log</h1>
<table><thead><tr><th>Time</th><th>API key</th><th>Node</th><th>Model req / served</th><th>Streamed</th><th>Status</th><th>Duration (ms)</th></tr></thead><tbody>
{{- if not .Entries}}
<tr><td colspan="7" class="muted">No requests recorded yet.</td></tr>
{{- end}}
{{range .Entries}}
<tr><td>{{.CreatedAt | fmtTime}}</td><td class="muted">{{.APIKeyID | shortID}}</td><td>{{.NodeID | orDash}}</td><td>{{.ModelRequested | orDash}} / {{.ModelServed | orDash}}</td><td>{{.WasStreamed}}</td><td>{{.StatusCode}}</td><td>{{.TotalDurationMs}}</td></tr>
{{- end}}
</tbody></table>
<h2>Admin auth events</h2>
<table><thead><tr><th>Time</th><th>Event</th><th>API key</th><th>Remote IP</th><th>Status</th></tr></thead><tbody>
{{- if not .AdminEvents}}
<tr><td colspan="5" class="muted">No admin events recorded yet.</td></tr>
{{- end}}
{{range .AdminEvents}}
<tr><td>{{.CreatedAt | fmtTime}}</td><td>{{.Event}}</td><td class="muted">{{.APIKeyID | shortID}}</td><td>{{.RemoteIP | orDash}}</td><td>{{.StatusCode}}</td></tr>
{{- end}}
</tbody></table>
{{end}}`
