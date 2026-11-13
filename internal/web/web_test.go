package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/api"
	"github.com/stuttgart-things/machinery-order-service/internal/auth"
	"github.com/stuttgart-things/machinery-order-service/internal/auth/authtest"
	"github.com/stuttgart-things/machinery-order-service/internal/web"
)

func order(name, user string, phase v1alpha1.Phase) *v1alpha1.Order {
	return &v1alpha1.Order{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "orders"},
		Spec: v1alpha1.OrderSpec{
			Template: "native-vm-labda-vsphere", RequestedBy: user,
			Placement: v1alpha1.Placement{ManagementCluster: "machinery"},
			Rendered:  &v1alpha1.Rendered{Files: []v1alpha1.RenderedFile{{Path: "clusters/x/demo.yaml", Content: "kind: NativeVsphereVM\n"}}},
		},
		Status: v1alpha1.OrderStatus{
			Phase:    phase,
			Delivery: &v1alpha1.DeliveryStatus{PullRequest: &v1alpha1.PullRequest{Number: 7, URL: "https://github.com/stuttgart-things/stuttgart-things/pull/7", State: "open"}},
			Live:     &v1alpha1.LiveStatus{Found: true, Ready: phase == v1alpha1.PhaseReady, Address: "10.0.0.9"},
			History:  []v1alpha1.PhaseTransition{{Phase: v1alpha1.PhaseOrdered, Time: metav1.Now()}, {Phase: phase, Time: metav1.Now()}},
		},
	}
}

func handler(t *testing.T, a *auth.Authenticator, objs ...client.Object) (http.Handler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Order{}).WithObjects(objs...).Build()
	srv := &api.Server{Client: c, Namespace: "orders", Hub: api.NewHub(), BasePath: "/orders", Auth: a}
	srv.Extra = (&web.UI{API: srv, BasePath: "/orders"}).Register
	return srv.Handler(), c
}

func get(t *testing.T, h http.Handler, path string, hdr map[string]string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, string(body)
}

// absolute finds URLs that start with "/" in href/src/hx-*/EventSource/ajax:
// behind sthings-portal they would leave BASE_PATH.
var absolute = regexp.MustCompile(`(?:href|src|hx-get|hx-post|action)="/|EventSource\('/|ajax\('GET', '/`)

func TestPagesUnderBasePath(t *testing.T) {
	h, _ := handler(t, nil, order("ord-a", "user:default/alice", v1alpha1.PhasePROpen), order("ord-b", "user:default/bob", v1alpha1.PhaseReady))
	for _, p := range []string{"/orders/ui/", "/orders/ui/orders/ord-a", "/orders/ui/orders/ord-b", "/orders/ui/rows", "/orders/ui/orders/ord-a/body"} {
		code, body := get(t, h, p, nil)
		if code != 200 {
			t.Errorf("%s: %d", p, code)
			continue
		}
		if m := absolute.FindString(strings.Replace(body, `<base href="/orders/">`, "", 1)); m != "" {
			t.Errorf("%s: absolute URL %q", p, m)
		}
		if strings.Contains(body, "<html") && !strings.Contains(body, `<base href="/orders/">`) {
			t.Errorf("%s: no base href", p)
		}
	}
	_, list := get(t, h, "/orders/ui/", nil)
	for _, want := range []string{"ord-a", "ord-b", "pull/7", "10.0.0.9", "PROpen", "Ready"} {
		if !strings.Contains(list, want) {
			t.Errorf("list misses %q", want)
		}
	}
	_, a := get(t, h, "/orders/ui/orders/ord-a", nil)
	if !strings.Contains(a, "Cancel order") || !strings.Contains(a, "clusters/x/demo.yaml") {
		t.Error("detail of an open order: no cancel button or no files")
	}
	_, b := get(t, h, "/orders/ui/orders/ord-b", nil)
	if strings.Contains(b, "Cancel order") {
		t.Error("a Ready order offers Cancel")
	}
	if code, _ := get(t, h, "/orders/ui/static/htmx.min.js", nil); code != 200 {
		t.Errorf("htmx: %d", code)
	}
	if _, portal := get(t, h, "/orders/ui/", map[string]string{"X-Forwarded-Prefix": "/orders"}); !strings.Contains(portal, "homerun2-tokens.css") {
		t.Error("no portal tokens behind the portal")
	}
	if code, _ := get(t, h, "/orders/ui/orders/nope", nil); code != 404 {
		t.Errorf("missing order: %d", code)
	}
}

func TestForwardedUserAndCancel(t *testing.T) {
	iss := authtest.NewIssuer(t)
	a, err := auth.New(auth.Config{Issuer: iss.URL(), Audiences: []string{"x"}, CAFile: iss.CAFile, TrustForwardedUser: true})
	if err != nil {
		t.Fatal(err)
	}
	h, c := handler(t, a, order("ord-a", "user:default/alice", v1alpha1.PhasePROpen), order("ord-b", "user:default/bob", v1alpha1.PhasePROpen))

	if code, _ := get(t, h, "/orders/ui/", nil); code != 401 {
		t.Errorf("no login: %d", code)
	}
	alice := map[string]string{"X-Forwarded-User": "alice@sthings.example"}
	_, list := get(t, h, "/orders/ui/", alice)
	if !strings.Contains(list, "ord-a") || strings.Contains(list, "ord-b") {
		t.Error("my orders shows someone else's order")
	}
	_, other := get(t, h, "/orders/ui/orders/ord-b", alice)
	if strings.Contains(other, "Cancel order") {
		t.Error("alice may cancel bob's order")
	}
	// The API ignores the forwarded user.
	if code, _ := get(t, h, "/orders/api/v1/orders", alice); code != 401 {
		t.Errorf("API with a forwarded user: %d", code)
	}

	o := &v1alpha1.Order{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: "orders", Name: "ord-a"}, o)
	form := url.Values{"resourceVersion": {o.ResourceVersion}}
	req := httptest.NewRequest("POST", "/orders/ui/orders/ord-a/cancel", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-User", "alice@sthings.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: "orders", Name: "ord-a"}, o)
	if !o.Spec.Cancel {
		t.Error("not cancelled")
	}
}
