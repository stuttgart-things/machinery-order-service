package controller_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
	"github.com/stuttgart-things/machinery-order-service/internal/recovery"
)

// TestRecoverOrders is #4823's acceptance: once every Order CR is gone, the
// service rebuilds them from the fake git remote and the fake machinery,
// with the same order IDs and the right phase.
func TestRecoverOrders(t *testing.T) {
	e := start(t)
	e.cluster(t, false)
	ctx := context.Background()
	dir := "clusters/labda/vsphere/machinery-orders-xrs/"

	// one order Ready, one (with a file outside the orders directory)
	// merged and still building
	e.order(t, "ord-000000000001", `{"name":"demo"}`)
	e.order(t, "ord-000000000002", `{"name":"two","use_case":"dev"}`)
	e.wait(t, "ord-000000000001", "PROpen", phase(v1alpha1.PhasePROpen))
	e.wait(t, "ord-000000000002", "PROpen", phase(v1alpha1.PhasePROpen))
	for _, pr := range e.git.Pulls() {
		e.git.Merge(pr.Number)
	}
	e.machinery.Set("NativeVsphereVM", "default", "demo-vm", machinery.Resource{Ready: true, Address: "10.31.103.7"})
	e.machinery.Set("NativeVsphereVM", "default", "two-vm", machinery.Resource{StatusMessage: "cloning"})
	e.wait(t, "ord-000000000001", "Ready", phase(v1alpha1.PhaseReady))
	e.wait(t, "ord-000000000002", "Building", phase(v1alpha1.PhaseBuilding))

	xr, _ := e.git.File("main", dir+"demo.yaml")
	if !strings.Contains(xr, "stuttgart-things.com/template: native-vm-labda-vsphere\n") {
		t.Fatalf("no template annotation:\n%s", xr)
	}

	// An order file from before the template annotation, committed straight
	// to main, and a hand-written XR without order-id.
	old := strings.ReplaceAll(strings.ReplaceAll(xr, "demo", "old"), "ord-000000000001", "ord-0000000000aa")
	old = strings.Replace(old, "    stuttgart-things.com/template: native-vm-labda-vsphere\n", "", 1)
	hand := "apiVersion: resources.stuttgart-things.com/v1alpha1\nkind: NativeVsphereVM\nmetadata:\n  name: hand-vm\n  annotations:\n    stuttgart-things.com/source-path: " + dir + "hand.yaml\nspec: {}\n"
	e.git.Commit("main", "feat(machinery): VM old\n\nOrder-Id: ord-0000000000aa\n", map[string]*string{dir + "old.yaml": &old, dir + "hand.yaml": &hand})

	// Every Order CR goes.
	for _, id := range []string{"ord-000000000001", "ord-000000000002"} {
		if err := e.c.Delete(ctx, &v1alpha1.Order{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: "default"}}); err != nil {
			t.Fatal(err)
		}
		e.gone(t, id)
	}

	gh, err := delivery.New(e.git.URL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	mc := &v1alpha1.ManagementCluster{}
	if err := e.c.Get(ctx, client.ObjectKey{Name: "machinery"}, mc); err != nil {
		t.Fatal(err)
	}

	// A dry run creates nothing.
	res, err := recovery.Run(ctx, e.c, gh, mc, "default", "user:default/root", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recovered) != 3 || len(res.Skipped) != 0 || res.Existing != 0 {
		t.Fatalf("dry run %+v", res)
	}
	if err := e.c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ord-000000000001"}, &v1alpha1.Order{}); !apierrors.IsNotFound(err) {
		t.Fatalf("dry run created an Order: %v", err)
	}

	res, err = recovery.Run(ctx, e.c, gh, mc, "default", "user:default/root", false)
	if err != nil || len(res.Recovered) != 3 {
		t.Fatalf("recover %+v %v", res, err)
	}

	o := e.wait(t, "ord-000000000001", "Ready again", phase(v1alpha1.PhaseReady))
	if o.Status.History[0].Phase != v1alpha1.PhaseRecovered || o.Spec.Template != "native-vm-labda-vsphere" ||
		o.Status.XRRef == nil || o.Status.XRRef.Name != "demo-vm" || o.Status.Live == nil || o.Status.Live.Address != "10.31.103.7" ||
		o.Status.Delivery == nil || o.Status.Delivery.PullRequest == nil || o.Status.Delivery.PullRequest.Number != 1 ||
		!strings.Contains(o.Status.History[0].Message, "by user:default/root") {
		t.Fatalf("recovered demo: %+v", o.Status)
	}
	o = e.wait(t, "ord-000000000002", "Building again", phase(v1alpha1.PhaseBuilding))
	if len(o.Status.Files) != 2 || o.Status.Files[0] != dir+"two.yaml" ||
		o.Status.Files[1] != "clusters/labda/vsphere/sthings-infra/monitoring/host-two.yaml" {
		t.Fatalf("recovered two: files %v", o.Status.Files)
	}
	// The old order: template unknown, no pull request, XR not seen yet.
	o = e.wait(t, "ord-0000000000aa", "Recovered", phase(v1alpha1.PhaseRecovered))
	if o.Spec.Template != recovery.TemplateUnknown+"NativeVsphereVM" || o.Status.Delivery.PullRequest != nil ||
		o.Status.Delivery.MergeSHA == "" {
		t.Fatalf("recovered old: %+v %+v", o.Spec, o.Status.Delivery)
	}
	e.machinery.Set("NativeVsphereVM", "default", "old-vm", machinery.Resource{Ready: true})
	e.wait(t, "ord-0000000000aa", "old Ready", phase(v1alpha1.PhaseReady))

	// A second run finds them all.
	res, err = recovery.Run(ctx, e.c, gh, mc, "default", "user:default/root", false)
	if err != nil || len(res.Recovered) != 0 || res.Existing != 3 {
		t.Fatalf("second run %+v %v", res, err)
	}

	// A recovered order tears down like any other: both files go.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &v1alpha1.Order{}
		if err := e.c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ord-000000000002"}, o); err != nil {
			return err
		}
		o.Spec.Teardown = &v1alpha1.TeardownRequest{Attempt: 1, RequestedBy: "user:default/root"}
		return e.c.Update(ctx, o)
	}); err != nil {
		t.Fatal(err)
	}
	e.wait(t, "ord-000000000002", "TeardownPROpen", phase(v1alpha1.PhaseTeardownPROpen))
	if _, ok := e.git.File("delete-claim/two-vm", dir+"two.yaml"); ok {
		t.Error("teardown keeps the XR file")
	}
	if _, ok := e.git.File("delete-claim/two-vm", "clusters/labda/vsphere/sthings-infra/monitoring/host-two.yaml"); ok {
		t.Error("teardown keeps the file outside the orders directory")
	}
}

func (e *env) gone(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		err := e.c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &v1alpha1.Order{})
		if apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("Order %s still there", name)
}
