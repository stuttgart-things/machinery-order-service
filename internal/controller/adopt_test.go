package controller_test

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/adopt"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
)

// TestAdoptLifecycle: an adopted XR goes through a pull request that only
// replaces its file, then the order follows the XR to Ready; a file that
// changed on main before the pull request fails the order instead.
func TestAdoptLifecycle(t *testing.T) {
	e := start(t)
	e.cluster(t, false)
	ctx := context.Background()
	dir := "clusters/labda/vsphere/machinery-xrs/"
	xr := func(name string) string {
		return "---\napiVersion: resources.stuttgart-things.com/v1alpha1\nkind: NativeVsphereVM\nmetadata:\n  name: " + name +
			"\n  namespace: default\n  annotations:\n    stuttgart-things.com/source-path: " + dir + name + ".yaml\nspec:\n  vm:\n    name: " + name + "\n"
	}
	kust := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - hand.yaml\n  - moved.yaml\n"
	hand, moved := xr("hand"), xr("moved")
	e.git.Commit("main", "hand-written", map[string]*string{dir + "hand.yaml": &hand, dir + "moved.yaml": &moved, dir + "kustomization.yaml": &kust})
	e.machinery.Set("NativeVsphereVM", "default", "hand", machinery.Resource{Ready: true, Address: "10.31.103.9"})

	mc := &v1alpha1.ManagementCluster{}
	if err := e.c.Get(ctx, client.ObjectKey{Name: "machinery"}, mc); err != nil {
		t.Fatal(err)
	}
	res, err := adopt.Plan(ctx, e.c, e.gh, e.renderer, mc, "default", adopt.Request{ManagementCluster: "machinery", Path: dir + "hand.yaml"}, "user:default/root", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.c.Create(ctx, res.Order); err != nil {
		t.Fatal(err)
	}
	e.wait(t, res.OrderID, "PROpen", phase(v1alpha1.PhasePROpen))
	var pr int
	for _, p := range e.git.Pulls() {
		if p.Head == "adopt/hand" {
			pr = p.Number
			if !strings.HasPrefix(p.Title, "chore(machinery): adopt NativeVsphereVM hand as order ") {
				t.Errorf("title %q", p.Title)
			}
		}
	}
	if pr == 0 {
		t.Fatalf("pulls %+v", e.git.Pulls())
	}
	got, _ := e.git.File("adopt/hand", dir+"hand.yaml")
	if got != res.Order.Spec.Rendered.Files[0].Content || !strings.Contains(got, "    stuttgart-things.com/order-id: "+res.OrderID+"\n") {
		t.Fatalf("file on the branch:\n%s", got)
	}
	if k, _ := e.git.File("adopt/hand", dir+"kustomization.yaml"); k != kust {
		t.Errorf("kustomization touched: %q", k)
	}
	e.git.Merge(pr)
	o := e.wait(t, res.OrderID, "Ready", phase(v1alpha1.PhaseReady))
	if o.Status.XRRef == nil || o.Status.XRRef.Name != "hand" || o.Status.Live.Address != "10.31.103.9" {
		t.Fatalf("status %+v", o.Status)
	}

	// The file changes on main after the adopt, before the pull request.
	res, err = adopt.Plan(ctx, e.c, e.gh, e.renderer, mc, "default", adopt.Request{ManagementCluster: "machinery", Path: dir + "moved.yaml"}, "user:default/root", "")
	if err != nil {
		t.Fatal(err)
	}
	changed := moved + "# edited by hand\n"
	e.git.Commit("main", "edit", map[string]*string{dir + "moved.yaml": &changed})
	if err := e.c.Create(ctx, res.Order); err != nil {
		t.Fatal(err)
	}
	o = e.wait(t, res.OrderID, "Failed", phase(v1alpha1.PhaseFailed))
	if !strings.Contains(o.Status.Message, "changed on main since it was adopted") {
		t.Errorf("message %q", o.Status.Message)
	}
}
