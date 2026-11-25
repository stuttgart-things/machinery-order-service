package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/clusterhealth"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
)

// waitCluster polls the ManagementCluster until its Ready condition has
// status s.
func (e *env) waitCluster(t *testing.T, s metav1.ConditionStatus) *v1alpha1.ManagementCluster {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	mc := &v1alpha1.ManagementCluster{}
	for time.Now().Before(deadline) {
		if err := e.c.Get(context.Background(), client.ObjectKey{Name: "machinery"}, mc); err == nil {
			if c := meta.FindStatusCondition(mc.Status.Conditions, clusterhealth.Ready); c != nil && c.Status == s {
				return mc
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("ManagementCluster Ready never became %s: %+v", s, mc.Status.Conditions)
	return nil
}

func TestClusterHealth(t *testing.T) {
	e := startHealth(t)
	ready := machinery.Resource{Ready: true, Conditions: []machinery.Condition{{Type: "Ready", Status: "True", Reason: "ReconciliationSucceeded"}}}
	e.machinery.Set("Kustomization", "flux-system", "machinery-orders-xrs", ready)
	e.machinery.Set("Provider", "", "provider-kubernetes", machinery.Resource{Conditions: []machinery.Condition{{Type: "Healthy", Status: "True", Reason: "HealthyPackageRevision"}}})
	e.cluster(t, true)

	mc := e.waitCluster(t, metav1.ConditionTrue)
	for _, typ := range []string{clusterhealth.MachineryReachable, clusterhealth.FluxReady, clusterhealth.CrossplaneHealthy} {
		if !meta.IsStatusConditionTrue(mc.Status.Conditions, typ) {
			t.Errorf("%s not True: %+v", typ, meta.FindStatusCondition(mc.Status.Conditions, typ))
		}
	}
	if mc.Status.CheckedAt == nil || mc.Status.ObservedGeneration != mc.Generation {
		t.Errorf("checkedAt %v observedGeneration %d (generation %d)", mc.Status.CheckedAt, mc.Status.ObservedGeneration, mc.Generation)
	}
	// The token Secret was read and sent.
	if len(e.machinery.Dialed) == 0 || e.machinery.Dialed[0].Token != "s3cret" {
		t.Errorf("dialed %+v", e.machinery.Dialed)
	}

	// machinery goes away: the cluster turns not ready, and a new order
	// waits in Ordered instead of opening a PR.
	e.machinery.SetDown(true)
	mc = e.waitCluster(t, metav1.ConditionFalse)
	if c := meta.FindStatusCondition(mc.Status.Conditions, clusterhealth.MachineryReachable); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("MachineryReachable %+v", c)
	}
	e.order(t, "ord-000000000031", `{"name":"demo","use_case":"dev"}`)
	e.wait(t, "ord-000000000031", "waiting for the cluster", func(o *v1alpha1.Order) bool {
		return o.Status.Phase == v1alpha1.PhaseOrdered && strings.Contains(o.Status.Message, "waiting: management cluster machinery is not ready")
	})
	if len(e.git.Pulls()) != 0 {
		t.Fatalf("a PR was opened for an unhealthy cluster: %+v", e.git.Pulls())
	}

	// machinery back: the cluster recovers and the order goes on.
	e.machinery.SetDown(false)
	e.waitCluster(t, metav1.ConditionTrue)
	e.wait(t, "ord-000000000031", "PROpen", phase(v1alpha1.PhasePROpen))

	// A failing Flux Kustomization also makes the cluster not ready.
	e.machinery.Set("Kustomization", "flux-system", "machinery-orders-xrs", machinery.Resource{
		Conditions: []machinery.Condition{{Type: "Ready", Status: "False", Reason: "BuildFailed", Message: "kustomize build failed"}},
	})
	mc = e.waitCluster(t, metav1.ConditionFalse)
	if c := meta.FindStatusCondition(mc.Status.Conditions, clusterhealth.Ready); !strings.Contains(c.Message, "kustomize build failed") {
		t.Errorf("Ready message %q", c.Message)
	}
	// An order already past the PR is not held back by it.
	e.git.Merge(e.git.Pulls()[0].Number)
	e.wait(t, "ord-000000000031", "Merged", phase(v1alpha1.PhaseMerged))
}
