package web

import "html/template"

// pageHead is clusterbook's (internal/portal.go): the base URL, the embedded
// class before the first paint, and the design tokens. The copy under
// :where(:root) keeps the standalone look; the portal's :root wins.
const pageHead = `<base href="{{.Page.Base}}/">
    <script>if (window.self !== window.top) document.documentElement.classList.add('hr-embedded')</script>
    <style>
        :where(:root) {
            --hr-bg: #0f172a; --hr-surface: #111827; --hr-card: #1e293b; --hr-border: #334155;
            --hr-text: #e2e8f0; --hr-muted: #94a3b8; --hr-accent: #6366f1;
            --hr-font: -apple-system, BlinkMacSystemFont, 'Segoe UI', system-ui, sans-serif;
        }
        .embedded-bar { display: none; }
        .hr-embedded .banner { display: none; }
        .hr-embedded .embedded-bar { display: flex; align-items: center; justify-content: space-between; gap: 1rem; min-height: 44px; padding: 6px 16px; font-family: var(--hr-font); background: var(--hr-surface); border-bottom: 1px solid var(--hr-border); }
        .hr-embedded .container { padding-top: 1rem; }
        .embedded-sub { font-size: 13px; color: var(--hr-muted); }
    </style>
    {{if .Page.PortalTheme}}<link rel="stylesheet" href="/static/homerun2-tokens.css">{{end}}`

const styles = `<style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body { font-family: var(--hr-font); background: var(--hr-bg); color: var(--hr-text); min-height: 100vh; }
        .container { max-width: 1200px; margin: 0 auto; padding: 2rem 1rem; }
        h1 { font-size: 1.75rem; margin-bottom: 0.25rem; }
        h2 { font-size: 1.1rem; margin: 1.5rem 0 0.75rem; color: var(--hr-muted); text-transform: uppercase; letter-spacing: 0.05em; }
        .subtitle { color: var(--hr-muted); margin-bottom: 1.5rem; }
        a { color: var(--hr-accent); text-decoration: none; }
        table { width: 100%; border-collapse: collapse; background: var(--hr-card); border-radius: 12px; overflow: hidden; }
        th { background: var(--hr-border); text-align: left; padding: 0.6rem 1rem; font-size: 0.75rem; text-transform: uppercase; letter-spacing: 0.05em; color: var(--hr-muted); }
        td { padding: 0.6rem 1rem; border-bottom: 1px solid var(--hr-border); font-size: 0.9rem; }
        code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 0.8rem; }
        .pill { display: inline-block; padding: 0.1rem 0.6rem; border-radius: 9999px; font-size: 0.75rem; border: 1px solid var(--hr-border); }
        .pill.ok { color: #22c55e; border-color: #22c55e; } .pill.bad { color: #ef4444; border-color: #ef4444; }
        .pill.busy { color: var(--hr-accent); border-color: var(--hr-accent); } .pill.muted { color: var(--hr-muted); }
        .card { background: var(--hr-card); border: 1px solid var(--hr-border); border-radius: 12px; padding: 1.25rem; margin-bottom: 1rem; }
        .grid { display: grid; grid-template-columns: max-content 1fr; gap: 0.4rem 1.5rem; font-size: 0.9rem; }
        .grid dt { color: var(--hr-muted); }
        .timeline { list-style: none; display: flex; flex-wrap: wrap; gap: 0.5rem; }
        .timeline li { padding: 0.5rem 0.75rem; border-radius: 8px; border: 1px solid var(--hr-border); font-size: 0.8rem; min-width: 7rem; }
        .timeline li.done { border-color: #22c55e; } .timeline li.current { border-color: var(--hr-accent); background: var(--hr-surface); }
        .timeline li.todo { opacity: 0.5; } .timeline small { display: block; color: var(--hr-muted); }
        details { margin-bottom: 0.5rem; } summary { cursor: pointer; padding: 0.4rem 0; }
        pre { background: var(--hr-bg); border: 1px solid var(--hr-border); border-radius: 8px; padding: 0.75rem; overflow-x: auto; }
        button.danger { background: transparent; color: #ef4444; border: 1px solid #ef4444; border-radius: 6px; padding: 0.4rem 1rem; cursor: pointer; }
        .error { color: #ef4444; margin-top: 0.5rem; } .muted { color: var(--hr-muted); }
        .empty { padding: 2rem; text-align: center; color: var(--hr-muted); }
    </style>`

var errorTmpl = template.Must(template.New("error").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>{{.Status}}</title></head>
<body style="font-family: system-ui; padding: 2rem"><h1>{{.Status}} {{.Text}}</h1>
<p>Log in through sthings-portal, or send a bearer token.</p></body></html>`))

const rowDef = `{{define "row"}}<tr id="row-{{.Name}}">
  <td><a href="ui/orders/{{.Name}}"><code>{{.Name}}</code></a></td>
  <td>{{.Spec.Template}}</td>
  <td>{{.Spec.Placement.ManagementCluster}}</td>
  <td><span class="pill {{phaseClass .Status.Phase}}">{{if .Status.Phase}}{{.Status.Phase}}{{else}}Pending{{end}}</span></td>
  <td>{{with .Status.Delivery}}{{with .PullRequest}}<a href="{{.URL}}" target="_blank" rel="noopener">#{{.Number}}</a> <span class="muted">{{.State}}</span>{{end}}{{end}}</td>
  <td>{{with .Status.Live}}{{if .Ready}}<span class="pill ok">Ready</span>{{else if .Found}}<span class="pill busy">building</span>{{else}}<span class="muted">not seen</span>{{end}} {{with .Address}}<code>{{.}}</code>{{end}}{{else}}<span class="muted">–</span>{{end}}</td>
  <td class="muted">{{ago .CreationTimestamp.Time}}</td>
</tr>{{end}}`

var rowTmpl = template.Must(template.New("rowonly").Funcs(funcs).Parse(rowDef + `{{template "row" .}}`))

var rowsTmpl = template.Must(template.New("rows").Funcs(funcs).Parse(rowDef + `{{range .}}{{template "row" .}}{{else}}<tr><td colspan="7" class="empty">No orders yet. Order through the claims CLI or Backstage.</td></tr>{{end}}`))

var listTmpl = template.Must(template.New("list").Funcs(funcs).Parse(rowDef + `<!DOCTYPE html>
<html lang="en"><head>
    <meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
    ` + pageHead + `
    <title>My orders · machinery-order-service</title>
    ` + styles + `
</head><body>
    <div class="embedded-bar"><span class="embedded-sub">My orders</span><span class="embedded-sub">{{.Page.User}}</span></div>
    <div class="container">
        <div class="banner">
            <h1>My orders</h1>
            <p class="subtitle">{{if .Page.User}}Orders of <code>{{.Page.User}}</code>{{else}}All orders (no login: auth is off){{end}} · updated live</p>
        </div>
        <table>
            <thead><tr><th>Order</th><th>Template</th><th>Cluster</th><th>Phase</th><th>Pull request</th><th>Live</th><th>Age</th></tr></thead>
            <tbody id="orders">{{range .Orders}}{{template "row" .}}{{else}}<tr id="empty"><td colspan="7" class="empty">No orders yet. Order through the claims CLI or Backstage.</td></tr>{{end}}</tbody>
        </table>
    </div>
    <script>
    (function () {
        var tbody = document.getElementById('orders');
        var es = new EventSource('ui/events');
        es.addEventListener('order', function (ev) {
            var t = document.createElement('tbody');
            t.innerHTML = ev.data;
            var row = t.firstElementChild;
            if (!row) return;
            var old = document.getElementById(row.id);
            if (row.dataset.deleted) { if (old) old.remove(); return; }
            var empty = document.getElementById('empty');
            if (empty) empty.remove();
            if (old) old.replaceWith(row); else tbody.prepend(row);
        });
    })();
    </script>
</body></html>`))

const detailBodyDef = `{{define "body"}}{{with .Order}}
<div class="card">
  <dl class="grid">
    <dt>Order ID</dt><dd><code>{{.Name}}</code></dd>
    <dt>Phase</dt><dd><span class="pill {{phaseClass .Status.Phase}}">{{if .Status.Phase}}{{.Status.Phase}}{{else}}Pending{{end}}</span> <span class="muted">{{.Status.Message}}</span></dd>
    <dt>Template</dt><dd>{{.Spec.Template}}</dd>
    <dt>Management cluster</dt><dd>{{.Spec.Placement.ManagementCluster}}</dd>
    {{with .Spec.RequestedBy}}<dt>Requested by</dt><dd><code>{{.}}</code></dd>{{end}}
    {{with .Spec.Reason}}<dt>Reason</dt><dd>{{.}}</dd>{{end}}
    {{with .Status.Delivery}}
      {{with .PullRequest}}<dt>Pull request</dt><dd><a href="{{.URL}}" target="_blank" rel="noopener">#{{.Number}}</a> <span class="muted">{{.State}}</span></dd>{{end}}
      {{with .Branch}}<dt>Branch</dt><dd><code>{{.}}</code></dd>{{end}}
      {{with .CommitSHA}}<dt>Commit</dt><dd><code>{{short .}}</code></dd>{{end}}
      {{with .MergeSHA}}<dt>Merge</dt><dd><code>{{short .}}</code></dd>{{end}}
    {{end}}
    {{with .Status.Flux}}<dt>Flux</dt><dd><code>{{.LastAppliedRevision}}</code></dd>{{end}}
    {{with .Status.XRRef}}<dt>XR</dt><dd><code>{{.Kind}} {{if .Namespace}}{{.Namespace}}/{{end}}{{.Name}}</code></dd>{{end}}
    {{with .Status.Live}}<dt>Live</dt><dd>{{if .Ready}}<span class="pill ok">Ready</span>{{else if .Found}}<span class="pill busy">building</span>{{else}}<span class="muted">not seen by machinery yet</span>{{end}} {{with .Address}}<code>{{.}}</code>{{end}} <span class="muted">{{.StatusMessage}}</span></dd>
      {{$info := .Info}}{{range infoKeys .Info}}<dt>{{.}}</dt><dd><code>{{index $info .}}</code></dd>{{end}}{{end}}
  </dl>
</div>
<h2>Timeline</h2>
<ol class="timeline">{{range timeline .}}<li class="{{.State}}"><strong>{{.Phase}}</strong>{{with .Time}}<small>{{.}}</small>{{end}}{{with .Message}}<small>{{.}}</small>{{end}}</li>{{end}}</ol>
{{end}}
{{if .CanCancel}}
<h2>Cancel</h2>
<form hx-post="ui/orders/{{.Order.Name}}/cancel" hx-target="#detail" hx-confirm="Cancel this order? Its pull request is closed and nothing is built.">
  <input type="hidden" name="resourceVersion" value="{{.Order.ResourceVersion}}">
  <button class="danger" type="submit">Cancel order</button>
</form>
{{end}}
<h2>Files</h2>
{{range .Files}}<details><summary><code>{{.Path}}</code></summary><pre>{{.Content}}</pre></details>{{else}}<p class="muted">Not rendered yet.</p>{{end}}
{{end}}`

var detailBodyTmpl = template.Must(template.New("detailBody").Funcs(funcs).Parse(detailBodyDef + `{{template "body" .}}`))

var detailTmpl = template.Must(template.New("detail").Funcs(funcs).Parse(detailBodyDef + `<!DOCTYPE html>
<html lang="en"><head>
    <meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
    ` + pageHead + `
    <title>{{.Order.Name}} · machinery-order-service</title>
    <script src="ui/static/htmx.min.js"></script>
    ` + styles + `
</head><body>
    <div class="embedded-bar"><span class="embedded-sub"><a href="ui/">My orders</a> › {{.Order.Name}}</span></div>
    <div class="container">
        <div class="banner">
            <p><a href="ui/">← My orders</a></p>
            <h1>Order {{.Order.Name}}</h1>
            <p class="subtitle">{{.Order.Spec.Template}} on {{.Order.Spec.Placement.ManagementCluster}} · updated live</p>
        </div>
        <div id="detail">{{template "body" .}}</div>
    </div>
    <script>
    (function () {
        var es = new EventSource('ui/events?id={{.Order.Name}}');
        es.addEventListener('order', function () {
            htmx.ajax('GET', 'ui/orders/{{.Order.Name}}/body', '#detail');
        });
    })();
    </script>
</body></html>`))
