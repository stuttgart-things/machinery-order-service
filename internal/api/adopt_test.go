package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery/githubfake"
)

const xrsDir = "clusters/labda/vsphere/machinery-xrs/"

func handXR(kind, name string) string {
	return `---
# hand-written, kept as it is
apiVersion: resources.stuttgart-things.com/v1alpha1
kind: ` + kind + `
metadata:
  name: ` + name + `
  namespace: default
  annotations:
    stuttgart-things.com/management-cluster: machinery
    stuttgart-things.com/source-path: ` + xrsDir + name + `.yaml
    kustomize.toolkit.fluxcd.io/prune: disabled
spec:
  vm:
    name: ` + name + ` # trailing comment
`
}

func TestAdopt(t *testing.T) {
	s := newSetup(t, true)
	alice := s.token(t, "alice")
	admin := s.token(t, "root", "platform-admin")
	ctx := context.Background()

	vm, bucket, excluded := handXR("NativeVsphereVM", "hand-vm"), handXR("MinioBucket", "data"), handXR("NativeVsphereVM", "keep-out")
	owned := strings.Replace(handXR("NativeVsphereVM", "owned"), "  annotations:\n", "  annotations:\n    stuttgart-things.com/order-id: ord-0000000000cc\n", 1)
	stack := strings.Replace(handXR("ClusterStack", "stack"), "resources.stuttgart-things.com", "config.stuttgart-things.com", 1)
	git := githubfake.New("stuttgart-things", "stuttgart-things", map[string]string{
		xrsDir + "hand-vm.yaml": vm, xrsDir + "data.yaml": bucket, xrsDir + "keep-out.yaml": excluded, xrsDir + "owned.yaml": owned,
		xrsDir + "stack.yaml": stack,
	})
	t.Cleanup(git.Close)
	gh, err := delivery.New(git.URL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	s.srv.Git = gh
	mc := &v1alpha1.ManagementCluster{}
	if err := s.c.Get(ctx, client.ObjectKey{Name: "machinery"}, mc); err != nil {
		t.Fatal(err)
	}
	mc.Spec.AdoptExclude = []string{"keep-out"}
	mc.Spec.AdoptKinds = []string{"NativeVsphereVM", "MinioBucket"}
	mc.Spec.AdoptPaths = []string{xrsDir}
	if err := s.c.Update(ctx, mc); err != nil {
		t.Fatal(err)
	}
	// a cluster without adoptKinds: adopt is off there
	off := &v1alpha1.ManagementCluster{ObjectMeta: metav1.ObjectMeta{Name: "off"},
		Spec: v1alpha1.ManagementClusterSpec{AdoptPaths: []string{xrsDir}}}
	if err := s.c.Create(ctx, off); err != nil {
		t.Fatal(err)
	}

	req := func(path string, extra map[string]any) map[string]any {
		b := map[string]any{"managementCluster": "machinery", "path": xrsDir + path}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	for name, c := range map[string]struct {
		token string
		body  map[string]any
		want  int
		msg   string
	}{
		"not admin":      {alice, req("hand-vm.yaml", nil), 403, "admins"},
		"adopt off":      {admin, map[string]any{"managementCluster": "off", "path": xrsDir + "hand-vm.yaml"}, 403, "adoptKinds is empty"},
		"kind not given": {admin, req("stack.yaml", nil), 403, "adopts no ClusterStack.config.stuttgart-things.com"},
		"not adoptPaths": {admin, map[string]any{"managementCluster": "machinery", "path": "clusters/labda/vsphere/other/x.yaml"}, 400, "spec.adoptPaths"},
		"excluded":       {admin, req("keep-out.yaml", nil), 403, "adoptExclude"},
		"an order":       {admin, req("owned.yaml", nil), 409, "ord-0000000000cc"},
		"orders dir":     {admin, map[string]any{"managementCluster": "machinery", "path": "clusters/labda/vsphere/machinery-orders-xrs/x.yaml"}, 400, "orders directory"},
		"missing":        {admin, req("nope.yaml", nil), 400, "not on main"},
		"kind mismatch":  {admin, req("data.yaml", map[string]any{"template": "native-vm-labda-vsphere"}), 400, "renders NativeVsphereVM"},
		"unknown fields": {admin, req("hand-vm.yaml", map[string]any{"owner": "x"}), 400, "unknown field"},
	} {
		rec, out := s.do(t, "POST", "/api/v1/adopt", c.token, c.body)
		if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.msg) {
			t.Errorf("%s: %d %v", name, rec.Code, out)
		}
	}

	// dry run: what would be added, nothing created
	rec, out := s.do(t, "POST", "/api/v1/adopt", admin, req("hand-vm.yaml", map[string]any{"template": "native-vm-labda-vsphere", "dryRun": true}))
	if rec.Code != 200 || len(out["added"].([]any)) != 3 || out["template"] != "native-vm-labda-vsphere" {
		t.Fatalf("dry run: %d %v", rec.Code, out)
	}
	var list v1alpha1.OrderList
	if err := s.c.List(ctx, &list); err != nil || len(list.Items) != 0 {
		t.Fatalf("dry run created %d orders (%v)", len(list.Items), err)
	}

	// without template: adopted:<Kind>
	rec, out = s.do(t, "POST", "/api/v1/adopt", admin, req("hand-vm.yaml", map[string]any{"reason": "take it over"}))
	if rec.Code != 201 || out["template"] != "adopted:NativeVsphereVM" {
		t.Fatalf("adopt: %d %v", rec.Code, out)
	}
	id := out["orderID"].(string)
	o := &v1alpha1.Order{}
	if err := s.c.Get(ctx, client.ObjectKey{Namespace: "orders", Name: id}, o); err != nil {
		t.Fatal(err)
	}
	if o.Spec.Adopted == nil || o.Spec.Adopted.Path != xrsDir+"hand-vm.yaml" || o.Spec.Adopted.By != "user:default/root" ||
		o.Spec.RequestedBy != "user:default/root" || o.Spec.Template != "adopted:NativeVsphereVM" || len(o.Spec.Rendered.Files) != 1 {
		t.Fatalf("order %+v", o.Spec)
	}
	// The file: exactly the three lines added, everything else as it was.
	got := strings.Split(o.Spec.Rendered.Files[0].Content, "\n")
	var added []string
	i := 0
	for _, l := range strings.Split(vm, "\n") {
		for got[i] != l {
			added = append(added, got[i])
			i++
		}
		i++
	}
	want := []string{"  labels:", "    stuttgart-things.com/order-id: " + id, "    stuttgart-things.com/template: adopted:NativeVsphereVM", "    stuttgart-things.com/order-id: " + id}
	if strings.Join(added, "\n") != strings.Join(want, "\n") || i != len(got) {
		t.Fatalf("added %q, file:\n%s", added, o.Spec.Rendered.Files[0].Content)
	}

	// The same file again: refused while that order adopts it.
	if rec, _ := s.do(t, "POST", "/api/v1/adopt", admin, req("hand-vm.yaml", nil)); rec.Code != 409 {
		t.Errorf("second adopt: %d", rec.Code)
	}
}
