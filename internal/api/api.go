// Package api is the service's HTTP API:
//
//	POST /api/v1/orders                 order (201), rendered with the caller's token
//	GET  /api/v1/orders                 list (?mine=true, ?phase=, ?cluster=)
//	GET  /api/v1/orders/watch           Server-Sent Events of order changes
//	GET  /api/v1/orders/{id}            one order
//	POST /api/v1/orders/{id}/cancel     cancel before the merge
//	GET  /health, /ready, /version
//
// With AUTH_OIDC_ISSUER set, every /api/v1/orders endpoint needs a ZITADEL
// JWT (internal/auth). The API answers at the root and under BASE_PATH.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/auth"
	"github.com/stuttgart-things/machinery-order-service/internal/intake"
	"github.com/stuttgart-things/machinery-order-service/internal/render"
	"github.com/stuttgart-things/machinery-order-service/internal/version"
)

// Server serves the API.
type Server struct {
	// Client reads (cached) and writes Orders and reads ManagementClusters.
	Client    client.Client
	Namespace string
	// Auth is nil when auth is off.
	Auth     *auth.Authenticator
	Renderer intake.Renderer
	Hub      *Hub
	BasePath string
	// Ready reports whether the caches have synced.
	Ready func() bool
	// Extra registers more routes under BasePath (the web UI).
	Extra func(mux *http.ServeMux, prefix string)
}

// CreateRequest is the body of POST /api/v1/orders.
type CreateRequest struct {
	Template          string         `json:"template"`
	Parameters        map[string]any `json:"parameters,omitempty"`
	ManagementCluster string         `json:"managementCluster"`
	Reason            string         `json:"reason,omitempty"`
	// RequestedBy is only taken when auth is off; with auth the login is.
	RequestedBy string `json:"requestedBy,omitempty"`
}

// CancelRequest is the body of POST /api/v1/orders/{id}/cancel.
type CancelRequest struct {
	// ResourceVersion, when set, must still be the order's: the cancel is
	// refused (409) when the order moved since the caller looked at it.
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// ErrorResponse is every error body; Fields name what is wrong per
// parameter.
type ErrorResponse struct {
	Error  string            `json:"error"`
	Fields map[string]string `json:"fields,omitempty"`
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	base := strings.TrimRight(s.BasePath, "/")
	prefixes := []string{""}
	if base != "" {
		prefixes = append(prefixes, base)
	}
	for _, p := range prefixes {
		mux.HandleFunc("POST "+p+"/api/v1/orders", s.create)
		mux.HandleFunc("GET "+p+"/api/v1/orders", s.list)
		mux.HandleFunc("GET "+p+"/api/v1/orders/watch", s.watch)
		mux.HandleFunc("GET "+p+"/api/v1/orders/{id}", s.get)
		mux.HandleFunc("POST "+p+"/api/v1/orders/{id}/cancel", s.cancel)
		mux.HandleFunc("GET "+p+"/health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
		mux.HandleFunc("GET "+p+"/ready", s.ready)
		mux.HandleFunc("GET "+p+"/version", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, version.Info()) })
	}
	if s.Extra != nil {
		s.Extra(mux, base)
	}
	return logRequests(mux)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if r.URL.Path != "/health" {
			slog.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start))
		}
	})
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(s int) { r.status = s; r.ResponseWriter.WriteHeader(s) }

// Flush keeps SSE working through the recorder.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}

func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	if s.Ready != nil && !s.Ready() {
		writeError(w, http.StatusServiceUnavailable, "caches not synced")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ready"})
}

// Caller is who calls: an identity when auth is on.
type Caller struct {
	ID    *auth.Identity
	Ref   string // user:default/<name>
	Token string // the bearer token, forwarded to claim-machinery-api
}

// Authorize checks the request. ui=true also accepts sthings-portal's
// forwarded identity (only for the web UI). It writes the error itself.
func (s *Server) Authorize(w http.ResponseWriter, r *http.Request, ui bool) (*Caller, bool) {
	c := &Caller{}
	if tok, ok := auth.BearerToken(r); ok {
		c.Token = tok
	}
	if s.Auth == nil {
		return c, true
	}
	var id auth.Identity
	var err error
	if ui {
		id, err = s.Auth.AuthorizeUI(r)
	} else {
		id, err = s.Auth.Authorize(r)
	}
	if err != nil {
		status := http.StatusUnauthorized
		switch {
		case errors.Is(err, auth.ErrForbidden):
			status = http.StatusForbidden
		case errors.Is(err, auth.ErrUnavailable):
			status = http.StatusServiceUnavailable
		}
		slog.Info("auth refused", "path", r.URL.Path, "status", status, "error", err)
		writeError(w, status, err.Error())
		return nil, false
	}
	ref, err := id.UserRef()
	if err != nil {
		writeError(w, http.StatusForbidden, "the login is not a Backstage user name")
		return nil, false
	}
	c.ID, c.Ref = &id, ref
	return c, true
}

// MayCancel reports whether the caller may cancel o: anyone without auth;
// with auth the orderer, a member of the order's owner group, or an admin.
func (s *Server) MayCancel(c *Caller, o *v1alpha1.Order) bool {
	if s.Auth == nil || c.ID == nil {
		return true
	}
	cfg := s.Auth.Config()
	if c.Ref == o.Spec.RequestedBy || cfg.IsAdmin(*c.ID) {
		return true
	}
	if rd := o.EffectiveRendered(); rd != nil && rd.Parameters != nil {
		var p map[string]any
		if json.Unmarshal(rd.Parameters.Raw, &p) == nil {
			if owner, _ := p["owner"].(string); owner != "" {
				return owner == c.Ref || slices.Contains(cfg.Groups(*c.ID), owner)
			}
		}
	}
	return false
}

// Create validates, renders with the caller's token and creates the Order.
// It returns the HTTP status and body for the API; the UI uses it as well.
func (s *Server) Create(ctx context.Context, c *Caller, req CreateRequest) (int, any) {
	if req.Template == "" || req.ManagementCluster == "" {
		return http.StatusBadRequest, ErrorResponse{Error: "template and managementCluster are required"}
	}
	if len(req.Reason) > 500 {
		return http.StatusBadRequest, ErrorResponse{Error: "reason: at most 500 characters"}
	}
	mc := &v1alpha1.ManagementCluster{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: req.ManagementCluster}, mc); err != nil {
		if apierrors.IsNotFound(err) {
			return http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("unknown management cluster %q", req.ManagementCluster)}
		}
		return http.StatusInternalServerError, ErrorResponse{Error: err.Error()}
	}
	if mc.Spec.Cordoned {
		return http.StatusConflict, ErrorResponse{Error: fmt.Sprintf("management cluster %s is cordoned", mc.Name)}
	}
	requestedBy := req.RequestedBy
	if c.ID != nil {
		requestedBy = c.Ref
	}
	params := req.Parameters
	if params == nil {
		params = map[string]any{}
	}
	raw, err := intake.Encode(params)
	if err != nil {
		return http.StatusBadRequest, ErrorResponse{Error: err.Error()}
	}
	id := intake.NewOrderID()
	o := &v1alpha1.Order{
		ObjectMeta: metav1.ObjectMeta{
			Name: id, Namespace: s.Namespace,
			Labels: map[string]string{v1alpha1.OrderIDKey: id},
		},
		Spec: v1alpha1.OrderSpec{
			Template: req.Template, Parameters: raw, RequestedBy: requestedBy, Reason: req.Reason,
			Placement: v1alpha1.Placement{ManagementCluster: mc.Name},
			Delivery:  v1alpha1.DeliveryPullRequest,
		},
	}
	if requestedBy != "" {
		o.Annotations = map[string]string{v1alpha1.RequestedByKey: requestedBy}
	}
	rd, err := intake.Render(ctx, s.Renderer, o, mc, c.Token)
	if err != nil {
		var ie *intake.InvalidError
		var ae *render.APIError
		switch {
		case errors.As(err, &ie):
			return http.StatusBadRequest, ErrorResponse{Error: ie.Msg}
		case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
			return http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("unknown template %q", req.Template)}
		case errors.As(err, &ae) && ae.Status/100 == 4:
			return ae.Status, ErrorResponse{Error: ae.Message, Fields: ae.Fields}
		}
		slog.Error("render failed", "template", req.Template, "error", err)
		return http.StatusBadGateway, ErrorResponse{Error: err.Error()}
	}
	o.Spec.Rendered = rd
	if err := s.Client.Create(ctx, o); err != nil {
		return http.StatusInternalServerError, ErrorResponse{Error: err.Error()}
	}
	slog.Info("order accepted", "order", id, "template", req.Template, "cluster", mc.Name, "requestedBy", requestedBy)
	return http.StatusCreated, typed(o)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	c, ok := s.Authorize(w, r, false)
	if !ok {
		return
	}
	var req CreateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	status, body := s.Create(r.Context(), c, req)
	writeJSON(w, status, body)
}

// typed sets the TypeMeta the cached objects lack.
func typed(o *v1alpha1.Order) *v1alpha1.Order {
	o = o.DeepCopy()
	o.APIVersion = v1alpha1.GroupVersion.String()
	o.Kind = "Order"
	o.ManagedFields = nil
	return o
}

// summary drops the rendered files, for lists and the watch.
func summary(o *v1alpha1.Order) *v1alpha1.Order {
	o = typed(o)
	o.Spec.Rendered = nil
	o.Status.Rendered = nil
	return o
}

// Filter selects orders for a list or a watch.
type Filter struct {
	RequestedBy string
	Phase       string
	Cluster     string
}

// Match reports whether o passes the filter.
func (f Filter) Match(o *v1alpha1.Order) bool {
	return (f.RequestedBy == "" || o.Spec.RequestedBy == f.RequestedBy) &&
		(f.Phase == "" || string(o.Status.Phase) == f.Phase) &&
		(f.Cluster == "" || o.Spec.Placement.ManagementCluster == f.Cluster)
}

func (s *Server) filter(w http.ResponseWriter, r *http.Request, c *Caller) (Filter, bool) {
	q := r.URL.Query()
	f := Filter{Phase: q.Get("phase"), Cluster: q.Get("cluster")}
	if q.Get("mine") == "true" {
		if c.Ref == "" {
			writeError(w, http.StatusBadRequest, "mine=true needs a login (auth is off)")
			return f, false
		}
		f.RequestedBy = c.Ref
	}
	return f, true
}

// List returns the orders that pass f, newest first.
func (s *Server) List(ctx context.Context, f Filter) ([]v1alpha1.Order, error) {
	var list v1alpha1.OrderList
	if err := s.Client.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	out := make([]v1alpha1.Order, 0, len(list.Items))
	for i := range list.Items {
		if f.Match(&list.Items[i]) {
			out = append(out, *summary(&list.Items[i]))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[j].CreationTimestamp.Before(&out[i].CreationTimestamp)
	})
	return out, nil
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	c, ok := s.Authorize(w, r, false)
	if !ok {
		return
	}
	f, ok := s.filter(w, r, c)
	if !ok {
		return
	}
	items, err := s.List(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, 200, v1alpha1.OrderList{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "OrderList"},
		Items:    items,
	})
}

// Get reads one order.
func (s *Server) Get(ctx context.Context, id string) (*v1alpha1.Order, error) {
	o := &v1alpha1.Order{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: id}, o); err != nil {
		return nil, err
	}
	return o, nil
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.Authorize(w, r, false); !ok {
		return
	}
	o, err := s.Get(r.Context(), r.PathValue("id"))
	if apierrors.IsNotFound(err) {
		writeError(w, http.StatusNotFound, "order not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, 200, typed(o))
}

// Cancel asks to cancel order id; resourceVersion, when set, must still be
// the order's.
func (s *Server) Cancel(ctx context.Context, c *Caller, id, resourceVersion string) (int, any) {
	o, err := s.Get(ctx, id)
	if apierrors.IsNotFound(err) {
		return http.StatusNotFound, ErrorResponse{Error: "order not found"}
	}
	if err != nil {
		return http.StatusInternalServerError, ErrorResponse{Error: err.Error()}
	}
	if !s.MayCancel(c, o) {
		return http.StatusForbidden, ErrorResponse{Error: "forbidden: only the orderer, its owner group or an admin may cancel"}
	}
	if resourceVersion != "" && resourceVersion != o.ResourceVersion {
		return http.StatusConflict, ErrorResponse{Error: "the order changed since it was read; reload and try again"}
	}
	switch {
	case o.Status.Phase == v1alpha1.PhaseCancelled:
		return http.StatusOK, typed(o)
	case o.Status.Phase.Terminal():
		return http.StatusConflict, ErrorResponse{Error: fmt.Sprintf("the order is %s", o.Status.Phase)}
	case !o.Status.Phase.BeforeMerge():
		return http.StatusConflict, ErrorResponse{Error: "the order is merged: cancelling is not possible, removing it needs a teardown"}
	}
	o.Spec.Cancel = true
	if resourceVersion != "" {
		o.ResourceVersion = resourceVersion
	}
	if err := s.Client.Update(ctx, o); err != nil {
		if apierrors.IsConflict(err) {
			return http.StatusConflict, ErrorResponse{Error: "the order changed since it was read; reload and try again"}
		}
		return http.StatusInternalServerError, ErrorResponse{Error: err.Error()}
	}
	slog.Info("order cancel requested", "order", id, "by", c.Ref)
	return http.StatusAccepted, typed(o)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	c, ok := s.Authorize(w, r, false)
	if !ok {
		return
	}
	var req CancelRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
	}
	status, body := s.Cancel(r.Context(), c, r.PathValue("id"), req.ResourceVersion)
	writeJSON(w, status, body)
}

// Stream writes order events as Server-Sent Events until the client leaves.
// render turns an event into the data line(s); nil sends the order as JSON.
func (s *Server) Stream(w http.ResponseWriter, r *http.Request, f Filter, name string, renderEvent func(Event) (string, bool)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	if s.Hub == nil {
		writeError(w, http.StatusServiceUnavailable, "watch unavailable")
		return
	}
	ch, cancel := s.Hub.Subscribe()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, ": watching orders\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			if !f.Match(e.Order) {
				continue
			}
			var data string
			if renderEvent != nil {
				d, ok := renderEvent(e)
				if !ok {
					continue
				}
				data = d
			} else {
				raw, err := json.Marshal(Event{Type: e.Type, Order: summary(e.Order)})
				if err != nil {
					continue
				}
				data = string(raw)
			}
			_, _ = fmt.Fprintf(w, "event: %s\n", name)
			for _, line := range strings.Split(data, "\n") {
				_, _ = fmt.Fprintf(w, "data: %s\n", line)
			}
			_, _ = fmt.Fprint(w, "\n")
			flusher.Flush()
		}
	}
}

func (s *Server) watch(w http.ResponseWriter, r *http.Request) {
	c, ok := s.Authorize(w, r, false)
	if !ok {
		return
	}
	f, ok := s.filter(w, r, c)
	if !ok {
		return
	}
	s.Stream(w, r, f, "order", nil)
}
