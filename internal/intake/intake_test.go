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
