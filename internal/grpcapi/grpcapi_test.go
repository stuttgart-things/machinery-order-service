package grpcapi_test

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orderv1 "github.com/stuttgart-things/machinery-order-service/api/proto/order/v1"
	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/api"
	"github.com/stuttgart-things/machinery-order-service/internal/auth"
	"github.com/stuttgart-things/machinery-order-service/internal/auth/authtest"
	"github.com/stuttgart-things/machinery-order-service/internal/grpcapi"
	"github.com/stuttgart-things/machinery-order-service/internal/render"
	"github.com/stuttgart-things/machinery-order-service/internal/render/rendertest"
)

const audience = "machinery-order-service"

type setup struct {
	api      *api.Server
	c        client.Client
	renderer *rendertest.Fake
	issuer   *authtest.Issuer
	oc       orderv1.OrderServiceClient
	health   healthpb.HealthClient
}

func newSetup(t *testing.T) *setup {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Order{}).WithObjects(
		&v1alpha1.ManagementCluster{ObjectMeta: metav1.ObjectMeta{Name: "machinery"},
			Spec: v1alpha1.ManagementClusterSpec{
				Lab:    "labda",
				GitOps: v1alpha1.GitOps{OrdersPath: "clusters/labda/vsphere/machinery-orders-xrs"},
				AutoMerge: &v1alpha1.AutoMerge{Rules: []v1alpha1.AutoMergeRule{{
					Name: "personas", Groups: []string{"group:default/ai-personas"},
					Parameters: map[string][]string{"size": {"small"}},
				}}},
			}},
		&v1alpha1.ManagementCluster{ObjectMeta: metav1.ObjectMeta{Name: "closed"}, Spec: v1alpha1.ManagementClusterSpec{Cordoned: true}},
	).Build()
	s := &setup{c: c, renderer: &rendertest.Fake{}, issuer: authtest.NewIssuer(t)}
	a, err := auth.New(auth.Config{
		Issuer: s.issuer.URL(), Audiences: []string{audience}, CAFile: s.issuer.CAFile,
		AdminRoles: []string{"platform-admin"}, OrderRoles: []string{"backstage-team-ai-personas", "backstage-team-other", "platform-admin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.api = &api.Server{Client: c, Namespace: "orders", Renderer: s.renderer, Hub: api.NewHub(), Auth: a}

	lis := bufconn.Listen(1 << 20)
	g := grpcapi.New(s.api)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s.oc = orderv1.NewOrderServiceClient(conn)
	s.health = healthpb.NewHealthClient(conn)
	return s
}

func (s *setup) token(t *testing.T, user string, roles ...string) string {
	t.Helper()
	claims := s.issuer.Claims(audience)
	claims["preferred_username"] = user
	rm := map[string]any{}
	for _, r := range roles {
		rm[r] = map[string]any{}
	}
	claims[auth.ZitadelRolesClaim] = rm
	return s.issuer.Sign(t, claims)
}

func as(token string) context.Context {
	ctx := context.Background()
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}
	return ctx
}

func code(err error) codes.Code { return status.Code(err) }

func params(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	p, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (s *setup) create(t *testing.T, token, name string) *orderv1.Order {
	t.Helper()
	o, err := s.oc.CreateOrder(as(token), &orderv1.CreateOrderRequest{
		Template: "native-vm-labda-vsphere", ManagementCluster: "machinery", Parameters: params(t, map[string]any{"name": name}),
	})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return o
}

func TestAuth(t *testing.T) {
	s := newSetup(t)
	if _, err := s.oc.ListOrders(context.Background(), &orderv1.ListOrdersRequest{}); code(err) != codes.Unauthenticated {
		t.Errorf("without a token: %v", err)
	}
	if _, err := s.oc.ListOrders(as("not-a-jwt"), &orderv1.ListOrdersRequest{}); code(err) != codes.Unauthenticated {
		t.Errorf("bad token: %v", err)
	}
	if _, err := s.oc.ListOrders(as(s.token(t, "eve", "something-else")), &orderv1.ListOrdersRequest{}); code(err) != codes.PermissionDenied {
		t.Errorf("without an order role: %v", err)
	}
	// Streams are checked too.
	ws, err := s.oc.WatchOrders(context.Background(), &orderv1.WatchOrdersRequest{})
	if err == nil {
		_, err = ws.Recv()
	}
	if code(err) != codes.Unauthenticated {
		t.Errorf("watch without a token: %v", err)
	}
	// Health needs no token.
	if h, err := s.health.Check(context.Background(), &healthpb.HealthCheckRequest{Service: orderv1.OrderService_ServiceDesc.ServiceName}); err != nil || h.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("health %v %v", h, err)
	}
}

func TestCreateGetListVisibility(t *testing.T) {
	s := newSetup(t)
	alice := s.token(t, "alice", "backstage-team-ai-personas")
	dave := s.token(t, "dave", "backstage-team-other")
	admin := s.token(t, "root", "platform-admin")

	o := s.create(t, alice, "vis")
	if o.GetRequestedBy() != "user:default/alice" || o.GetPhase() != "Ordered" || o.GetParameters().AsMap()["name"] != "vis" ||
		len(o.GetRendered().GetFiles()) != 1 {
		t.Fatalf("created %v", o)
	}
	// requested_by comes from the token: the renderer saw alice's.
	if tok := s.renderer.Tokens[len(s.renderer.Tokens)-1]; tok != alice {
		t.Error("rendered without the caller's token")
	}

	got, err := s.oc.GetOrder(as(alice), &orderv1.GetOrderRequest{Id: o.GetId()})
	if err != nil || got.GetId() != o.GetId() || !strings.Contains(got.GetRendered().GetFiles()[0].GetContent(), "order-id: "+o.GetId()) {
		t.Errorf("get: %v %v", got, err)
	}
	if _, err := s.oc.GetOrder(as(dave), &orderv1.GetOrderRequest{Id: o.GetId()}); code(err) != codes.NotFound {
		t.Errorf("someone else's order: %v", err)
	}
	if _, err := s.oc.GetOrder(as(admin), &orderv1.GetOrderRequest{Id: o.GetId()}); err != nil {
		t.Errorf("admin: %v", err)
	}
	if _, err := s.oc.GetOrder(as(alice), &orderv1.GetOrderRequest{Id: "ord-missing"}); code(err) != codes.NotFound {
		t.Errorf("missing: %v", err)
	}

	s.create(t, dave, "other")
	for name, tc := range map[string]struct {
		token string
		f     *orderv1.OrderFilter
		want  int
	}{
		"alice sees hers":      {alice, nil, 1},
		"admin sees all":       {admin, nil, 2},
		"admin, mine":          {admin, &orderv1.OrderFilter{Mine: true}, 0},
		"phase filter":         {admin, &orderv1.OrderFilter{Phase: "PROpen"}, 0},
		"cluster filter":       {admin, &orderv1.OrderFilter{Cluster: "machinery"}, 2},
		"other cluster filter": {admin, &orderv1.OrderFilter{Cluster: "closed"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			l, err := s.oc.ListOrders(as(tc.token), &orderv1.ListOrdersRequest{Filter: tc.f})
			if err != nil || len(l.GetOrders()) != tc.want {
				t.Fatalf("%d orders, want %d (%v)", len(l.GetOrders()), tc.want, err)
			}
			for _, o := range l.GetOrders() {
				if o.GetRendered() != nil {
					t.Error("list carries rendered files")
				}
			}
		})
	}
}

func TestErrorCodes(t *testing.T) {
	s := newSetup(t)
	alice := s.token(t, "alice", "backstage-team-ai-personas")
	dave := s.token(t, "dave", "backstage-team-other")
	create := func(token string, req *orderv1.CreateOrderRequest) error {
		_, err := s.oc.CreateOrder(as(token), req)
		return err
	}

	if err := create(alice, &orderv1.CreateOrderRequest{Template: "native-vm-labda-vsphere"}); code(err) != codes.InvalidArgument {
		t.Errorf("no cluster: %v", err)
	}
	if err := create(alice, &orderv1.CreateOrderRequest{Template: "t", ManagementCluster: "closed"}); code(err) != codes.FailedPrecondition {
		t.Errorf("cordoned: %v", err)
	}
	if err := create(dave, &orderv1.CreateOrderRequest{Template: "native-vm-labda-vsphere", ManagementCluster: "machinery", AutoMerge: true,
		Parameters: params(t, map[string]any{"name": "am"})}); code(err) != codes.PermissionDenied ||
		!strings.Contains(err.Error(), "personas: not for user:default/dave") {
		t.Errorf("auto-merge without a rule: %v", err)
	}

	// claim-machinery-api's fields come back as BadRequest field violations.
	s.renderer.Err = &render.APIError{Status: 400, Message: "invalid parameters", Fields: map[string]string{"size": "must be one of small, medium", "name": "required"}}
	err := create(alice, &orderv1.CreateOrderRequest{Template: "native-vm-labda-vsphere", ManagementCluster: "machinery", Parameters: params(t, map[string]any{"name": "bad"})})
	if code(err) != codes.InvalidArgument {
		t.Fatalf("invalid parameters: %v", err)
	}
	var fields []string
	for _, d := range status.Convert(err).Details() {
		if br, ok := d.(*errdetails.BadRequest); ok {
			for _, v := range br.GetFieldViolations() {
				fields = append(fields, v.GetField()+": "+v.GetDescription())
			}
		}
	}
	if strings.Join(fields, "; ") != "name: required; size: must be one of small, medium" {
		t.Errorf("field violations %v", fields)
	}
	s.renderer.Err = nil

	o := s.create(t, alice, "cancel-me")
	if _, err := s.oc.CancelOrder(as(alice), &orderv1.CancelOrderRequest{Id: o.GetId(), ResourceVersion: "1"}); code(err) != codes.Aborted {
		t.Errorf("stale resource version: %v", err)
	}
	if _, err := s.oc.CancelOrder(as(dave), &orderv1.CancelOrderRequest{Id: o.GetId()}); code(err) != codes.NotFound {
		t.Errorf("cancel someone else's: %v", err)
	}
	if _, err := s.oc.TeardownOrder(as(alice), &orderv1.TeardownOrderRequest{Id: o.GetId()}); code(err) != codes.FailedPrecondition ||
		!strings.Contains(err.Error(), "cancel it instead") {
		t.Errorf("teardown before the merge: %v", err)
	}
	c, err := s.oc.CancelOrder(as(alice), &orderv1.CancelOrderRequest{Id: o.GetId(), ResourceVersion: o.GetResourceVersion()})
	if err != nil || !c.GetCancel() {
		t.Errorf("cancel: %v %v", c, err)
	}
}

func TestListManagementClusters(t *testing.T) {
	s := newSetup(t)
	l, err := s.oc.ListManagementClusters(as(s.token(t, "dave", "backstage-team-other")), &orderv1.ListManagementClustersRequest{})
	if err != nil || len(l.GetClusters()) != 2 {
		t.Fatalf("%v %v", l, err)
	}
	mc := l.GetClusters()[1]
	if mc.GetName() != "machinery" || mc.GetLab() != "labda" || len(mc.GetAutoMergeRules()) != 1 ||
		mc.GetAutoMergeRules()[0].GetParameters()["size"].GetValues()[0] != "small" {
		t.Errorf("cluster %v", mc)
	}
	if !l.GetClusters()[0].GetCordoned() {
		t.Errorf("closed %v", l.GetClusters()[0])
	}
}

// setPhase moves an order on as the controller would, which the hub sees.
func (s *setup) setPhase(t *testing.T, id string, p v1alpha1.Phase) {
	t.Helper()
	o := &v1alpha1.Order{}
	if err := s.c.Get(context.Background(), client.ObjectKey{Namespace: "orders", Name: id}, o); err != nil {
		t.Fatal(err)
	}
	o.Status.Phase = p
	if err := s.c.Status().Update(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	s.api.Hub.OnUpdate(nil, o)
}

func recvAll(t *testing.T, ws grpc.ServerStreamingClient[orderv1.OrderEvent], n int) []*orderv1.OrderEvent {
	t.Helper()
	var out []*orderv1.OrderEvent
	for len(out) < n {
		ev, err := ws.Recv()
		if err != nil {
			t.Fatalf("after %d events: %v", len(out), err)
		}
		out = append(out, ev)
	}
	return out
}

func TestWatch(t *testing.T) {
	s := newSetup(t)
	alice := s.token(t, "alice", "backstage-team-ai-personas")
	dave := s.token(t, "dave", "backstage-team-other")
	o := s.create(t, alice, "watched")
	other := s.create(t, dave, "unseen")

	// One order until PROpen: Placed passes, PROpen ends the stream.
	ctx, cancel := context.WithTimeout(as(alice), 10*time.Second)
	defer cancel()
	ws, err := s.oc.WatchOrders(ctx, &orderv1.WatchOrdersRequest{OrderId: o.GetId(), UntilPhases: []string{"PROpen"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the server subscribes to the hub
	s.setPhase(t, other.GetId(), v1alpha1.PhasePlaced)
	s.setPhase(t, o.GetId(), v1alpha1.PhasePlaced)
	s.setPhase(t, o.GetId(), v1alpha1.PhasePROpen)
	evs := recvAll(t, ws, 2)
	if evs[0].GetOrder().GetPhase() != "Placed" || evs[1].GetOrder().GetPhase() != "PROpen" || evs[1].GetType() != "MODIFIED" {
		t.Errorf("events %v", evs)
	}
	if _, err := ws.Recv(); err != io.EOF {
		t.Errorf("the watch did not end at PROpen: %v", err)
	}

	// Already there: sent once, then the end.
	ws, err = s.oc.WatchOrders(ctx, &orderv1.WatchOrdersRequest{OrderId: o.GetId(), UntilPhases: []string{"PROpen"}})
	if err != nil {
		t.Fatal(err)
	}
	if evs := recvAll(t, ws, 1); evs[0].GetOrder().GetPhase() != "PROpen" {
		t.Errorf("events %v", evs)
	}
	if _, err := ws.Recv(); err != io.EOF {
		t.Errorf("the watch did not end: %v", err)
	}

	// Someone else's order is not there.
	ws, err = s.oc.WatchOrders(ctx, &orderv1.WatchOrdersRequest{OrderId: other.GetId()})
	if err == nil {
		_, err = ws.Recv()
	}
	if code(err) != codes.NotFound {
		t.Errorf("watch someone else's: %v", err)
	}

	// All of alice's: initial state, SYNCED, then changes; dave's are hidden.
	ws, err = s.oc.WatchOrders(ctx, &orderv1.WatchOrdersRequest{SendInitial: true})
	if err != nil {
		t.Fatal(err)
	}
	evs = recvAll(t, ws, 2)
	if evs[0].GetType() != "ADDED" || evs[0].GetOrder().GetId() != o.GetId() || evs[1].GetType() != "SYNCED" {
		t.Errorf("initial %v", evs)
	}
	s.setPhase(t, other.GetId(), v1alpha1.PhasePROpen)
	s.setPhase(t, o.GetId(), v1alpha1.PhaseMerged)
	if evs = recvAll(t, ws, 1); evs[0].GetOrder().GetId() != o.GetId() || evs[0].GetOrder().GetPhase() != "Merged" {
		t.Errorf("change %v", evs)
	}

	// Resumed after the latest version: nothing to catch up on.
	cur, _ := s.oc.GetOrder(as(alice), &orderv1.GetOrderRequest{Id: o.GetId()})
	ws, err = s.oc.WatchOrders(ctx, &orderv1.WatchOrdersRequest{SendInitial: true, ResourceVersion: cur.GetResourceVersion()})
	if err != nil {
		t.Fatal(err)
	}
	if evs = recvAll(t, ws, 1); evs[0].GetType() != "SYNCED" {
		t.Errorf("resumed %v", evs)
	}
}
