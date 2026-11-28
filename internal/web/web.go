// Package web is the UI, stage 1 of stuttgart-things/stuttgart-things#4300:
// two pages, read-only except Cancel and Tear down. No ordering form: orders come in
// through the API (claims, Backstage).
//
//	ui/                 My orders: phase, cluster, PR, live status; SSE updates
//	ui/?scope=all       All I may see (ADR 0001: admins every order, others
//	                    their own and their teams'), with orderer and group;
//	                    &group= keeps one group, &sort=group groups the rows
//	ui/orders/{id}      Order detail: timeline, files, PR/commit, order ID, Cancel, Tear down
//	ui/manifest.json    sthings-portal manifest (no login)
//	ui/fragment/orders  portal overview fragment: my orders by state, newest
//
// Built like clusterbook (internal/portal.go): every URL is relative to
// <base href>, so the pages work under BASE_PATH behind sthings-portal;
// colours come from the --hr-* tokens, the portal's win when the request came
// through it (X-Forwarded-Prefix); inside the portal's frame the banner
// becomes one compact line. Login: sthings-portal's forwarded user, when
// AUTH_TRUST_FORWARDED_USER is set, or a bearer token.
package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/api"
)

//go:embed static
var staticFS embed.FS

// UI serves the pages through the API server's logic.
type UI struct {
	API      *api.Server
	BasePath string
}

// Register adds the UI routes under prefix (the BASE_PATH).
func (u *UI) Register(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("GET "+prefix+"/ui/{$}", u.list)
	mux.HandleFunc("GET "+prefix+"/ui/rows", u.rows)
	mux.HandleFunc("GET "+prefix+"/ui/events", u.events)
	mux.HandleFunc("GET "+prefix+"/ui/orders/{id}", u.detail)
	mux.HandleFunc("GET "+prefix+"/ui/orders/{id}/body", u.detailBody)
	mux.HandleFunc("POST "+prefix+"/ui/orders/{id}/cancel", u.cancel)
	mux.HandleFunc("POST "+prefix+"/ui/orders/{id}/teardown", u.teardown)
	mux.HandleFunc("GET "+prefix+"/ui/manifest.json", u.manifest)
	mux.HandleFunc("GET "+prefix+"/ui/fragment/orders", u.ordersFragment)
	mux.Handle("GET "+prefix+"/ui/static/", http.StripPrefix(prefix+"/ui/", http.FileServerFS(staticFS)))
	if prefix != "" {
		mux.Handle("GET "+prefix+"/ui", http.RedirectHandler(prefix+"/ui/", http.StatusMovedPermanently))
		mux.Handle("GET "+prefix+"/{$}", http.RedirectHandler(prefix+"/ui/", http.StatusFound))
	} else {
		mux.Handle("GET /ui", http.RedirectHandler("/ui/", http.StatusMovedPermanently))
		mux.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusFound))
	}
}

// page is what every full page gets.
type page struct {
	Base        string
	PortalTheme bool
	User        string
	Auth        bool
}

func (u *UI) page(r *http.Request, c *api.Caller) page {
	return page{
		Base:        strings.TrimRight(u.BasePath, "/"),
		PortalTheme: r.Header.Get("X-Forwarded-Prefix") != "",
		User:        c.Ref,
		Auth:        u.API.Auth != nil,
	}
}

// authorize checks the UI login and answers an HTML error itself.
func (u *UI) authorize(w http.ResponseWriter, r *http.Request) (*api.Caller, bool) {
	rec := &captured{header: http.Header{}}
	c, ok := u.API.Authorize(rec, r, true)
	if ok {
		return c, true
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(rec.status)
	_ = errorTmpl.Execute(w, map[string]any{"Status": rec.status, "Text": http.StatusText(rec.status)})
	return nil, false
}

// captured swallows the API's JSON error so the UI can answer HTML.
type captured struct {
	header http.Header
	status int
}

func (c *captured) Header() http.Header         { return c.header }
func (c *captured) Write(b []byte) (int, error) { return len(b), nil }
func (c *captured) WriteHeader(s int)           { c.status = s }

// view is what the list shows: "my orders" (the default) or everything the
// caller may see (scope=all), optionally one group and sorted by group.
type view struct {
	All     bool
	Group   string
	ByGroup bool
	Admin   bool
	// Groups are the groups of the visible orders, for the group filter.
	Groups []string
	// Query repeats the view for ui/events and ui/rows.
	Query string
}

func (u *UI) viewOf(r *http.Request, c *api.Caller) view {
	q := r.URL.Query()
	v := view{All: q.Get("scope") == "all", Admin: u.API.IsAdmin(c)}
	if v.All {
		v.Group = q.Get("group")
		v.ByGroup = q.Get("sort") == "group"
	}
	qs := url.Values{}
	if v.All {
		qs.Set("scope", "all")
		if v.Group != "" {
			qs.Set("group", v.Group)
		}
		if v.ByGroup {
			qs.Set("sort", "group")
		}
	}
	v.Query = qs.Encode()
	return v
}

// filter is the view's filter. "My orders" are the caller's with auth and
// every order without; scope=all is what the caller may see.
func (u *UI) filter(c *api.Caller, v view) api.Filter {
	if !v.All {
		return api.Filter{RequestedBy: c.Ref}
	}
	return api.Filter{Visible: u.API.VisibleTo(c), Group: v.Group}
}

// listRow is one table row: the order, and whether the wide (scope=all)
// columns are shown.
type listRow struct {
	api.Row
	Wide bool
}

// GroupKey is the row's group label ("" for none).
func (r listRow) GroupKey() string { return strings.Join(r.Groups, ", ") }

// GroupLabel is GroupKey without the Backstage default namespace.
func (r listRow) GroupLabel() string {
	return strings.ReplaceAll(r.GroupKey(), "group:default/", "")
}

func (u *UI) listRows(r *http.Request, c *api.Caller, v *view) ([]listRow, error) {
	rows, err := u.API.ListRows(r.Context(), u.filter(c, *v))
	if err != nil {
		return nil, err
	}
	if v.All {
		// The group filter offers every group of what the caller may see,
		// not only of the rows the current filter keeps.
		all := rows
		if v.Group != "" {
			if all, err = u.API.ListRows(r.Context(), u.filter(c, view{All: true})); err != nil {
				return nil, err
			}
		}
		set := map[string]bool{}
		for _, a := range all {
			for _, g := range a.Groups {
				set[g] = true
			}
		}
		for g := range set {
			v.Groups = append(v.Groups, g)
		}
		sort.Strings(v.Groups)
	}
	if v.ByGroup {
		// Grouped: by group label (orders without a group last), newest
		// first within a group (rows already come newest first).
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := strings.Join(rows[i].Groups, ", "), strings.Join(rows[j].Groups, ", ")
			if (a == "") != (b == "") {
				return b == ""
			}
			return a < b
		})
	}
	out := make([]listRow, len(rows))
	for i, row := range rows {
		out[i] = listRow{Row: row, Wide: v.All}
	}
	return out, nil
}

func (u *UI) list(w http.ResponseWriter, r *http.Request) {
	c, ok := u.authorize(w, r)
	if !ok {
		return
	}
	v := u.viewOf(r, c)
	rows, err := u.listRows(r, c, &v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, listTmpl, map[string]any{"Page": u.page(r, c), "Rows": rows, "View": v})
}

func (u *UI) rows(w http.ResponseWriter, r *http.Request) {
	c, ok := u.authorize(w, r)
	if !ok {
		return
	}
	v := u.viewOf(r, c)
	rows, err := u.listRows(r, c, &v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, rowsTmpl, map[string]any{"Rows": rows, "View": v})
}

// events streams changed rows (list) or a reload signal (detail, ?id=).
func (u *UI) events(w http.ResponseWriter, r *http.Request) {
	c, ok := u.authorize(w, r)
	if !ok {
		return
	}
	v := u.viewOf(r, c)
	f := u.filter(c, v)
	only := r.URL.Query().Get("id")
	u.API.Stream(w, r, f, "order", func(e api.Event) (string, bool) {
		if only != "" {
			if e.Order.Name != only {
				return "", false
			}
			return e.Order.ResourceVersion, true
		}
		if e.Type == "DELETED" {
			return fmt.Sprintf(`<tr id="row-%s" data-deleted="true"></tr>`, template.HTMLEscapeString(e.Order.Name)), true
		}
		var buf bytes.Buffer
		if err := rowTmpl.Execute(&buf, listRow{Row: api.RowOf(e.Order), Wide: v.All}); err != nil {
			return "", false
		}
		return strings.ReplaceAll(buf.String(), "\n", " "), true
	})
}

type detailData struct {
	Page        page
	Order       *v1alpha1.Order
	Files       []v1alpha1.RenderedFile
	CanCancel   bool
	CanTearDown bool
	Error       string
}

func (u *UI) load(w http.ResponseWriter, r *http.Request) (*api.Caller, *detailData, bool) {
	c, ok := u.authorize(w, r)
	if !ok {
		return nil, nil, false
	}
	o, err := u.API.Get(r.Context(), r.PathValue("id"))
	if apierrors.IsNotFound(err) || (err == nil && !u.API.MaySee(c, o)) {
		http.Error(w, "order not found", http.StatusNotFound)
		return nil, nil, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, nil, false
	}
	d := &detailData{Page: u.page(r, c), Order: o}
	if rd := o.EffectiveRendered(); rd != nil {
		d.Files = rd.Files
	}
	d.CanCancel = o.Status.Phase.BeforeMerge() && !o.Spec.Cancel && u.API.MayCancel(c, o)
	d.CanTearDown = api.TeardownRefused(o) == "" && u.API.MayTearDown(c, o)
	return c, d, true
}

func (u *UI) detail(w http.ResponseWriter, r *http.Request) {
	if _, d, ok := u.load(w, r); ok {
		render(w, detailTmpl, d)
	}
}

func (u *UI) detailBody(w http.ResponseWriter, r *http.Request) {
	if _, d, ok := u.load(w, r); ok {
		render(w, detailBodyTmpl, d)
	}
}

func (u *UI) cancel(w http.ResponseWriter, r *http.Request) {
	c, ok := u.authorize(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	status, body := u.API.Cancel(r.Context(), c, id, r.FormValue("resourceVersion"))
	u.after(w, r, id, status, body, "cancel failed")
}

// teardown asks for a teardown from the UI. The UI never asks for
// auto-merge: a human merges (docs/adr/0003-teardown.md).
func (u *UI) teardown(w http.ResponseWriter, r *http.Request) {
	c, ok := u.authorize(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	status, body := u.API.Teardown(r.Context(), c, id, api.TeardownRequest{
		Reason: strings.TrimSpace(r.FormValue("reason")), ResourceVersion: r.FormValue("resourceVersion"),
	})
	u.after(w, r, id, status, body, "teardown failed")
}

// after answers an action: the error, or the reloaded detail body.
func (u *UI) after(w http.ResponseWriter, r *http.Request, id string, status int, body any, failed string) {
	if status/100 != 2 {
		msg := failed
		if e, ok := body.(api.ErrorResponse); ok {
			msg = e.Error
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `<p class="error">%s</p>`, template.HTMLEscapeString(msg))
		return
	}
	// Reload the detail body; the controller acts right after.
	r2 := r.Clone(r.Context())
	r2.SetPathValue("id", id)
	u.detailBody(w, r2)
}

func render(w http.ResponseWriter, t *template.Template, data any) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		slog.Error("template", "name", t.Name(), "error", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// phaseClass colours a phase.
func phaseClass(p v1alpha1.Phase) string {
	switch p {
	case v1alpha1.PhaseReady:
		return "ok"
	case v1alpha1.PhaseFailed:
		return "bad"
	case v1alpha1.PhaseCancelled, v1alpha1.PhaseDeleted:
		return "muted"
	case "":
		return "muted"
	}
	return "busy"
}

// timeline is every phase of the happy path, marked done/current/todo, plus
// the terminal phase when the order ended off it.
type step struct {
	Phase   v1alpha1.Phase
	State   string // done, current, todo
	Time    string
	Message string
}

var path = []v1alpha1.Phase{
	v1alpha1.PhaseOrdered, v1alpha1.PhasePlaced, v1alpha1.PhasePROpen, v1alpha1.PhaseMerged,
	v1alpha1.PhaseApplied, v1alpha1.PhaseBuilding, v1alpha1.PhaseReady,
}

// recoveredPath replaces path for an Order rebuilt from git (#4823).
var recoveredPath = []v1alpha1.Phase{v1alpha1.PhaseRecovered, v1alpha1.PhaseBuilding, v1alpha1.PhaseReady}

// teardownPath follows path once a teardown was asked for.
var teardownPath = []v1alpha1.Phase{v1alpha1.PhaseTeardownPROpen, v1alpha1.PhaseTearingDown, v1alpha1.PhaseDeleted}

func timeline(o *v1alpha1.Order) []step {
	seen := map[v1alpha1.Phase]v1alpha1.PhaseTransition{}
	for _, h := range o.Status.History {
		seen[h.Phase] = h
	}
	var out []step
	phases := path
	if rc := o.Spec.Recovered; rc != nil {
		phases = recoveredPath
		if rc.PullRequest != nil && rc.PullRequest.State == "open" {
			// recovered before the merge: from PROpen on
			phases = path[slices.Index(path, v1alpha1.PhasePROpen):]
		}
	}
	if o.Spec.Teardown != nil {
		phases = append(slices.Clone(phases), teardownPath...)
	}
	for _, p := range phases {
		s := step{Phase: p, State: "todo"}
		if h, ok := seen[p]; ok {
			s.State, s.Time, s.Message = "done", h.Time.UTC().Format(time.DateTime), h.Message
		}
		if p == o.Status.Phase {
			s.State = "current"
		}
		out = append(out, s)
	}
	if o.Status.Phase.Terminal() || (o.Status.Phase == v1alpha1.PhaseDeleted && o.Spec.Teardown == nil) {
		h := seen[o.Status.Phase]
		out = append(out, step{Phase: o.Status.Phase, State: "current", Time: h.Time.UTC().Format(time.DateTime), Message: h.Message})
	}
	return out
}

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func infoKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var funcs = template.FuncMap{
	"phaseClass": phaseClass,
	"timeline":   timeline,
	"ago":        ago,
	"infoKeys":   infoKeys,
	"join":       strings.Join,
	// groupNames shows groups without the Backstage default namespace.
	"groupNames": func(gs []string) string {
		out := make([]string, len(gs))
		for i, g := range gs {
			out[i] = strings.TrimPrefix(g, "group:default/")
		}
		return strings.Join(out, ", ")
	},
	"groupName": func(g string) string { return strings.TrimPrefix(g, "group:default/") },
	"short": func(s string) string {
		if len(s) > 7 {
			return s[:7]
		}
		return s
	},
}
