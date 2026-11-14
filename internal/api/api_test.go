package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/api"
	"github.com/stuttgart-things/machinery-order-service/internal/auth"
	"github.com/stuttgart-things/machinery-order-service/internal/auth/authtest"
	"github.com/stuttgart-things/machinery-order-service/internal/render"
	"github.com/stuttgart-things/machinery-order-service/internal/render/rendertest"
)

const audience = "machinery-order-service"

type setup struct {
	srv      *api.Server
	h        http.Handler
	c        client.Client
	renderer *rendertest.Fake
	issuer   *authtest.Issuer
}

func newSetup(t *testing.T, withAuth bool) *setup {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Order{}).WithObjects(
		&v1alpha1.ManagementCluster{ObjectMeta: metav1.ObjectMeta{Name: "machinery"},
			Spec: v1alpha1.ManagementClusterSpec{GitOps: v1alpha1.GitOps{OrdersPath: "clusters/labda/vsphere/machinery-orders-xrs"}}},
		&v1alpha1.ManagementCluster{ObjectMeta: metav1.ObjectMeta{Name: "closed"}, Spec: v1alpha1.ManagementClusterSpec{Cordoned: true}},
	).Build()
	s := &setup{c: c, renderer: &rendertest.Fake{}}
	s.srv = &api.Server{Client: c, Namespace: "orders", Renderer: s.renderer, Hub: api.NewHub(), BasePath: "/orders"}
	if withAuth {
		s.issuer = authtest.NewIssuer(t)
		a, err := auth.New(auth.Config{Issuer: s.issuer.URL(), Audiences: []string{audience}, CAFile: s.issuer.CAFile, AdminRoles: []string{"platform-admin"}})
		if err != nil {
			t.Fatal(err)
		}
		s.srv.Auth = a
	}
	s.h = s.srv.Handler()
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

func (s *setup) do(t *testing.T, method, path, token string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = strings.NewReader(string(raw))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestCreateWithoutAuth(t *testing.T) {
	s := newSetup(t, false)
	rec, out := s.do(t, "POST", "/api/v1/orders", "", map[string]any{
		"template": "native-vm-labda-vsphere", "managementCluster": "machinery",
		"parameters": map[string]any{"name": "demo"}, "requestedBy": "user:default/sthings",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	id := out["metadata"].(map[string]any)["name"].(string)
	if !strings.HasPrefix(id, "ord-") {
		t.Errorf("id %q", id)
	}
	o := &v1alpha1.Order{}
	if err := s.c.Get(context.Background(), client.ObjectKey{Namespace: "orders", Name: id}, o); err != nil {
		t.Fatal(err)
	}
	if o.Spec.Rendered == nil || len(o.Spec.Rendered.Files) != 1 || o.Spec.RequestedBy != "user:default/sthings" || o.Labels[v1alpha1.OrderIDKey] != id {
		t.Errorf("order %+v", o)
	}
	if !strings.Contains(o.Spec.Rendered.Files[0].Content, "order-id: "+id) {
		t.Errorf("rendered without the order id")
	}

	// Under BASE_PATH as well.
	rec, _ = s.do(t, "GET", "/orders/api/v1/orders/"+id, "", nil)
	if rec.Code != 200 {
		t.Errorf("base path: %d", rec.Code)
	}

	for name, tc := range map[string]struct {
		body map[string]any
		code int
	}{
		"reserved":  {map[string]any{"template": "t", "managementCluster": "machinery", "parameters": map[string]any{"name": "x", "order_id": "y"}}, 400},
		"cluster":   {map[string]any{"template": "t", "managementCluster": "nope", "parameters": map[string]any{"name": "x"}}, 400},
		"cordoned":  {map[string]any{"template": "t", "managementCluster": "closed", "parameters": map[string]any{"name": "x"}}, 409},
		"no fields": {map[string]any{"managementCluster": "machinery"}, 400},
		"unknown":   {map[string]any{"template": "t", "managementCluster": "machinery", "bogus": 1}, 400},
	} {
		t.Run(name, func(t *testing.T) {
			if rec, _ := s.do(t, "POST", "/api/v1/orders", "", tc.body); rec.Code != tc.code {
				t.Errorf("%d %s", rec.Code, rec.Body)
			}
		})
	}

	s.renderer.Err = &render.APIError{Status: 400, Message: "invalid parameters", Fields: map[string]string{"size": "no"}}
	rec, out = s.do(t, "POST", "/api/v1/orders", "", map[string]any{"template": "t", "managementCluster": "machinery", "parameters": map[string]any{"name": "x"}})
	if rec.Code != 400 || out["fields"].(map[string]any)["size"] != "no" {
		t.Errorf("api error: %d %v", rec.Code, out)
	}
}

func TestAuthListCancel(t *testing.T) {
	s := newSetup(t, true)
	alice := s.token(t, "alice@sthings.example")
	bob := s.token(t, "bob")
	admin := s.token(t, "root", "platform-admin")

	if rec, _ := s.do(t, "POST", "/api/v1/orders", "", map[string]any{"template": "t", "managementCluster": "machinery"}); rec.Code != 401 {
		t.Errorf("no token: %d", rec.Code)
	}
	rec, out := s.do(t, "POST", "/api/v1/orders", alice, map[string]any{
		"template": "native-vm-labda-vsphere", "managementCluster": "machinery",
		"parameters": map[string]any{"name": "a"}, "requestedBy": "user:default/forged",
	})
	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := out["spec"].(map[string]any)["requestedBy"]; got != "user:default/alice" {
		t.Errorf("requestedBy %v", got)
	}
	if s.renderer.Tokens[0] != alice {
		t.Error("the caller's token was not forwarded to claim-machinery-api")
	}
	id := out["metadata"].(map[string]any)["name"].(string)
	if rec, _ := s.do(t, "POST", "/api/v1/orders", bob, map[string]any{"template": "native-vm-labda-vsphere", "managementCluster": "machinery", "parameters": map[string]any{"name": "b"}}); rec.Code != 201 {
		t.Fatal(rec.Body)
	}

	_, out = s.do(t, "GET", "/api/v1/orders?mine=true", alice, nil)
	items := out["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["spec"].(map[string]any)["rendered"] != nil {
		t.Errorf("mine: %v", items)
	}
	_, out = s.do(t, "GET", "/api/v1/orders", alice, nil)
	if len(out["items"].([]any)) != 2 {
		t.Errorf("all: %v", out)
	}

	// Cancel: not bob's; a moved resourceVersion; merged; then alice.
	if rec, _ := s.do(t, "POST", "/api/v1/orders/"+id+"/cancel", bob, nil); rec.Code != 403 {
		t.Errorf("bob: %d", rec.Code)
	}
	if rec, _ := s.do(t, "POST", "/api/v1/orders/"+id+"/cancel", alice, map[string]any{"resourceVersion": "999"}); rec.Code != 409 {
		t.Errorf("stale rv: %d %s", rec.Code, rec.Body)
	}
	o := &v1alpha1.Order{}
	_ = s.c.Get(context.Background(), client.ObjectKey{Namespace: "orders", Name: id}, o)
	o.Status.Phase = v1alpha1.PhaseMerged
	_ = s.c.Status().Update(context.Background(), o)
	if rec, _ := s.do(t, "POST", "/api/v1/orders/"+id+"/cancel", admin, nil); rec.Code != 409 {
		t.Errorf("merged: %d", rec.Code)
	}
	_ = s.c.Get(context.Background(), client.ObjectKey{Namespace: "orders", Name: id}, o)
	o.Status.Phase = v1alpha1.PhasePROpen
	_ = s.c.Status().Update(context.Background(), o)
	_ = s.c.Get(context.Background(), client.ObjectKey{Namespace: "orders", Name: id}, o)
	rec, _ = s.do(t, "POST", "/api/v1/orders/"+id+"/cancel", alice, map[string]any{"resourceVersion": o.ResourceVersion})
	if rec.Code != 202 {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	_ = s.c.Get(context.Background(), client.ObjectKey{Namespace: "orders", Name: id}, o)
	if !o.Spec.Cancel {
		t.Error("spec.cancel not set")
	}
}

func TestWatch(t *testing.T) {
	s := newSetup(t, false)
	ts := httptest.NewServer(s.h)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/orders/api/v1/orders/watch?cluster=machinery", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Scan() // ": watching orders"
	go func() {
		time.Sleep(100 * time.Millisecond)
		s.srv.Hub.OnAdd(&v1alpha1.Order{ObjectMeta: metav1.ObjectMeta{Name: "ord-other"}, Spec: v1alpha1.OrderSpec{Placement: v1alpha1.Placement{ManagementCluster: "elsewhere"}}}, false)
		s.srv.Hub.OnUpdate(nil, &v1alpha1.Order{ObjectMeta: metav1.ObjectMeta{Name: "ord-x"}, Spec: v1alpha1.OrderSpec{Placement: v1alpha1.Placement{ManagementCluster: "machinery"}},
			Status: v1alpha1.OrderStatus{Phase: v1alpha1.PhaseReady}})
	}()
	var event, data string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	var e api.Event
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		t.Fatalf("%v: %q", err, data)
	}
	if event != "order" || e.Type != "MODIFIED" || e.Order.Name != "ord-x" || e.Order.Status.Phase != v1alpha1.PhaseReady {
		t.Errorf("event %q %+v", event, e)
	}
}
