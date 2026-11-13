// Package web is the UI, stage 1 of stuttgart-things/stuttgart-things#4300:
// two pages, read-only except Cancel. No ordering form: orders come in
// through the API (claims, Backstage).
//
//	ui/                 My orders: phase, cluster, PR, live status; SSE updates
//	ui/orders/{id}      Order detail: timeline, files, PR/commit, order ID, Cancel
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

// filter is "my orders": the caller's with auth, every order without.
func filter(c *api.Caller) api.Filter {
	return api.Filter{RequestedBy: c.Ref}
}

func (u *UI) list(w http.ResponseWriter, r *http.Request) {
	c, ok := u.authorize(w, r)
	if !ok {
		return
	}
	orders, err := u.API.List(r.Context(), filter(c))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, listTmpl, map[string]any{"Page": u.page(r, c), "Orders": orders})
}

func (u *UI) rows(w http.ResponseWriter, r *http.Request) {
	c, ok := u.authorize(w, r)
	if !ok {
		return
	}
	orders, err := u.API.List(r.Context(), filter(c))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, rowsTmpl, orders)
}

// events streams changed rows (list) or a reload signal (detail, ?id=).
func (u *UI) events(w http.ResponseWriter, r *http.Request) {
	c, ok := u.authorize(w, r)
	if !ok {
		return
	}
	f := filter(c)
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
		if err := rowTmpl.Execute(&buf, e.Order); err != nil {
			return "", false
		}
		return strings.ReplaceAll(buf.String(), "\n", " "), true
	})
}

type detailData struct {
	Page      page
	Order     *v1alpha1.Order
	Files     []v1alpha1.RenderedFile
	CanCancel bool
	Error     string
}

func (u *UI) load(w http.ResponseWriter, r *http.Request) (*api.Caller, *detailData, bool) {
	c, ok := u.authorize(w, r)
	if !ok {
		return nil, nil, false
	}
	o, err := u.API.Get(r.Context(), r.PathValue("id"))
	if apierrors.IsNotFound(err) {
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
	if status/100 != 2 {
		msg := "cancel failed"
		if e, ok := body.(api.ErrorResponse); ok {
			msg = e.Error
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `<p class="error">%s</p>`, template.HTMLEscapeString(msg))
		return
	}
	// Reload the detail body; the controller closes the PR right after.
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
	case v1alpha1.PhaseCancelled:
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

func timeline(o *v1alpha1.Order) []step {
	seen := map[v1alpha1.Phase]v1alpha1.PhaseTransition{}
	for _, h := range o.Status.History {
		seen[h.Phase] = h
	}
	var out []step
	for _, p := range path {
		s := step{Phase: p, State: "todo"}
		if h, ok := seen[p]; ok {
			s.State, s.Time, s.Message = "done", h.Time.UTC().Format(time.DateTime), h.Message
		}
		if p == o.Status.Phase {
			s.State = "current"
		}
		out = append(out, s)
	}
	if o.Status.Phase.Terminal() {
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
	"short": func(s string) string {
		if len(s) > 7 {
			return s[:7]
		}
		return s
	},
}
