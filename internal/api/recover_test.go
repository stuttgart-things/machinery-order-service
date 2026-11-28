package api_test

import (
	"context"
	"net/http"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery/githubfake"
)

const oldOrder = `---
apiVersion: resources.stuttgart-things.com/v1alpha1
kind: NativeVsphereVM
metadata:
  name: old-vm
  namespace: default
  labels:
    stuttgart-things.com/order-id: ord-0000000000aa
  annotations:
    stuttgart-things.com/management-cluster: machinery
    stuttgart-things.com/source-path: clusters/labda/vsphere/machinery-orders-xrs/old.yaml
    stuttgart-things.com/requested-by: user:default/alice
    stuttgart-things.com/order-id: ord-0000000000aa
spec:
  vm:
    name: old
`

func TestRecover(t *testing.T) {
	s := newSetup(t, true)
	alice := s.token(t, "alice")
	admin := s.token(t, "root", "platform-admin")
	body := map[string]any{"managementCluster": "machinery"}

	if rec, _ := s.do(t, "POST", "/api/v1/recover", admin, body); rec.Code != http.StatusNotImplemented {
		t.Errorf("without git: %d", rec.Code)
	}

	git := githubfake.New("stuttgart-things", "stuttgart-things", nil)
	t.Cleanup(git.Close)
	p := "clusters/labda/vsphere/machinery-orders-xrs/old.yaml"
	content := oldOrder
	git.Commit("main", "feat(machinery): VM old\n\nOrder-Id: ord-0000000000aa\n", map[string]*string{p: &content})
	gh, err := delivery.New(git.URL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	s.srv.Git = gh

	if rec, _ := s.do(t, "POST", "/api/v1/recover", alice, body); rec.Code != http.StatusForbidden {
		t.Errorf("alice: %d", rec.Code)
	}
	if rec, _ := s.do(t, "POST", "/api/v1/recover", admin, map[string]any{"managementCluster": "nope"}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown cluster: %d", rec.Code)
	}

	dry := map[string]any{"managementCluster": "machinery", "dryRun": true}
	rec, out := s.do(t, "POST", "/api/v1/recover", admin, dry)
	if rec.Code != 200 {
		t.Fatalf("dry run: %d %v", rec.Code, out)
	}
	results := out["results"].([]any)
	got := results[0].(map[string]any)["recovered"].([]any)
	if len(got) != 1 || got[0].(map[string]any)["orderID"] != "ord-0000000000aa" ||
		got[0].(map[string]any)["template"] != "recovered:NativeVsphereVM" {
		t.Fatalf("dry run: %v", out)
	}
	ctx := context.Background()
	o := &v1alpha1.Order{}
	if err := s.c.Get(ctx, client.ObjectKey{Namespace: "orders", Name: "ord-0000000000aa"}, o); err == nil {
		t.Fatal("dry run created the Order")
	}

	if rec, out := s.do(t, "POST", "/api/v1/recover", admin, body); rec.Code != 200 {
		t.Fatalf("recover: %d %v", rec.Code, out)
	}
	if err := s.c.Get(ctx, client.ObjectKey{Namespace: "orders", Name: "ord-0000000000aa"}, o); err != nil {
		t.Fatal(err)
	}
	r := o.Spec.Recovered
	if r == nil || r.By != "user:default/root" || r.Path != p || r.CommitSHA != git.Head("main") ||
		o.Spec.RequestedBy != "user:default/alice" || o.Labels[v1alpha1.OrderIDKey] != "ord-0000000000aa" ||
		o.Spec.Rendered == nil || len(o.Spec.Rendered.Files) != 1 || o.Spec.Rendered.Files[0].Content != oldOrder {
		t.Fatalf("order %+v", o.Spec)
	}

	// Once the Order is there, a second run leaves it alone.
	_, out = s.do(t, "POST", "/api/v1/recover", admin, body)
	r0 := out["results"].([]any)[0].(map[string]any)
	if r0["existing"].(float64) != 1 || len(r0["recovered"].([]any)) != 0 {
		t.Errorf("second run: %v", out)
	}
}
