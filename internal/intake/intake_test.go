package intake_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/intake"
	"github.com/stuttgart-things/machinery-order-service/internal/render/rendertest"
)

func order(t *testing.T, params map[string]any) *v1alpha1.Order {
	t.Helper()
	raw, err := intake.Encode(params)
	if err != nil {
		t.Fatal(err)
	}
	return &v1alpha1.Order{
		ObjectMeta: metav1.ObjectMeta{Name: "ord-0123456789ab"},
		Spec: v1alpha1.OrderSpec{
			Template: "native-vm-labda-vsphere", Parameters: raw, RequestedBy: "user:default/sthings",
			Placement: v1alpha1.Placement{ManagementCluster: "machinery"},
		},
	}
}

func cluster() *v1alpha1.ManagementCluster {
	return &v1alpha1.ManagementCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "machinery"},
		Spec: v1alpha1.ManagementClusterSpec{
			GitOps: v1alpha1.GitOps{OrdersPath: "clusters/labda/vsphere/machinery-orders-xrs"},
		},
	}
}

func TestRenderAndConvention(t *testing.T) {
	f := &rendertest.Fake{}
	o := order(t, map[string]any{"name": "demo", "use_case": "dev"})
	rd, err := intake.Render(context.Background(), f, o, cluster(), "caller")
	if err != nil {
		t.Fatal(err)
	}
	if f.Tokens[0] != "caller" {
		t.Errorf("token %q", f.Tokens[0])
	}
	paths := intake.Paths(rd)
	if len(paths) != 2 || paths[0] != "clusters/labda/vsphere/machinery-orders-xrs/demo.yaml" {
		t.Errorf("paths %v", paths)
	}
	if !strings.Contains(rd.Files[0].Content, "stuttgart-things.com/order-id: ord-0123456789ab") {
		t.Errorf("order-id missing:\n%s", rd.Files[0].Content)
	}
	ref, err := intake.XRRef(rd)
	if err != nil || ref.Kind != "NativeVsphereVM" || ref.Name != "demo-vm" || ref.Namespace != "default" {
		t.Errorf("xr %+v %v", ref, err)
	}
	tmpl, _ := f.Template(context.Background(), o.Spec.Template, "")
	conv, err := intake.ConventionFor(o, rd, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if conv.Branch != "vm-demo" || conv.Title != "feat(machinery): VM demo (dev, labda-vsphere) (MERGE = BAU)" {
		t.Errorf("convention %+v", conv)
	}
	if !strings.Contains(conv.Body, "Order ID: `ord-0123456789ab`") {
		t.Errorf("body: %s", conv.Body)
	}
}

func TestParametersReserved(t *testing.T) {
	var ie *intake.InvalidError
	for _, k := range intake.Reserved {
		_, err := intake.Parameters(order(t, map[string]any{"name": "x", k: "y"}), cluster())
		if !errors.As(err, &ie) {
			t.Errorf("%s: want InvalidError, got %v", k, err)
		}
	}
	if _, err := intake.Parameters(order(t, map[string]any{}), cluster()); !errors.As(err, &ie) {
		t.Errorf("missing name: %v", err)
	}
	if _, err := intake.Parameters(order(t, map[string]any{"name": "../x"}), cluster()); !errors.As(err, &ie) {
		t.Errorf("bad name: %v", err)
	}
	p, err := intake.Parameters(order(t, map[string]any{"name": "x"}), cluster())
	if err != nil || p["management_cluster"] != "machinery" || p["order_id"] != "ord-0123456789ab" || p["source_path"] != "clusters/labda/vsphere/machinery-orders-xrs/x.yaml" {
		t.Errorf("params %v %v", p, err)
	}
	if _, ok := p["environment_config"]; ok {
		t.Error("environment_config set without the cluster naming one")
	}
}

// The orderer goes into the render as requested_by, so the VM file carries
// the requested-by and owner annotations (#5).
func TestRenderRequestedBy(t *testing.T) {
	f := &rendertest.Fake{}
	o := order(t, map[string]any{"name": "demo", "owner": "group:default/platform-team"})
	rd, err := intake.Render(context.Background(), f, o, cluster(), "caller")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"stuttgart-things.com/requested-by: user:default/sthings",
		"stuttgart-things.com/owner: group:default/platform-team",
	} {
		if !strings.Contains(rd.Files[0].Content, want) {
			t.Errorf("%s missing:\n%s", want, rd.Files[0].Content)
		}
	}

	// The same orderer may be given; another one is refused.
	if _, err := intake.Parameters(order(t, map[string]any{"name": "x", "requested_by": "user:default/sthings"}), cluster()); err != nil {
		t.Errorf("same requested_by: %v", err)
	}
	var ie *intake.InvalidError
	if _, err := intake.Parameters(order(t, map[string]any{"name": "x", "requested_by": "user:default/other"}), cluster()); !errors.As(err, &ie) {
		t.Errorf("other requested_by: want InvalidError, got %v", err)
	}

	// Without spec.requestedBy the service adds nothing.
	o = order(t, map[string]any{"name": "x"})
	o.Spec.RequestedBy = ""
	if p, err := intake.Parameters(o, cluster()); err != nil || p["requested_by"] != nil {
		t.Errorf("params %v %v", p, err)
	}
}

// With the service's own token, a requested_by the template binds to the
// login is left to claim-machinery-api; with the orderer's it is sent.
func TestRenderRequestedByIdentityBound(t *testing.T) {
	f := &rendertest.Fake{Identity: map[string]string{"requested_by": "user"}}
	o := order(t, map[string]any{"name": "demo"})
	if _, err := intake.Render(context.Background(), f, o, cluster(), ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Params[0]["requested_by"]; ok {
		t.Errorf("service token: requested_by sent: %v", f.Params[0])
	}
	if _, err := intake.Render(context.Background(), f, o, cluster(), "caller"); err != nil {
		t.Fatal(err)
	}
	if f.Params[1]["requested_by"] != "user:default/sthings" {
		t.Errorf("caller token: params %v", f.Params[1])
	}
}

func TestXREnvironment(t *testing.T) {
	xr := func(spec string) *v1alpha1.Rendered {
		return &v1alpha1.Rendered{Files: []v1alpha1.RenderedFile{{
			Path: "clusters/labul/proxmox/machinery-labul-orders-xrs/demo.yaml",
			Content: "apiVersion: resources.stuttgart-things.com/v1alpha1\nkind: NativeProxmoxVM\nmetadata:\n  name: demo-vm\n" +
				"  annotations:\n    stuttgart-things.com/source-path: clusters/labul/proxmox/machinery-labul-orders-xrs/demo.yaml\n" + spec,
		}}}
	}
	for _, tc := range []struct{ name, spec, want string }{
		{"set", "spec:\n  environmentConfig: labul\n  vm:\n    cpu: 2\n", "labul"},
		{"unset", "spec:\n  vm:\n    cpu: 2\n", ""},
		{"not a string", "spec:\n  environmentConfig:\n    name: labul\n", ""},
		{"no spec", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := intake.XREnvironment(xr(tc.spec)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	// no document names its own file
	if got := intake.XREnvironment(&v1alpha1.Rendered{Files: []v1alpha1.RenderedFile{{Path: "x.yaml", Content: "kind: ConfigMap\n"}}}); got != "" {
		t.Errorf("no XR: %q", got)
	}
}
