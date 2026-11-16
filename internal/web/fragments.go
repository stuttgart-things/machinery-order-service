package web

import (
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/intake"
	"github.com/stuttgart-things/machinery-order-service/internal/version"
)

// Fragments for sthings-portal's overview (its README, "Fragments"). The
// portal reads <prefix>/ui/manifest.json with every health check, without a
// login, and the browser loads each fragment through the portal's proxy with
// htmx, so the forwarded user reaches it like the pages.
//
// A fragment is bare HTML on the portal's origin, styled only by its hr-*
// classes. Unlike the pages it has no <base href>: every URL in it carries
// the prefix.

const (
	fragmentLimit   = 5
	fragmentRefresh = 10
)

type fragment struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	URL            string `json:"url"`
	RefreshSeconds int    `json:"refreshSeconds"`
}

type manifest struct {
	Name      string     `json:"name"`
	Title     string     `json:"title"`
	Version   string     `json:"version"`
	BasePath  string     `json:"basePath"`
	Fragments []fragment `json:"fragments"`
}

func (u *UI) prefix() string {
	return strings.TrimRight(u.BasePath, "/")
}

// manifest serves GET <prefix>/ui/manifest.json. It holds no order data, so
// it needs no login.
func (u *UI) manifest(w http.ResponseWriter, _ *http.Request) {
	m := manifest{
		Name:     "machinery-order-service",
		Title:    "Orders",
		Version:  version.Info()["version"],
		BasePath: u.prefix(),
		Fragments: []fragment{{
			ID: "orders", Title: "My orders",
			URL: u.prefix() + "/ui/fragment/orders", RefreshSeconds: fragmentRefresh,
		}},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(m); err != nil {
		slog.Debug("manifest", "error", err)
	}
}

type fragmentItem struct {
	ID, Name, Phase, Sev, Age string
}

type ordersFragment struct {
	Prefix  string
	SignIn  bool
	Err     string
	Open    int
	Ready   int
	Failed  int
	Items   []fragmentItem
	More    int
	AuthOff bool
}

// ordersFragment serves GET <prefix>/ui/fragment/orders: the caller's
// orders by state and the newest of them. htmx swaps 2xx only, so a missing
// login or a failed read is reported in the body with status 200.
func (u *UI) ordersFragment(w http.ResponseWriter, r *http.Request) {
	data := ordersFragment{Prefix: u.prefix(), AuthOff: u.API.Auth == nil}
	rec := &captured{header: http.Header{}}
	c, ok := u.API.Authorize(rec, r, true)
	if !ok {
		data.SignIn = true
		render(w, ordersFragmentTmpl, data)
		return
	}
	orders, err := u.API.List(r.Context(), filter(c))
	if err != nil {
		data.Err = err.Error()
		render(w, ordersFragmentTmpl, data)
		return
	}
	for i := range orders {
		o := &orders[i]
		switch o.Status.Phase {
		case v1alpha1.PhaseReady:
			data.Ready++
		case v1alpha1.PhaseFailed:
			data.Failed++
		case v1alpha1.PhaseCancelled, v1alpha1.PhaseDeleted:
		default:
			data.Open++
		}
		if len(data.Items) < fragmentLimit {
			data.Items = append(data.Items, item(o))
		}
	}
	data.More = len(orders) - len(data.Items)
	render(w, ordersFragmentTmpl, data)
}

// item is one order in the fragment, named by its resource (the name
// parameter) when it has one.
func item(o *v1alpha1.Order) fragmentItem {
	it := fragmentItem{ID: o.Name, Name: o.Name, Phase: string(o.Status.Phase), Sev: phaseSev(o.Status.Phase), Age: ago(o.CreationTimestamp.Time)}
	if it.Phase == "" {
		it.Phase = "Pending"
	}
	if p, err := intake.UserParameters(o); err == nil {
		if n, ok := p["name"].(string); ok && n != "" {
			it.Name = n
		}
	}
	return it
}

// phaseSev maps a phase to the portal's hr-sev-* classes.
func phaseSev(p v1alpha1.Phase) string {
	switch phaseClass(p) {
	case "ok":
		return "success"
	case "bad":
		return "error"
	case "muted":
		return "debug"
	}
	return "info"
}

var ordersFragmentTmpl = template.Must(template.New("fragment-orders").Parse(`<div class="hr-frag">
{{- if .SignIn}}<span class="hr-muted">Sign in to see your orders.</span>
{{- else if .Err}}<span class="hr-muted">Cannot read the orders: {{.Err}}</span>
{{- else}}<span class="hr-muted">{{if .AuthOff}}All orders{{else}}Your orders{{end}}: {{.Open}} open · {{.Ready}} ready · {{.Failed}} failed</span><ul class="hr-list">
{{- range .Items}}<li class="hr-item"><span class="hr-sev hr-sev-{{.Sev}}">{{.Phase}}</span><a class="hr-title" href="{{$.Prefix}}/ui/orders/{{.ID}}" title="{{.ID}}">{{.Name}}</a><span class="hr-time">{{.Age}}</span></li>
{{- else}}<li class="hr-item hr-muted">No orders yet</li>{{end}}
{{- if .More}}<li class="hr-item hr-muted">+{{.More}} more</li>{{end -}}
</ul>{{end -}}
<a class="hr-frag-more" href="{{.Prefix}}/ui/">Open orders →</a></div>`))
