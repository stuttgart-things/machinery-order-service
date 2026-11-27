// Package grpcapi is the service's gRPC API (api/proto/order/v1, #24). It
// calls the same service layer as the REST API (internal/api): validation
// and rendering at intake, visibility (ADR 0001), auto-merge rules (ADR 0002)
// and teardown (ADR 0003). The watch hangs on the same Hub.
//
// Auth is the REST API's: a ZITADEL bearer token in the "authorization"
// metadata, checked against the same configuration. Health
// (grpc.health.v1) and reflection need none.
package grpcapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	orderv1 "github.com/stuttgart-things/machinery-order-service/api/proto/order/v1"
	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/api"
)

// Server implements orderv1.OrderServiceServer on top of api.Server.
type Server struct {
	orderv1.UnimplementedOrderServiceServer
	API *api.Server
}

// New returns a gRPC server with the order service, health and reflection,
// and the auth interceptors.
func New(a *api.Server) *grpc.Server {
	s := &Server{API: a}
	g := grpc.NewServer(grpc.ChainUnaryInterceptor(s.unaryAuth), grpc.ChainStreamInterceptor(s.streamAuth))
	orderv1.RegisterOrderServiceServer(g, s)
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	hs.SetServingStatus(orderv1.OrderService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(g, hs)
	reflection.Register(g)
	return g
}

type callerKey struct{}

// needsAuth reports whether the method is the order service's: health and
// reflection are open.
func needsAuth(fullMethod string) bool {
	return strings.HasPrefix(fullMethod, "/"+orderv1.OrderService_ServiceDesc.ServiceName+"/")
}

// authenticate checks the "authorization" metadata the way the REST API
// checks the header and puts the caller into the context.
func (s *Server) authenticate(ctx context.Context, fullMethod string) (context.Context, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, fullMethod, nil)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("authorization"); len(v) > 0 {
			r.Header.Set("Authorization", v[0])
		}
	}
	c, code, err := s.API.Identify(r, false)
	if err != nil {
		slog.Info("grpc auth refused", "method", fullMethod, "status", code, "error", err)
		return nil, status.Error(httpCode(code), err.Error())
	}
	return context.WithValue(ctx, callerKey{}, c), nil
}

func (s *Server) unaryAuth(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if !needsAuth(info.FullMethod) {
		return handler(ctx, req)
	}
	ctx, err := s.authenticate(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

type authedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (a authedStream) Context() context.Context { return a.ctx }

func (s *Server) streamAuth(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if !needsAuth(info.FullMethod) {
		return handler(srv, ss)
	}
	ctx, err := s.authenticate(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	return handler(srv, authedStream{ServerStream: ss, ctx: ctx})
}

func caller(ctx context.Context) *api.Caller {
	if c, ok := ctx.Value(callerKey{}).(*api.Caller); ok {
		return c
	}
	return &api.Caller{}
}

// httpCode maps the REST API's status to a gRPC code.
func httpCode(code int) codes.Code {
	switch code {
	case http.StatusBadRequest:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusConflict:
		return codes.FailedPrecondition
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return codes.Unavailable
	}
	if code/100 == 4 {
		return codes.InvalidArgument
	}
	return codes.Internal
}

// result turns the REST API's (status, body) into the order or a gRPC error.
// A 400 carries claim-machinery-api's fields as BadRequest field violations;
// a 409 for an order that moved since it was read is Aborted.
func result(code int, body any, withRendered bool) (*orderv1.Order, error) {
	if o, ok := body.(*v1alpha1.Order); ok && code/100 == 2 {
		return toOrder(o, withRendered), nil
	}
	e, _ := body.(api.ErrorResponse)
	c := httpCode(code)
	if code == http.StatusConflict && e.Error == api.ErrChanged {
		c = codes.Aborted
	}
	st := status.New(c, e.Error)
	if len(e.Fields) > 0 {
		br := &errdetails.BadRequest{}
		keys := make([]string, 0, len(e.Fields))
		for k := range e.Fields {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			br.FieldViolations = append(br.FieldViolations, &errdetails.BadRequest_FieldViolation{Field: k, Description: e.Fields[k]})
		}
		if d, err := st.WithDetails(br); err == nil {
			st = d
		}
	}
	return nil, st.Err()
}

// CreateOrder implements orderv1.OrderServiceServer.
func (s *Server) CreateOrder(ctx context.Context, req *orderv1.CreateOrderRequest) (*orderv1.Order, error) {
	code, body := s.API.Create(ctx, caller(ctx), api.CreateRequest{
		Template: req.GetTemplate(), ManagementCluster: req.GetManagementCluster(),
		Parameters: req.GetParameters().AsMap(), Reason: req.GetReason(), AutoMerge: req.GetAutoMerge(),
	})
	return result(code, body, true)
}

// GetOrder implements orderv1.OrderServiceServer.
func (s *Server) GetOrder(ctx context.Context, req *orderv1.GetOrderRequest) (*orderv1.Order, error) {
	o, err := s.API.Get(ctx, req.GetId())
	if apierrors.IsNotFound(err) || (err == nil && !s.API.MaySee(caller(ctx), o)) {
		return nil, status.Error(codes.NotFound, "order not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return toOrder(o, true), nil
}

// filter is the REST API's filter for the request.
func (s *Server) filter(c *api.Caller, f *orderv1.OrderFilter) (api.Filter, error) {
	out := api.Filter{Phase: f.GetPhase(), Cluster: f.GetCluster(), Visible: s.API.VisibleTo(c)}
	if f.GetMine() {
		if c.Ref == "" {
			return out, status.Error(codes.InvalidArgument, "mine needs a login (auth is off)")
		}
		out.RequestedBy = c.Ref
	}
	return out, nil
}

// ListOrders implements orderv1.OrderServiceServer.
func (s *Server) ListOrders(ctx context.Context, req *orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error) {
	f, err := s.filter(caller(ctx), req.GetFilter())
	if err != nil {
		return nil, err
	}
	items, err := s.API.List(ctx, f)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &orderv1.ListOrdersResponse{Orders: make([]*orderv1.Order, 0, len(items))}
	for i := range items {
		out.Orders = append(out.Orders, toOrder(&items[i], false))
	}
	return out, nil
}

// newer reports whether resource version rv is after since; versions that
// are not numbers count as newer (sent rather than lost).
func newer(rv, since string) bool {
	a, errA := strconv.ParseUint(rv, 10, 64)
	b, errB := strconv.ParseUint(since, 10, 64)
	return errA != nil || errB != nil || a > b
}

// WatchOrders implements orderv1.OrderServiceServer.
func (s *Server) WatchOrders(req *orderv1.WatchOrdersRequest, stream grpc.ServerStreamingServer[orderv1.OrderEvent]) error {
	ctx := stream.Context()
	c := caller(ctx)
	f, err := s.filter(c, req.GetFilter())
	if err != nil {
		return err
	}
	if s.API.Hub == nil {
		return status.Error(codes.Unavailable, "watch unavailable")
	}
	id := req.GetOrderId()
	if id != "" {
		visible := f.Visible
		f.Visible = func(o *v1alpha1.Order) bool { return o.Name == id && (visible == nil || visible(o)) }
	}
	until := req.GetUntilPhases()
	done := func(o *v1alpha1.Order) bool {
		return id != "" && (o.Status.Phase.Terminal() || slices.Contains(until, string(o.Status.Phase)))
	}
	send := func(t string, o *v1alpha1.Order) error {
		e := &orderv1.OrderEvent{Type: t}
		if o != nil {
			e.Order = toOrder(o, false)
		}
		return stream.Send(e)
	}

	// Subscribe before reading the current state, so no change falls in
	// between; a change may come twice.
	ch, cancel := s.API.Hub.Subscribe()
	defer cancel()

	if id != "" {
		o, err := s.API.Get(ctx, id)
		if apierrors.IsNotFound(err) || (err == nil && !s.API.MaySee(c, o)) {
			return status.Error(codes.NotFound, "order not found")
		}
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		if f.Match(o) && (req.GetSendInitial() || done(o)) {
			if err := send("ADDED", o); err != nil {
				return err
			}
		}
		if done(o) {
			return nil
		}
	}
	if req.GetSendInitial() {
		if id == "" {
			items, err := s.API.List(ctx, f)
			if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			// Oldest first, as they happened.
			for i := len(items) - 1; i >= 0; i-- {
				if rv := req.GetResourceVersion(); rv != "" && !newer(items[i].ResourceVersion, rv) {
					continue
				}
				if err := send("ADDED", &items[i]); err != nil {
					return err
				}
			}
		}
		if err := send("SYNCED", nil); err != nil {
			return err
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-ch:
			if !ok {
				return status.Error(codes.Unavailable, "watch closed")
			}
			if !f.Match(e.Order) {
				continue
			}
			if err := send(e.Type, e.Order); err != nil {
				return err
			}
			if id != "" && (e.Type == "DELETED" || done(e.Order)) {
				return nil
			}
		}
	}
}

// CancelOrder implements orderv1.OrderServiceServer.
func (s *Server) CancelOrder(ctx context.Context, req *orderv1.CancelOrderRequest) (*orderv1.Order, error) {
	code, body := s.API.Cancel(ctx, caller(ctx), req.GetId(), req.GetResourceVersion())
	return result(code, body, false)
}

// TeardownOrder implements orderv1.OrderServiceServer.
func (s *Server) TeardownOrder(ctx context.Context, req *orderv1.TeardownOrderRequest) (*orderv1.Order, error) {
	code, body := s.API.Teardown(ctx, caller(ctx), req.GetId(), api.TeardownRequest{
		Reason: req.GetReason(), AutoMerge: req.GetAutoMerge(), ResourceVersion: req.GetResourceVersion(),
	})
	return result(code, body, false)
}

// ListManagementClusters implements orderv1.OrderServiceServer.
func (s *Server) ListManagementClusters(ctx context.Context, _ *orderv1.ListManagementClustersRequest) (*orderv1.ListManagementClustersResponse, error) {
	cs, err := s.API.Clusters(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &orderv1.ListManagementClustersResponse{}
	for _, c := range cs {
		mc := &orderv1.ManagementCluster{
			Name: c.Name, Lab: c.Lab, Cordoned: c.Cordoned, Repository: c.Repository, OrdersPath: c.OrdersPath,
			Ready: c.Ready, ReadyMessage: c.ReadyMessage, AllowedKinds: c.AllowedKinds,
		}
		if am := c.AutoMerge; am != nil {
			mc.AutoMergeMethod = am.Method
			for _, r := range am.Rules {
				pr := &orderv1.AutoMergeRule{
					Name: r.Name, Users: r.Users, Groups: r.Groups, Templates: r.Templates,
					Parameters: lists(r.Parameters), ParameterPrefixes: lists(r.ParameterPrefixes),
				}
				for _, a := range r.Actions {
					pr.Actions = append(pr.Actions, string(a))
				}
				mc.AutoMergeRules = append(mc.AutoMergeRules, pr)
			}
		}
		out.Clusters = append(out.Clusters, mc)
	}
	return out, nil
}

func lists(m map[string][]string) map[string]*orderv1.StringList {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]*orderv1.StringList, len(m))
	for k, v := range m {
		out[k] = &orderv1.StringList{Values: v}
	}
	return out
}

// toStruct decodes a JSON object; nil when there is none or it is not one.
func toStruct(raw *runtime.RawExtension) *structpb.Struct {
	if raw == nil || len(raw.Raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw.Raw, &m); err != nil {
		return nil
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

func ts(t *metav1.Time) *timestamppb.Timestamp {
	if t == nil || t.IsZero() {
		return nil
	}
	return timestamppb.New(t.Time)
}

func pullRequest(d *v1alpha1.DeliveryStatus) *orderv1.PullRequest {
	if d == nil || d.PullRequest == nil {
		return nil
	}
	return &orderv1.PullRequest{Number: int32(d.PullRequest.Number), Url: d.PullRequest.URL, State: d.PullRequest.State, MergeSha: d.MergeSHA}
}

// toOrder flattens o; the rendered files only with withRendered.
func toOrder(o *v1alpha1.Order, withRendered bool) *orderv1.Order {
	out := &orderv1.Order{
		Id: o.Name, ResourceVersion: o.ResourceVersion, CreatedAt: ts(&o.CreationTimestamp),
		Template: o.Spec.Template, ManagementCluster: o.Spec.Placement.ManagementCluster,
		Parameters: toStruct(o.Spec.Parameters), RequestedBy: o.Spec.RequestedBy,
		RequestedByGroups: o.Spec.RequestedByGroups, Reason: o.Spec.Reason, Cancel: o.Spec.Cancel,
		Phase: string(o.Status.Phase), Message: o.Status.Message,
		PullRequest: pullRequest(o.Status.Delivery), Files: o.Status.Files,
	}
	if am := o.Spec.AutoMerge; am != nil {
		out.AutoMergeRule = am.Rule
	}
	if t := o.Spec.Teardown; t != nil {
		td := &orderv1.Teardown{Attempt: int32(t.Attempt), RequestedBy: t.RequestedBy, Reason: t.Reason}
		if t.AutoMerge != nil {
			td.AutoMergeRule = t.AutoMerge.Rule
		}
		if st := o.Status.Teardown; st != nil && st.Attempt == t.Attempt {
			td.Outcome = st.Outcome
			td.PullRequest = pullRequest(st.Delivery)
		}
		out.Teardown = td
	}
	if x := o.Status.XRRef; x != nil {
		out.XrRef = &orderv1.XRRef{ApiVersion: x.APIVersion, Kind: x.Kind, Name: x.Name, Namespace: x.Namespace}
	}
	if l := o.Status.Live; l != nil {
		out.Live = &orderv1.Live{
			Found: l.Found, Ready: l.Ready, StatusMessage: l.StatusMessage, Address: l.Address, Info: l.Info,
			ObservedAt: ts(l.ObservedAt), LastSeenAt: ts(l.LastSeenAt),
		}
	}
	for _, h := range o.Status.History {
		out.History = append(out.History, &orderv1.PhaseTransition{Phase: string(h.Phase), Time: ts(&h.Time), Message: h.Message})
	}
	for _, c := range o.Status.Conditions {
		out.Conditions = append(out.Conditions, &orderv1.Condition{
			Type: c.Type, Status: string(c.Status), Reason: c.Reason, Message: c.Message, LastTransitionTime: ts(&c.LastTransitionTime),
		})
	}
	if rd := o.EffectiveRendered(); withRendered && rd != nil {
		r := &orderv1.Rendered{Parameters: toStruct(rd.Parameters), RenderedAt: ts(&rd.RenderedAt)}
		for _, f := range rd.Files {
			r.Files = append(r.Files, &orderv1.RenderedFile{Path: f.Path, Content: f.Content})
		}
		out.Rendered = r
	}
	return out
}
