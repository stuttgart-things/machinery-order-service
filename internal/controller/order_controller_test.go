package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/controller"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
)

func TestOrderLifecycle(t *testing.T) {
	e := start(t)
	e.cluster(t, true)
	ctx := context.Background()

	e.order(t, "ord-000000000001", `{"name":"demo","use_case":"dev"}`)
	o := e.wait(t, "ord-000000000001", "PROpen", phase(v1alpha1.PhasePROpen))

	// The service rendered with its own identity (no caller token).
	if len(e.renderer.Tokens) == 0 || e.renderer.Tokens[0] != "" {
		t.Errorf("tokens %v", e.renderer.Tokens)
	}
	if o.Status.XRRef == nil || o.Status.XRRef.Name != "demo-vm" || len(o.Status.Files) != 2 {
		t.Fatalf("status: %+v", o.Status)
	}
	pulls := e.git.Pulls()
	if len(pulls) != 1 || pulls[0].Head != "vm-demo" || pulls[0].Title != "feat(machinery): VM demo (dev, labda-vsphere) (MERGE = BAU)" {
		t.Fatalf("pulls %+v", pulls)
	}
	if !delivery.HasTrailer(e.git.Message(pulls[0].HeadSHA), "ord-000000000001") {
		t.Error("no Order-Id trailer")
	}
	xr, _ := e.git.File("vm-demo", "clusters/labda/vsphere/machinery-orders-xrs/demo.yaml")
	if !strings.Contains(xr, "stuttgart-things.com/order-id: ord-000000000001") {
		t.Errorf("xr file:\n%s", xr)
	}
	if k, _ := e.git.File("vm-demo", "clusters/labda/vsphere/machinery-orders-xrs/kustomization.yaml"); k != kust+"  - demo.yaml\n" {
		t.Errorf("kustomization %q", k)
	}

	merge := e.git.Merge(pulls[0].Number)
	o = e.wait(t, "ord-000000000001", "Merged", phase(v1alpha1.PhaseMerged))
	if o.Status.Delivery.MergeSHA != merge {
		t.Errorf("merge sha %q", o.Status.Delivery.MergeSHA)
	}

	// Flux at a revision before the merge: still Merged.
	e.machinery.Set("Kustomization", "flux-system", "machinery-orders-xrs", machinery.Resource{Info: map[string]string{"lastAppliedRevision": "main@sha1:" + pulls[0].HeadSHA}})
	e.wait(t, "ord-000000000001", "waiting for Flux", func(o *v1alpha1.Order) bool {
		return o.Status.Phase == v1alpha1.PhaseMerged && strings.Contains(o.Status.Message, "waiting for")
	})
	// Flux past the merge: Applied, then Building once machinery sees the XR.
	later := e.git.Advance()
	e.machinery.Set("Kustomization", "flux-system", "machinery-orders-xrs", machinery.Resource{Info: map[string]string{"lastAppliedRevision": "main@sha1:" + later}})
	e.wait(t, "ord-000000000001", "Applied", phase(v1alpha1.PhaseApplied))
	e.machinery.Set("NativeVsphereVM", "default", "demo-vm", machinery.Resource{StatusMessage: "cloning"})
	e.wait(t, "ord-000000000001", "Building", phase(v1alpha1.PhaseBuilding))
	e.machinery.Set("NativeVsphereVM", "default", "demo-vm", machinery.Resource{Ready: true, Address: "10.31.103.7"})
	o = e.wait(t, "ord-000000000001", "Ready", phase(v1alpha1.PhaseReady))
	if o.Status.Live == nil || o.Status.Live.Address != "10.31.103.7" {
		t.Errorf("live %+v", o.Status.Live)
	}
	if e.machinery.Dialed[0].Token != "s3cret" || !e.machinery.Dialed[0].Insecure {
		t.Errorf("dialed %+v", e.machinery.Dialed[0])
	}
	var seen []v1alpha1.Phase
	for _, h := range o.Status.History {
		seen = append(seen, h.Phase)
	}
	want := []v1alpha1.Phase{"Ordered", "Placed", "PROpen", "Merged", "Applied", "Building", "Ready"}
	if len(seen) != len(want) {
		t.Fatalf("history %v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("history %v", seen)
		}
	}

	// Deleting a merged order touches nothing: the finalizer just goes.
	if err := e.c.Delete(ctx, o); err != nil {
		t.Fatal(err)
	}
	waitGone(t, e, "ord-000000000001")
	if _, ok := e.git.File("main", "clusters/labda/vsphere/machinery-orders-xrs/demo.yaml"); !ok {
		t.Error("the order's file is gone from main")
	}
}

func TestCancelAndClose(t *testing.T) {
	e := start(t)
	e.cluster(t, false)
	ctx := context.Background()

	// Cancel before the merge closes the pull request.
	e.order(t, "ord-000000000002", `{"name":"one"}`)
	o := e.wait(t, "ord-000000000002", "PROpen", phase(v1alpha1.PhasePROpen))
	o.Spec.Cancel = true
	if err := e.c.Update(ctx, o); err != nil {
		t.Fatal(err)
	}
	e.wait(t, "ord-000000000002", "Cancelled", phase(v1alpha1.PhaseCancelled))
	if p := e.git.Pulls()[0]; p.State != "closed" || e.git.Head("vm-one") != "" {
		t.Errorf("pull %+v head %q", p, e.git.Head("vm-one"))
	}

	// A pull request closed on GitHub ends the order Cancelled.
	e.order(t, "ord-000000000003", `{"name":"two"}`)
	o = e.wait(t, "ord-000000000003", "PROpen", phase(v1alpha1.PhasePROpen))
	e.git.ClosePR(o.Status.Delivery.PullRequest.Number)
	e.wait(t, "ord-000000000003", "Cancelled", phase(v1alpha1.PhaseCancelled))

	// Deleting an order with an open pull request closes it.
	e.order(t, "ord-000000000004", `{"name":"three"}`)
	o = e.wait(t, "ord-000000000004", "PROpen", phase(v1alpha1.PhasePROpen))
	if err := e.c.Delete(ctx, o); err != nil {
		t.Fatal(err)
	}
	waitGone(t, e, "ord-000000000004")
	for _, p := range e.git.Pulls() {
		if p.Head == "vm-three" && p.State != "closed" {
			t.Errorf("pull of the deleted order still open: %+v", p)
		}
	}

	// Without a Flux check, a merged order goes on once machinery sees the XR.
	e.order(t, "ord-000000000005", `{"name":"four"}`)
	o = e.wait(t, "ord-000000000005", "PROpen", phase(v1alpha1.PhasePROpen))
	e.git.Merge(o.Status.Delivery.PullRequest.Number)
	e.wait(t, "ord-000000000005", "Merged", phase(v1alpha1.PhaseMerged))
	e.machinery.Set("NativeVsphereVM", "default", "four-vm", machinery.Resource{Ready: true})
	e.wait(t, "ord-000000000005", "Ready", phase(v1alpha1.PhaseReady))
}

func TestFailures(t *testing.T) {
	e := start(t)
	e.cluster(t, false)

	// A reserved parameter fails the order.
	e.order(t, "ord-000000000006", `{"name":"x","order_id":"forged"}`)
	o := e.wait(t, "ord-000000000006", "Failed", phase(v1alpha1.PhaseFailed))
	if !strings.Contains(o.Status.Message, "order_id") {
		t.Errorf("message %q", o.Status.Message)
	}

	// Two orders for the same name: the second fails on the taken branch.
	e.order(t, "ord-000000000007", `{"name":"same"}`)
	e.wait(t, "ord-000000000007", "PROpen", phase(v1alpha1.PhasePROpen))
	e.order(t, "ord-000000000008", `{"name":"same"}`)
	o = e.wait(t, "ord-000000000008", "Failed", phase(v1alpha1.PhaseFailed))
	if !strings.Contains(o.Status.Message, "vm-same") {
		t.Errorf("message %q", o.Status.Message)
	}
}

func TestRevisionSHA(t *testing.T) {
	for in, want := range map[string]string{"main@sha1:abc": "abc", "sha1:abc": "abc", "abc": "abc", "main@abc": "abc"} {
		if got := controller.RevisionSHA(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

func waitGone(t *testing.T, e *env, name string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		err := e.c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &v1alpha1.Order{})
		if apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("order %s not deleted", name)
}
