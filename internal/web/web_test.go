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
	if !strings.Contains(b, "Tear down") || !strings.Contains(b, `hx-post="ui/orders/ord-b/teardown"`) {
		t.Error("a Ready order offers no teardown")
	}
	if strings.Contains(a, `ui/orders/ord-a/teardown`) {
		t.Error("an open order offers a teardown")
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

func TestDeletedOrderShowsGone(t *testing.T) {
	o := order("ord-g", "user:default/alice", v1alpha1.PhaseDeleted)
	seen := metav1.NewTime(metav1.Now().Add(-time.Hour))
	o.Status.Live = &v1alpha1.LiveStatus{Found: false, Address: "10.0.0.9", LastSeenAt: &seen}
	h, _ := handler(t, nil, o)
	for _, p := range []string{"/orders/ui/", "/orders/ui/orders/ord-g"} {
		code, body := get(t, h, p, nil)
		if code != 200 || !strings.Contains(body, ">gone<") || !strings.Contains(body, "10.0.0.9") || strings.Contains(body, "not seen") {
			t.Errorf("%s: %d\n%s", p, code, body)
		}
	}
	if _, d := get(t, h, "/orders/ui/orders/ord-g", nil); !strings.Contains(d, "last seen "+seen.UTC().Format(time.DateTime)) || !strings.Contains(d, "<strong>Deleted</strong>") {
		t.Error("detail: no last seen or no Deleted in the timeline")
	}
}

// The portal fetches the manifest without a login; the fragment is the
// caller's orders, with the prefix on every URL (no <base href> on the
// portal's overview).
func TestPortalFragment(t *testing.T) {
	iss := authtest.NewIssuer(t)
	a, err := auth.New(auth.Config{Issuer: iss.URL(), Audiences: []string{"x"}, CAFile: iss.CAFile, TrustForwardedUser: true})
	if err != nil {
		t.Fatal(err)
	}
	mine := order("ord-a", "user:default/alice", v1alpha1.PhaseBuilding)
	mine.Spec.Parameters = &runtime.RawExtension{Raw: []byte(`{"name":"vm-<alice>"}`)}
	h, _ := handler(t, a, mine,
		order("ord-b", "user:default/alice", v1alpha1.PhaseReady),
		order("ord-c", "user:default/alice", v1alpha1.PhaseFailed),
		order("ord-d", "user:default/bob", v1alpha1.PhaseReady))

	code, m := get(t, h, "/orders/ui/manifest.json", nil)
	if code != 200 || !strings.Contains(m, `"url":"/orders/ui/fragment/orders"`) || !strings.Contains(m, `"title":"Orders"`) {
		t.Errorf("manifest: %d %s", code, m)
	}

	if code, f := get(t, h, "/orders/ui/fragment/orders", nil); code != 200 || !strings.Contains(f, "Sign in") || strings.Contains(f, "ord-") {
		t.Errorf("fragment without login: %d %s", code, f)
	}

	code, f := get(t, h, "/orders/ui/fragment/orders", map[string]string{"X-Forwarded-User": "alice@sthings.example"})
	if code != 200 {
		t.Fatalf("fragment: %d", code)
	}
	for _, want := range []string{
		"1 open · 1 ready · 1 failed",
		`href="/orders/ui/orders/ord-a"`, "vm-&lt;alice&gt;", "hr-sev-info", "hr-sev-success", "hr-sev-error",
		`href="/orders/ui/"`,
	} {
		if !strings.Contains(f, want) {
			t.Errorf("fragment misses %q:\n%s", want, f)
		}
	}
	if strings.Contains(f, "ord-d") || strings.Contains(f, "<html") || strings.Contains(f, "<script") {
		t.Errorf("fragment: someone else's order or a full page:\n%s", f)
	}
	for _, href := range regexp.MustCompile(`href="([^"]*)"`).FindAllStringSubmatch(f, -1) {
		if !strings.HasPrefix(href[1], "/orders/") {
			t.Errorf("fragment URL without the prefix: %s", href[1])
		}
	}
}

func TestTeardownButton(t *testing.T) {
	iss := authtest.NewIssuer(t)
	a, err := auth.New(auth.Config{Issuer: iss.URL(), Audiences: []string{"x"}, CAFile: iss.CAFile, TrustForwardedUser: true})
	if err != nil {
		t.Fatal(err)
	}
	h, c := handler(t, a, order("ord-a", "user:default/alice", v1alpha1.PhaseReady), order("ord-b", "user:default/bob", v1alpha1.PhaseReady))
	alice := map[string]string{"X-Forwarded-User": "alice@sthings.example"}
	if _, other := get(t, h, "/orders/ui/orders/ord-b", alice); strings.Contains(other, "teardown") {
		t.Error("alice may tear down bob's order")
	}
	o := &v1alpha1.Order{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: "orders", Name: "ord-a"}, o)
	form := url.Values{"resourceVersion": {o.ResourceVersion}, "reason": {"no longer needed"}}
	req := httptest.NewRequest("POST", "/orders/ui/orders/ord-a/teardown", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-User", "alice@sthings.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("teardown: %d %s", rec.Code, rec.Body)
	}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: "orders", Name: "ord-a"}, o)
	if td := o.Spec.Teardown; td == nil || td.Reason != "no longer needed" || td.AutoMerge != nil || td.RequestedBy != "user:default/alice" {
		t.Errorf("spec.teardown %+v", td)
	}
	if strings.Contains(rec.Body.String(), `ui/orders/ord-a/teardown`) {
		t.Error("the button is still there while the teardown runs")
	}
	if !strings.Contains(rec.Body.String(), "TeardownPROpen") {
		t.Error("the timeline shows no teardown")
	}
}

// owned is an order whose rendered parameters name an owner group.
func owned(name, user, owner string, created time.Time) *v1alpha1.Order {
	o := order(name, user, v1alpha1.PhaseReady)
	o.CreationTimestamp = metav1.NewTime(created)
	o.Spec.Rendered.Parameters = &runtime.RawExtension{Raw: []byte(`{"owner":"` + owner + `"}`)}
	return o
}

func TestAllOrdersByGroup(t *testing.T) {
	iss := authtest.NewIssuer(t)
	a, err := auth.New(auth.Config{Issuer: iss.URL(), Audiences: []string{"x"}, CAFile: iss.CAFile,
		TrustForwardedUser: true, AdminRoles: []string{"portal-admin"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	persona := order("ord-persona", "user:default/persona-developer", v1alpha1.PhaseReady)
	persona.CreationTimestamp = metav1.NewTime(now.Add(-1 * time.Minute))
	persona.Spec.RequestedByGroups = []string{"group:default/ai-personas", "group:default/guests"}
	old := order("ord-nogroup", "user:default/carol", v1alpha1.PhaseDeleted) // before requestedByGroups
	old.CreationTimestamp = metav1.NewTime(now.Add(-3 * time.Hour))
	h, _ := handler(t, a,
		owned("ord-ops", "user:default/bob", "group:default/ops", now.Add(-2*time.Hour)),
		owned("ord-platform", "user:default/alice", "group:default/platform-team", now.Add(-30*time.Minute)),
		persona, old)

	// Admin: every order, with orderer and group columns.
	admin := map[string]string{"X-Forwarded-User": "alice@sthings.example", "X-Forwarded-Groups": "portal-admin"}
	_, mine := get(t, h, "/orders/ui/", admin)
	if !strings.Contains(mine, "ord-platform") || strings.Contains(mine, "ord-persona") {
		t.Error("my orders: want only alice's")
	}
	_, all := get(t, h, "/orders/ui/?scope=all", admin)
	for _, want := range []string{"ord-ops", "ord-platform", "ord-persona", "ord-nogroup", "Every order (admin)",
		"<th>Requested by</th><th>Group</th>", `<code title="owner group:default/ops">ops</code>`,
		`title="no owner: the orderer's groups group:default/ai-personas, group:default/guests">ai-personas, guests</span>`,
		`<option value="group:default/ai-personas">`, `EventSource('ui/events?scope=all')`} {
		if !strings.Contains(all, want) {
			t.Errorf("all orders lacks %q", want)
		}
	}
	if m := absolute.FindString(strings.Replace(all, `<base href="/orders/">`, "", 1)); m != "" {
		t.Errorf("absolute URL in the all-orders page: %q", m)
	}

	// One group: the owner or one of the orderer's groups.
	_, one := get(t, h, "/orders/ui/?scope=all&group="+url.QueryEscape("group:default/ai-personas"), admin)
	if !strings.Contains(one, "ord-persona") || strings.Contains(one, "ord-ops") || strings.Contains(one, "ord-platform") {
		t.Error("group filter ai-personas")
	}
	if !strings.Contains(one, `<option value="group:default/ops">`) {
		t.Error("the group filter should still offer the other groups")
	}
	_, none := get(t, h, "/orders/ui/?scope=all&group=-", admin)
	if !strings.Contains(none, "ord-nogroup") || strings.Contains(none, "ord-persona") {
		t.Error("group filter: no group")
	}

	// Sorted by group: headers in order, orders without a group last.
	_, by := get(t, h, "/orders/ui/?scope=all&sort=group", admin)
	idx := func(s string) int { return strings.Index(by, s) }
	heads := []string{`data-group-head="group:default/ai-personas, group:default/guests"`, `data-group-head="group:default/ops"`,
		`data-group-head="group:default/platform-team"`, `data-group-head=""`}
	for i, hd := range heads {
		if idx(hd) < 0 {
			t.Fatalf("missing group header %s", hd)
		}
		if i > 0 && idx(heads[i-1]) > idx(hd) {
			t.Errorf("group %s before %s", hd, heads[i-1])
		}
	}
	if !(idx(heads[1]) < idx("ord-ops") && idx("ord-ops") < idx(heads[2])) {
		t.Error("ord-ops not under its group")
	}
	// html/template writes & in a JS string as &, which the browser reads as &.
	if !strings.Contains(by, "EventSource('ui/events?scope=all\\u0026sort=group')") {
		t.Errorf("events URL does not keep the view: %s", regexp.MustCompile(`EventSource\([^)]*\)`).FindString(by))
	}

	// Not an admin: own orders and the teams' (owner), not the persona's.
	bob := map[string]string{"X-Forwarded-User": "bob@sthings.example", "X-Forwarded-Groups": "backstage-team-platform-team"}
	_, bobs := get(t, h, "/orders/ui/?scope=all", bob)
	if !strings.Contains(bobs, "ord-ops") || !strings.Contains(bobs, "ord-platform") || strings.Contains(bobs, "ord-persona") ||
		!strings.Contains(bobs, "Yours and your teams") || !strings.Contains(bobs, "All I may see") {
		t.Error("non-admin all view: want own + team-owned only")
	}
}

// #27: the detail shows the auto-merge rule and when the order runs out.
func TestDetailShowsExpiry(t *testing.T) {
	o := order("ord-x", "user:default/alice", v1alpha1.PhaseReady)
	exp := metav1.NewTime(time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC))
	o.Spec.AutoMerge = &v1alpha1.OrderAutoMerge{Rule: "persona-test-vms", ExpiresAt: &exp}
	h, _ := handler(t, nil, o)
	code, body := get(t, h, "/orders/ui/orders/ord-x", nil)
	if code != 200 || !strings.Contains(body, "<code>persona-test-vms</code>") || !strings.Contains(body, "2026-11-24 13:00:00 UTC") {
		t.Errorf("%d\n%s", code, body)
	}
}
