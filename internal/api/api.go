// Package api is the service's HTTP API:
//
//	POST /api/v1/orders                 order (201), rendered with the caller's token
//	GET  /api/v1/orders                 list (?mine=true, ?phase=, ?cluster=)
//	GET  /api/v1/orders/watch           Server-Sent Events of order changes
//	GET  /api/v1/orders/{id}            one order
//	POST /api/v1/orders/{id}/cancel     cancel before the merge
//	POST /api/v1/orders/{id}/teardown   remove what a merged order built (ADR 0003)
//	GET  /api/v1/management-clusters    where orders can go
//	GET  /health, /ready, /version
//
// With AUTH_OIDC_ISSUER set, every /api/v1 endpoint needs a ZITADEL JWT
// (internal/auth). A caller sees the orders it placed and those owned by one
// of its groups; admins see all (docs/adr/0001-order-visibility.md). The API
// answers at the root and under BASE_PATH.
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
	"github.com/stuttgart-things/machinery-order-service/internal/automerge"
	"github.com/stuttgart-things/machinery-order-service/internal/capabilities"
	"github.com/stuttgart-things/machinery-order-service/internal/clusterhealth"
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
	// AutoMerge asks the service to merge the pull request once its checks
	// pass. Refused (403) unless a rule of the management cluster allows it
	// (docs/adr/0002-auto-merge.md).
	AutoMerge bool `json:"autoMerge,omitempty"`
}

// CancelRequest is the body of POST /api/v1/orders/{id}/cancel.
type CancelRequest struct {
	// ResourceVersion, when set, must still be the order's: the cancel is
	// refused (409) when the order moved since the caller looked at it.
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// TeardownRequest is the body of POST /api/v1/orders/{id}/teardown
// (docs/adr/0003-teardown.md).
type TeardownRequest struct {
	Reason string `json:"reason,omitempty"`
	// AutoMerge asks the service to merge the teardown's pull request once
	// its checks pass. Refused (403) unless the caller placed the order and a
	// rule of the management cluster allows teardowns for it.
	AutoMerge bool `json:"autoMerge,omitempty"`
	// ResourceVersion, when set, must still be the order's (409 otherwise).
	ResourceVersion string `json:"resourceVersion,omitempty"`
	// RequestedBy is only taken when auth is off; with auth the login is.
	RequestedBy string `json:"requestedBy,omitempty"`
}

// ErrChanged is the error of a 409 because the order moved since the caller
// read it (resourceVersion); other 409s refuse the request as such.
const ErrChanged = "the order changed since it was read; reload and try again"

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
		mux.HandleFunc("POST "+p+"/api/v1/orders/{id}/teardown", s.teardown)
		mux.HandleFunc("GET "+p+"/api/v1/management-clusters", s.clusters)
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
	c, status, err := s.Identify(r, ui)
	if err != nil {
		slog.Info("auth refused", "path", r.URL.Path, "status", status, "error", err)
		writeError(w, status, err.Error())
		return nil, false
	}
	return c, true
}

// Identify checks the request's credentials, as Authorize, and returns the
// HTTP status of a refusal instead of writing it. The gRPC API calls it with
// the token of its metadata.
func (s *Server) Identify(r *http.Request, ui bool) (*Caller, int, error) {
	c := &Caller{}
	if tok, ok := auth.BearerToken(r); ok {
		c.Token = tok
	}
	if s.Auth == nil {
		return c, http.StatusOK, nil
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
		return nil, status, err
	}
	ref, err := id.UserRef()
	if err != nil {
		return nil, http.StatusForbidden, errors.New("the login is not a Backstage user name")
	}
	c.ID, c.Ref = &id, ref
	return c, http.StatusOK, nil
}

// MaySee reports whether the caller may see o: anyone without auth; with
// auth the orderer, the order's owner (the caller or one of its groups), or
// an admin (docs/adr/0001-order-visibility.md).
func (s *Server) MaySee(c *Caller, o *v1alpha1.Order) bool {
	if s.Auth == nil || c.ID == nil {
		return true
	}
	cfg := s.Auth.Config()
	if c.Ref == o.Spec.RequestedBy || cfg.IsAdmin(*c.ID) {
		return true
	}
	owner := Owner(o)
	return owner != "" && (owner == c.Ref || slices.Contains(cfg.Groups(*c.ID), owner))
}

// MayCancel reports whether the caller may cancel o: whoever may see it.
func (s *Server) MayCancel(c *Caller, o *v1alpha1.Order) bool { return s.MaySee(c, o) }

// MayTearDown reports whether the caller may tear o down: whoever may see it
// (docs/adr/0003-teardown.md).
func (s *Server) MayTearDown(c *Caller, o *v1alpha1.Order) bool { return s.MaySee(c, o) }

// TeardownRefused says why o cannot be torn down now, "" when it can.
func TeardownRefused(o *v1alpha1.Order) string {
	switch {
	case o.TornDown():
		return "the order is torn down already"
	case o.TeardownRunning():
		return "a teardown of the order is running"
	case o.Status.Phase.BeforeMerge():
		return "the order is not merged yet: cancel it instead"
	case !o.Status.Phase.Teardownable():
		return fmt.Sprintf("the order is %s", o.Status.Phase)
	}
	return ""
}

// Teardown asks to tear order id down: the controller opens a delete-claim
// pull request (docs/adr/0003-teardown.md).
func (s *Server) Teardown(ctx context.Context, c *Caller, id string, req TeardownRequest) (int, any) {
	if len(req.Reason) > 500 {
		return http.StatusBadRequest, ErrorResponse{Error: "reason: at most 500 characters"}
	}
	o, err := s.Get(ctx, id)
	if apierrors.IsNotFound(err) {
		return http.StatusNotFound, ErrorResponse{Error: "order not found"}
	}
	if err != nil {
		return http.StatusInternalServerError, ErrorResponse{Error: err.Error()}
	}
	if !s.MayTearDown(c, o) {
		return http.StatusNotFound, ErrorResponse{Error: "order not found"}
	}
	if req.ResourceVersion != "" && req.ResourceVersion != o.ResourceVersion {
		return http.StatusConflict, ErrorResponse{Error: ErrChanged}
	}
	if why := TeardownRefused(o); why != "" {
		return http.StatusConflict, ErrorResponse{Error: "teardown refused: " + why}
	}
	by := req.RequestedBy
	if c.ID != nil {
		by = c.Ref
	}
	attempt := 1
	if o.Spec.Teardown != nil {
		attempt = o.Spec.Teardown.Attempt + 1
	}
	now := metav1.Now()
	t := &v1alpha1.TeardownRequest{Attempt: attempt, RequestedBy: by, RequestedAt: &now, Reason: req.Reason}
	if req.AutoMerge {
		mc := &v1alpha1.ManagementCluster{}
		if err := s.Client.Get(ctx, types.NamespacedName{Name: o.Spec.Placement.ManagementCluster}, mc); err != nil {
			return http.StatusInternalServerError, ErrorResponse{Error: err.Error()}
		}
		if by == "" || by != o.Spec.RequestedBy {
			return http.StatusForbidden, ErrorResponse{Error: fmt.Sprintf(
				"auto-merge of a teardown is only for the orderer (%s); tear down without autoMerge and a human merges", orNobody(o.Spec.RequestedBy))}
		}
		var groups []string
		if c.ID != nil && s.Auth != nil {
			groups = s.Auth.Config().Groups(*c.ID)
		}
		rule, why := automerge.Find(mc.Spec.AutoMerge, v1alpha1.ActionTeardown, by, groups, o.Spec.Template, automerge.Params(o.EffectiveRendered()))
		if rule == nil {
			return http.StatusForbidden, ErrorResponse{Error: fmt.Sprintf(
				"auto-merge of a teardown not allowed on %s: %s. Tear down without autoMerge and a human merges (rules: GET /api/v1/management-clusters)", mc.Name, why)}
		}
		t.AutoMerge = &v1alpha1.OrderAutoMerge{Rule: rule.Name}
	}
	o.Spec.Teardown = t
	if req.ResourceVersion != "" {
		o.ResourceVersion = req.ResourceVersion
	}
	if err := s.Client.Update(ctx, o); err != nil {
		if apierrors.IsConflict(err) {
			return http.StatusConflict, ErrorResponse{Error: ErrChanged}
		}
		return http.StatusInternalServerError, ErrorResponse{Error: err.Error()}
	}
	slog.Info("order teardown requested", "order", id, "by", by, "attempt", attempt, "autoMerge", t.AutoMerge != nil)
	return http.StatusAccepted, typed(o)
}

func (s *Server) teardown(w http.ResponseWriter, r *http.Request) {
	c, ok := s.Authorize(w, r, false)
	if !ok {
		return
	}
	var req TeardownRequest
	if r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
	}
	status, body := s.Teardown(r.Context(), c, r.PathValue("id"), req)
	writeJSON(w, status, body)
}

func orNobody(s string) string {
	if s == "" {
		return "nobody recorded"
	}
	return s
}

// Owner is the order's owner parameter (user:… or group:…), "" if none.
func Owner(o *v1alpha1.Order) string {
	if rd := o.EffectiveRendered(); rd != nil && rd.Parameters != nil {
		var p map[string]any
		if json.Unmarshal(rd.Parameters.Raw, &p) == nil {
			owner, _ := p["owner"].(string)
			return owner
		}
	}
	return ""
}

// visibleTo is the visibility part of a filter: nil when the caller sees
// everything (auth off, admin).
// IsAdmin reports whether the caller sees every order.
func (s *Server) IsAdmin(c *Caller) bool {
	return s.Auth == nil || c.ID == nil || s.Auth.Config().IsAdmin(*c.ID)
}

// VisibleTo is the visibility part of a filter: nil when the caller sees
// everything (auth off, admin).
func (s *Server) VisibleTo(c *Caller) func(*v1alpha1.Order) bool {
	return s.visibleTo(c)
}

func (s *Server) visibleTo(c *Caller) func(*v1alpha1.Order) bool {
	if s.Auth == nil || c.ID == nil || s.Auth.Config().IsAdmin(*c.ID) {
		return nil
	}
	return func(o *v1alpha1.Order) bool { return s.MaySee(c, o) }
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
			msg := fmt.Sprintf("unknown management cluster %q", req.ManagementCluster)
			if names, lerr := s.clusterNames(ctx); lerr == nil && len(names) > 0 {
				msg += "; valid: " + strings.Join(names, ", ") + " (GET /api/v1/management-clusters)"
			}
			return http.StatusBadRequest, ErrorResponse{Error: msg}
		}
		return http.StatusInternalServerError, ErrorResponse{Error: err.Error()}
	}
	if mc.Spec.Cordoned {
		return http.StatusConflict, ErrorResponse{Error: fmt.Sprintf("management cluster %s is cordoned", mc.Name)}
	}
	if blocked, why := clusterhealth.Blocking(mc, time.Now()); blocked {
		return http.StatusConflict, ErrorResponse{Error: why}
	}
	requestedBy := req.RequestedBy
	var requestedByGroups []string
	if c.ID != nil {
		requestedBy = c.Ref
		if s.Auth != nil {
			requestedByGroups = s.Auth.Config().Groups(*c.ID)
		}
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
			Template: req.Template, Parameters: raw, RequestedBy: requestedBy, RequestedByGroups: requestedByGroups, Reason: req.Reason,
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
	if xr, err := intake.XRRef(rd); err == nil {
		if why := capabilities.Check(mc, capabilities.XR{APIVersion: xr.APIVersion, Kind: xr.Kind, Environment: intake.XREnvironment(rd)}, time.Now()); why != "" {
			return http.StatusBadRequest, ErrorResponse{Error: why, Fields: map[string]string{"managementCluster": why}}
		}
	}
	if req.AutoMerge {
		rule, why := automerge.Find(mc.Spec.AutoMerge, v1alpha1.ActionOrder, requestedBy, requestedByGroups, req.Template, automerge.Params(rd))
		if rule == nil {
			return http.StatusForbidden, ErrorResponse{Error: fmt.Sprintf(
				"auto-merge not allowed on %s: %s. Order without autoMerge and a human merges (rules: GET /api/v1/management-clusters)", mc.Name, why)}
		}
		o.Spec.AutoMerge = &v1alpha1.OrderAutoMerge{Rule: rule.Name}
		if ml := rule.MaxLifetime; ml != nil && ml.Duration > 0 {
			// The rule's lifetime counts from the intake; the controller
			// tears the order down once it is over (#27).
			exp := metav1.NewTime(time.Now().Add(ml.Duration).Truncate(time.Second))
			o.Spec.AutoMerge.ExpiresAt = &exp
		}
	}
	if err := s.Client.Create(ctx, o); err != nil {
		return http.StatusInternalServerError, ErrorResponse{Error: err.Error()}
	}
	slog.Info("order accepted", "order", id, "template", req.Template, "cluster", mc.Name, "requestedBy", requestedBy, "autoMerge", req.AutoMerge)
	s.markAccepted(ctx, o)
	return http.StatusCreated, typed(o)
}

// AcceptedMessage is the status message of a fresh order.
const AcceptedMessage = "accepted: rendered; the pull request follows (GET /api/v1/orders/{id} or /api/v1/orders/watch)"

// markAccepted sets the first phase right away, so the answer to the create
// (and a GET right after it) says the order was taken and what comes next
// (#14). The controller does the same when it gets there first; a conflict
// means it did, and the order is read again.
func (s *Server) markAccepted(ctx context.Context, o *v1alpha1.Order) {
	o.Status.OrderID = o.Name
	o.Status.Phase = v1alpha1.PhaseOrdered
	o.Status.Message = AcceptedMessage
	o.Status.History = []v1alpha1.PhaseTransition{{Phase: v1alpha1.PhaseOrdered, Time: metav1.Now(), Message: AcceptedMessage}}
	err := s.Client.Status().Update(ctx, o)
	if err == nil {
		return
	}
	if fresh, gerr := s.Get(ctx, o.Name); gerr == nil && fresh.Status.Phase != "" {
		*o = *fresh
		return
	}
	slog.Info("order status not set at intake, the controller will", "order", o.Name, "error", err)
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
	if o, ok := body.(*v1alpha1.Order); ok && status == http.StatusCreated {
		w.Header().Set("Location", strings.TrimRight(s.BasePath, "/")+"/api/v1/orders/"+o.Name)
	}
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

// NoGroup is the Filter.Group of orders with neither an owner nor recorded
// orderer groups.
const NoGroup = "-"

// Groups are what an order belongs to, for grouping in the UI: the owner
// parameter, else the orderer's groups at order time. Read from the full
// order (the owner is in the rendered parameters, which summary drops).
func Groups(o *v1alpha1.Order) []string {
	if owner := Owner(o); owner != "" {
		return []string{owner}
	}
	return o.Spec.RequestedByGroups
}

// Row is an order for a list, with what the summary drops.
type Row struct {
	Order *v1alpha1.Order
	// Owner is the owner parameter ("" when none).
	Owner string
	// Groups as Groups(o).
	Groups []string
}

// RowOf builds the Row of a full order.
func RowOf(o *v1alpha1.Order) Row {
	return Row{Order: summary(o), Owner: Owner(o), Groups: Groups(o)}
}

// Filter selects orders for a list or a watch.
type Filter struct {
	RequestedBy string
	Phase       string
	Cluster     string
	// Group keeps the orders whose Groups contain it; NoGroup keeps those
	// without any.
	Group string
	// Visible, when set, hides the orders the caller may not see.
	Visible func(*v1alpha1.Order) bool
}

// Match reports whether o passes the filter.
func (f Filter) Match(o *v1alpha1.Order) bool {
	return (f.Visible == nil || f.Visible(o)) &&
		(f.RequestedBy == "" || o.Spec.RequestedBy == f.RequestedBy) &&
		(f.Phase == "" || string(o.Status.Phase) == f.Phase) &&
		(f.Cluster == "" || o.Spec.Placement.ManagementCluster == f.Cluster) &&
		(f.Group == "" || (f.Group == NoGroup && len(Groups(o)) == 0) || slices.Contains(Groups(o), f.Group))
}

func (s *Server) filter(w http.ResponseWriter, r *http.Request, c *Caller) (Filter, bool) {
	q := r.URL.Query()
	f := Filter{Phase: q.Get("phase"), Cluster: q.Get("cluster"), Visible: s.visibleTo(c)}
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
	rows, err := s.ListRows(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]v1alpha1.Order, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r.Order)
	}
	return out, nil
}

// ListRows is List with each order's owner and groups.
func (s *Server) ListRows(ctx context.Context, f Filter) ([]Row, error) {
	var list v1alpha1.OrderList
	if err := s.Client.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(list.Items))
	for i := range list.Items {
		if f.Match(&list.Items[i]) {
			out = append(out, RowOf(&list.Items[i]))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[j].Order.CreationTimestamp.Before(&out[i].Order.CreationTimestamp)
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
	c, ok := s.Authorize(w, r, false)
	if !ok {
		return
	}
	o, err := s.Get(r.Context(), r.PathValue("id"))
	if apierrors.IsNotFound(err) || (err == nil && !s.MaySee(c, o)) {
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
	if !s.MaySee(c, o) {
		// not visible = not there for this caller (no hint that it exists)
		return http.StatusNotFound, ErrorResponse{Error: "order not found"}
	}
	if resourceVersion != "" && resourceVersion != o.ResourceVersion {
		return http.StatusConflict, ErrorResponse{Error: ErrChanged}
	}
	switch {
	case o.Status.Phase == v1alpha1.PhaseCancelled:
		return http.StatusOK, typed(o)
	case o.Status.Phase.Terminal():
		return http.StatusConflict, ErrorResponse{Error: fmt.Sprintf("the order is %s", o.Status.Phase)}
	case !o.Status.Phase.BeforeMerge():
		return http.StatusConflict, ErrorResponse{Error: "the order is merged: cancelling is not possible, tear it down instead (POST /api/v1/orders/{id}/teardown)"}
	}
	o.Spec.Cancel = true
	if resourceVersion != "" {
		o.ResourceVersion = resourceVersion
	}
	if err := s.Client.Update(ctx, o); err != nil {
		if apierrors.IsConflict(err) {
			return http.StatusConflict, ErrorResponse{Error: ErrChanged}
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

// ClusterInfo is one entry of GET /api/v1/management-clusters: what an
// orderer needs to pick a cluster, nothing about how the service reaches it.
type ClusterInfo struct {
	Name     string `json:"name"`
	Lab      string `json:"lab,omitempty"`
	Cordoned bool   `json:"cordoned"`
	// Repository and OrdersPath are where the cluster's orders land in git.
	Repository string `json:"repository,omitempty"`
	OrdersPath string `json:"ordersPath,omitempty"`
	// AutoMerge lists who may order with autoMerge, and what.
	AutoMerge *v1alpha1.AutoMerge `json:"autoMerge,omitempty"`
	// Ready is True, False or Unknown (no machinery, not checked yet, or
	// the last check is stale). False refuses orders like cordoned (#31).
	Ready        string `json:"ready"`
	ReadyMessage string `json:"readyMessage,omitempty"`
	// Conditions are the cluster's health conditions as last checked.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	CheckedAt  *metav1.Time       `json:"checkedAt,omitempty"`
	// Capabilities are the kinds and EnvironmentConfigs the cluster has (#32).
	Capabilities *v1alpha1.Capabilities `json:"capabilities,omitempty"`
}

// Clusters lists the management clusters, by name.
func (s *Server) Clusters(ctx context.Context) ([]ClusterInfo, error) {
	var list v1alpha1.ManagementClusterList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make([]ClusterInfo, 0, len(list.Items))
	now := time.Now()
	for i := range list.Items {
		mc := &list.Items[i]
		ready, msg := clusterhealth.Effective(mc, now, clusterhealth.MaxAge)
		out = append(out, ClusterInfo{
			Name: mc.Name, Lab: mc.Spec.Lab, Cordoned: mc.Spec.Cordoned,
			Repository: mc.Spec.GitOps.Repository, OrdersPath: mc.Spec.GitOps.OrdersPath,
			AutoMerge: mc.Spec.AutoMerge,
			Ready:     string(ready), ReadyMessage: msg,
			Conditions: mc.Status.Conditions, CheckedAt: mc.Status.CheckedAt,
			Capabilities: mc.Status.Capabilities,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// clusterNames are the names of the clusters that take orders.
func (s *Server) clusterNames(ctx context.Context) ([]string, error) {
	cs, err := s.Clusters(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, c := range cs {
		if !c.Cordoned && c.Ready != string(metav1.ConditionFalse) {
			names = append(names, c.Name)
		}
	}
	return names, nil
}

func (s *Server) clusters(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.Authorize(w, r, false); !ok {
		return
	}
	cs, err := s.Clusters(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": cs})
}
