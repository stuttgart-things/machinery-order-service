package controller_test

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/controller"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery/githubfake"
)

func autoMergeOrder(t *testing.T, e *env, id, name, rule string) {
	t.Helper()
	o := &v1alpha1.Order{
		ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: "default"},
		Spec: v1alpha1.OrderSpec{
			Template:    "native-vm-labda-vsphere",
			Parameters:  &runtime.RawExtension{Raw: []byte(`{"name":"` + name + `","size":"small"}`)},
			Placement:   v1alpha1.Placement{ManagementCluster: "machinery"},
			RequestedBy: "user:default/persona-developer",
			AutoMerge:   &v1alpha1.OrderAutoMerge{Rule: rule},
		},
	}
	if err := e.c.Create(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}

func autoMergeCond(reason, contains string) func(*v1alpha1.Order) bool {
	return func(o *v1alpha1.Order) bool {
		c := meta.FindStatusCondition(o.Status.Conditions, controller.ConditionAutoMerge)
		return c != nil && c.Reason == reason && strings.Contains(c.Message, contains)
	}
}

func TestAutoMerge(t *testing.T) {
	e := start(t)
	e.cluster(t, false)
	ctx := context.Background()
	mc := &v1alpha1.ManagementCluster{}
	if err := e.c.Get(ctx, client.ObjectKey{Name: "machinery"}, mc); err != nil {
		t.Fatal(err)
	}
	mc.Spec.AutoMerge = &v1alpha1.AutoMerge{Method: "squash", Rules: []v1alpha1.AutoMergeRule{{
		Name: "personas", Groups: []string{"group:default/ai-personas"},
		Parameters: map[string][]string{"size": {"small"}},
	}}}
	if err := e.c.Update(ctx, mc); err != nil {
		t.Fatal(err)
	}

	// Checks pending, then failed, then passed: merged only at the end.
	autoMergeOrder(t, e, "ord-0000000000a1", "am-checks", "personas")
	e.wait(t, "ord-0000000000a1", "PROpen", phase(v1alpha1.PhasePROpen))
	pr := e.git.Pulls()[0]
	if !strings.Contains(pr.Body, "**Auto-merge:**") || !strings.Contains(pr.Body, "rule `personas`") {
		t.Errorf("PR body:\n%s", pr.Body)
	}
	e.git.SetChecks(pr.HeadSHA, githubfake.Check{Name: "validate", Status: "in_progress"})
	e.wait(t, "ord-0000000000a1", "waiting for checks", autoMergeCond("Waiting", "validate"))
	e.git.SetChecks(pr.HeadSHA, githubfake.Check{Name: "validate", Status: "completed", Conclusion: "failure"})
	e.wait(t, "ord-0000000000a1", "checks failed", autoMergeCond("ChecksFailed", "validate (failure)"))
	if p := e.git.Pulls()[0]; p.Merged {
		t.Fatal("merged with a failed check")
	}
	e.git.SetChecks(pr.HeadSHA,
		githubfake.Check{Name: "validate", Status: "completed", Conclusion: "success"},
		githubfake.Check{Name: "deploy", Status: "completed", Conclusion: "skipped"})
	o := e.wait(t, "ord-0000000000a1", "Merged", phase(v1alpha1.PhaseMerged))
	if c := meta.FindStatusCondition(o.Status.Conditions, controller.ConditionAutoMerge); c == nil || c.Reason != "Merged" || !strings.Contains(c.Message, "2 checks passed") {
		t.Errorf("condition %+v", c)
	}
	if m := e.git.MergeMethods(); len(m) != 1 || m[0] != "squash" {
		t.Errorf("merge methods %v", m)
	}

	// A rule that is gone leaves the PR to a human.
	autoMergeOrder(t, e, "ord-0000000000a2", "am-gone", "removed")
	e.wait(t, "ord-0000000000a2", "rule gone", autoMergeCond("RuleGone", "removed"))

	// No checks at all: merged once the settle time is over.
	autoMergeOrder(t, e, "ord-0000000000a3", "am-none", "personas")
	e.wait(t, "ord-0000000000a3", "Merged without checks", phase(v1alpha1.PhaseMerged))

	for _, p := range e.git.Pulls() {
		if p.Head == "vm-am-gone" && p.Merged {
			t.Error("merged the order whose rule is gone")
		}
	}
}
