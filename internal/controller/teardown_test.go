package controller_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/controller"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery/githubfake"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
)

const xrFile = "clusters/labda/vsphere/machinery-orders-xrs/"

// readyOrder places an order, merges it and makes its XR Ready.
func readyOrder(t *testing.T, e *env, id, name string) {
	t.Helper()
	e.order(t, id, `{"name":"`+name+`","size":"small"}`)
	e.wait(t, id, "PROpen", phase(v1alpha1.PhasePROpen))
	o := &v1alpha1.Order{}
	_ = e.c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: id}, o)
	e.git.Merge(o.Status.Delivery.PullRequest.Number)
	e.wait(t, id, "Merged", phase(v1alpha1.PhaseMerged)) // no Flux check: Merged until machinery sees the XR
	e.machinery.Set("NativeVsphereVM", "default", name+"-vm", machinery.Resource{Ready: true, Address: "10.0.0.9"})
	e.wait(t, id, "Ready", phase(v1alpha1.PhaseReady))
}

func requestTeardown(t *testing.T, e *env, id string, attempt int, am *v1alpha1.OrderAutoMerge) {
	t.Helper()
	o := &v1alpha1.Order{}
	if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: id}, o); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	o.Spec.Teardown = &v1alpha1.TeardownRequest{Attempt: attempt, RequestedBy: "user:default/persona-developer", RequestedAt: &now, Reason: "test done", AutoMerge: am}
	if err := e.c.Update(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}

func teardownCond(reason string) func(*v1alpha1.Order) bool {
	return func(o *v1alpha1.Order) bool {
		c := meta.FindStatusCondition(o.Status.Conditions, controller.ConditionTeardown)
		return c != nil && c.Reason == reason
	}
}

func pullOn(e *env, head string) githubfake.PR {
	var last githubfake.PR
	for _, p := range e.git.Pulls() {
		if p.Head == head {
			last = p
		}
	}
	return last
}

func TestTeardown(t *testing.T) {
	e := start(t)
	e.cluster(t, false)
	const id = "ord-0000000000t1"
	readyOrder(t, e, id, "gone")
	// #20: the merged order's branch is deleted.
	if slices.Contains(e.git.Branches(), "vm-gone") {
		t.Error("branch vm-gone left after the merge")
	}

	// Attempt 1, withdrawn: back to Ready, branch deleted.
	requestTeardown(t, e, id, 1, nil)
	o := e.wait(t, id, "TeardownPROpen", phase(v1alpha1.PhaseTeardownPROpen))
	pr := pullOn(e, "delete-claim/gone-vm")
	if pr.Title != "Delete claim: NativeVsphereVM/gone-vm" || !slices.Contains(pr.Labels, "delete-claim") ||
		!strings.Contains(pr.Body, "**Reason:** test done") || !strings.Contains(pr.Body, "Merging deletes the object") {
		t.Errorf("teardown PR %+v", pr)
	}
	if _, ok := e.git.File("delete-claim/gone-vm", xrFile+"gone.yaml"); ok {
		t.Error("the teardown branch still has the file")
	}
	if o.Status.Teardown.FromPhase != v1alpha1.PhaseReady {
		t.Errorf("from %q", o.Status.Teardown.FromPhase)
	}
	e.git.ClosePR(pr.Number)
	o = e.wait(t, id, "withdrawn", func(o *v1alpha1.Order) bool {
		return o.Status.Phase == v1alpha1.PhaseReady && o.Status.Teardown != nil && o.Status.Teardown.Outcome == v1alpha1.TeardownWithdrawn
	})
	if slices.Contains(e.git.Branches(), "delete-claim/gone-vm") {
		t.Error("withdrawn teardown branch left behind")
	}

	// Attempt 2, merged by a human; Delete-XR fails, then the retry works.
	requestTeardown(t, e, id, 2, nil)
	e.wait(t, id, "TeardownPROpen again", func(o *v1alpha1.Order) bool {
		return o.Status.Phase == v1alpha1.PhaseTeardownPROpen && o.Status.Teardown.Attempt == 2
	})
	pr = pullOn(e, "delete-claim/gone-vm")
	e.git.Merge(pr.Number)
	e.wait(t, id, "TearingDown", phase(v1alpha1.PhaseTearingDown))
	if _, ok := e.git.File("main", xrFile+"gone.yaml"); ok {
		t.Error("file still on main")
	}
	e.git.SetChecks(pr.HeadSHA, githubfake.Check{Name: "Delete-XR", Status: "completed", Conclusion: "failure"})
	o = e.wait(t, id, "DeleteFailed", teardownCond("DeleteFailed"))
	if !strings.Contains(o.Status.Message, "gh workflow run delete-claim-xr.yaml -R stuttgart-things/stuttgart-things -f pr=") {
		t.Errorf("message %q", o.Status.Message)
	}
	e.machinery.Delete("NativeVsphereVM", "default", "gone-vm")
	o = e.wait(t, id, "Deleted", phase(v1alpha1.PhaseDeleted))
	if !o.TornDown() || o.Status.Live.Address != "10.0.0.9" {
		t.Errorf("teardown %+v live %+v", o.Status.Teardown, o.Status.Live)
	}

	// #20: the same name can be ordered again, and the torn-down order does
	// not follow the new XR.
	readyOrder(t, e, "ord-0000000000t2", "gone")
	o = e.wait(t, id, "still Deleted", phase(v1alpha1.PhaseDeleted))
	if o.Status.Live.Found {
		t.Error("the torn-down order follows the new XR")
	}
}

func TestTeardownAutoMergeAndConflict(t *testing.T) {
	e := start(t)
	e.cluster(t, false)
	ctx := context.Background()
	mc := &v1alpha1.ManagementCluster{}
	if err := e.c.Get(ctx, client.ObjectKey{Name: "machinery"}, mc); err != nil {
		t.Fatal(err)
	}
	mc.Spec.AutoMerge = &v1alpha1.AutoMerge{Rules: []v1alpha1.AutoMergeRule{{
		Name: "personas", Groups: []string{"group:default/ai-personas"},
		Actions: []v1alpha1.AutoMergeAction{v1alpha1.ActionTeardown},
	}}}
	if err := e.c.Update(ctx, mc); err != nil {
		t.Fatal(err)
	}

	readyOrder(t, e, "ord-0000000000t3", "auto")
	requestTeardown(t, e, "ord-0000000000t3", 1, &v1alpha1.OrderAutoMerge{Rule: "personas"})
	e.wait(t, "ord-0000000000t3", "TeardownPROpen", phase(v1alpha1.PhaseTeardownPROpen))
	pr := pullOn(e, "delete-claim/auto-vm")
	if !strings.Contains(pr.Body, "**Auto-merge:**") {
		t.Errorf("body %s", pr.Body)
	}
	e.git.SetChecks(pr.HeadSHA, githubfake.Check{Name: "Verify-Claim-Deletion", Status: "completed", Conclusion: "success"})
	o := e.wait(t, "ord-0000000000t3", "TearingDown", phase(v1alpha1.PhaseTearingDown))
	if c := meta.FindStatusCondition(o.Status.Conditions, controller.ConditionTeardownAutoMerge); c == nil || c.Reason != "Merged" {
		t.Errorf("condition %+v", c)
	}

	// A foreign open PR on the teardown branch: the teardown fails, the
	// order stays Ready.
	readyOrder(t, e, "ord-0000000000t4", "busy")
	x := "x"
	e.git.Commit("delete-claim/busy-vm", "someone else", map[string]*string{"notes.txt": &x})
	requestTeardown(t, e, "ord-0000000000t4", 1, nil)
	o = e.wait(t, "ord-0000000000t4", "teardown failed", teardownCond("Failed"))
	if o.Status.Phase != v1alpha1.PhaseReady || o.Status.Teardown.Outcome != v1alpha1.TeardownFailed {
		t.Errorf("phase %s teardown %+v", o.Status.Phase, o.Status.Teardown)
	}
}
