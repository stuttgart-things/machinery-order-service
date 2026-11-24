package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/controller"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery/githubfake"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
)

// expiringOrder creates an order under rule ttl that runs out after d.
func expiringOrder(t *testing.T, e *env, id, name string, d time.Duration) {
	t.Helper()
	exp := metav1.NewTime(time.Now().Add(d))
	o := &v1alpha1.Order{
		ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: "default"},
		Spec: v1alpha1.OrderSpec{
			Template:    "native-vm-labda-vsphere",
			Parameters:  &runtime.RawExtension{Raw: []byte(`{"name":"` + name + `","size":"small"}`)},
			Placement:   v1alpha1.Placement{ManagementCluster: "machinery"},
			RequestedBy: "user:default/persona-developer",
			AutoMerge:   &v1alpha1.OrderAutoMerge{Rule: "ttl", ExpiresAt: &exp},
		},
	}
	if err := e.c.Create(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}

func TestExpiry(t *testing.T) {
	e := start(t)
	e.cluster(t, false)
	ctx := context.Background()
	mc := &v1alpha1.ManagementCluster{}
	if err := e.c.Get(ctx, client.ObjectKey{Name: "machinery"}, mc); err != nil {
		t.Fatal(err)
	}
	// maxLifetime without the teardown action is refused.
	mc.Spec.AutoMerge = &v1alpha1.AutoMerge{Rules: []v1alpha1.AutoMergeRule{{
		Name: "ttl", Groups: []string{"group:default/ai-personas"},
		MaxLifetime: &metav1.Duration{Duration: time.Hour},
	}}}
	if err := e.c.Update(ctx, mc); err == nil || !strings.Contains(err.Error(), "maxLifetime needs the teardown action") {
		t.Fatalf("maxLifetime without teardown: %v", err)
	}
	// Teardowns only: the order's own PR waits for a human (the test merges
	// it), the expiry teardown is merged by the service.
	mc.Spec.AutoMerge.Rules[0].Actions = []v1alpha1.AutoMergeAction{v1alpha1.ActionTeardown}
	if err := e.c.Update(ctx, mc); err != nil {
		t.Fatal(err)
	}

	// Ready when it runs out: torn down by the service.
	const id = "ord-0000000000e1"
	expiringOrder(t, e, id, "ttl", 6*time.Second)
	o := e.wait(t, id, "PROpen", phase(v1alpha1.PhasePROpen))
	e.git.Merge(o.Status.Delivery.PullRequest.Number)
	e.machinery.Set("NativeVsphereVM", "default", "ttl-vm", machinery.Resource{Ready: true, Address: "10.0.0.7"})
	o = e.wait(t, id, "Ready", phase(v1alpha1.PhaseReady))
	if !strings.Contains(o.Status.Message, "(expires ") {
		t.Errorf("message without the expiry: %q", o.Status.Message)
	}
	o = e.wait(t, id, "TeardownPROpen", phase(v1alpha1.PhaseTeardownPROpen))
	td := o.Spec.Teardown
	if td == nil || td.RequestedBy != controller.ServiceRequester || td.AutoMerge == nil || td.AutoMerge.Rule != "ttl" ||
		!strings.HasPrefix(td.Reason, "expired: ") {
		t.Fatalf("teardown %+v", td)
	}
	if h := o.Status.History[len(o.Status.History)-1]; h.Phase != v1alpha1.PhaseTeardownPROpen || !strings.Contains(h.Message, "expired: ") {
		t.Errorf("history %+v", h)
	}
	pr := pullOn(e, "delete-claim/ttl-vm")
	if !strings.Contains(pr.Body, "**Reason:** expired: ") || !strings.Contains(pr.Body, controller.ServiceRequester) {
		t.Errorf("teardown PR body %s", pr.Body)
	}
	e.git.SetChecks(pr.HeadSHA, githubfake.Check{Name: "Verify-Claim-Deletion", Status: "completed", Conclusion: "success"})
	e.wait(t, id, "TearingDown", phase(v1alpha1.PhaseTearingDown))
	e.machinery.Delete("NativeVsphereVM", "default", "ttl-vm")
	if o = e.wait(t, id, "Deleted", phase(v1alpha1.PhaseDeleted)); !o.TornDown() {
		t.Errorf("teardown %+v", o.Status.Teardown)
	}

	// Still PROpen when it runs out: cancelled, the PR closed.
	const early = "ord-0000000000e2"
	expiringOrder(t, e, early, "early", 3*time.Second)
	e.wait(t, early, "PROpen", phase(v1alpha1.PhasePROpen))
	o = e.wait(t, early, "Cancelled", phase(v1alpha1.PhaseCancelled))
	if !strings.HasPrefix(o.Status.Message, "expired: ") || o.Spec.Teardown != nil {
		t.Errorf("message %q teardown %+v", o.Status.Message, o.Spec.Teardown)
	}
	if p := pullOn(e, "vm-early"); p.State != "closed" || p.Merged {
		t.Errorf("order PR %+v", p)
	}
}
