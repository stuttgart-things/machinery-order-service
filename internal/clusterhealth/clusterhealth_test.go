package clusterhealth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/clusterhealth"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery/machinerytest"
)

func cluster(flux bool) *v1alpha1.ManagementCluster {
	mc := &v1alpha1.ManagementCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "machinery", Generation: 3},
		Spec: v1alpha1.ManagementClusterSpec{
			Machinery: &v1alpha1.Machinery{Endpoint: "machinery:50051", Insecure: true},
		},
	}
	if flux {
		mc.Spec.Flux = &v1alpha1.Flux{Name: "machinery-orders-xrs"}
	}
	return mc
}

func cond(t, status, reason, msg string) machinery.Condition {
	return machinery.Condition{Type: t, Status: status, Reason: reason, Message: msg}
}

// healthy is a fake with a Ready Kustomization and one healthy package of
// each kind.
func healthy() *machinerytest.Fake {
	f := &machinerytest.Fake{}
	f.Set("Kustomization", "flux-system", "machinery-orders-xrs", machinery.Resource{
		Ready: true, Conditions: []machinery.Condition{cond("Ready", "True", "ReconciliationSucceeded", "Applied revision: main@sha1:abc")},
	})
	for _, k := range clusterhealth.PackageKinds {
		f.Set(k, "", strings.ToLower(k)+"-a", machinery.Resource{
			Conditions: []machinery.Condition{cond("Installed", "True", "ActivePackageRevision", ""), cond("Healthy", "True", "HealthyPackageRevision", "")},
		})
	}
	return f
}

type want map[string]metav1.ConditionStatus

func check(t *testing.T, conds []metav1.Condition, w want) {
	t.Helper()
	if conds[len(conds)-1].Type != clusterhealth.Ready {
		t.Errorf("Ready must come last, got %s", conds[len(conds)-1].Type)
	}
	for typ, s := range w {
		c := meta.FindStatusCondition(conds, typ)
		if c == nil {
			t.Errorf("%s missing", typ)
			continue
		}
		if c.Status != s {
			t.Errorf("%s = %s (%s: %s), want %s", typ, c.Status, c.Reason, c.Message, s)
		}
		if c.ObservedGeneration != 3 {
			t.Errorf("%s observedGeneration = %d, want 3", typ, c.ObservedGeneration)
		}
		if c.Reason == "" {
			t.Errorf("%s has no reason", typ)
		}
	}
}

func ready(conds []metav1.Condition) *metav1.Condition {
	return meta.FindStatusCondition(conds, clusterhealth.Ready)
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	T, F, U := metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown

	t.Run("healthy", func(t *testing.T) {
		conds := clusterhealth.Check(ctx, cluster(true), healthy(), nil)
		check(t, conds, want{clusterhealth.MachineryReachable: T, clusterhealth.FluxReady: T, clusterhealth.CrossplaneHealthy: T, clusterhealth.Ready: T})
		if m := meta.FindStatusCondition(conds, clusterhealth.CrossplaneHealthy).Message; m != "3 packages healthy" {
			t.Errorf("message %q", m)
		}
	})

	t.Run("no machinery", func(t *testing.T) {
		mc := cluster(true)
		mc.Spec.Machinery = nil
		check(t, clusterhealth.Check(ctx, mc, nil, nil), want{clusterhealth.MachineryReachable: U, clusterhealth.Ready: U})
	})

	t.Run("token secret missing", func(t *testing.T) {
		conds := clusterhealth.Check(ctx, cluster(true), nil, errors.New("machinery token of machinery: secret not found"))
		check(t, conds, want{clusterhealth.MachineryReachable: F, clusterhealth.FluxReady: U, clusterhealth.Ready: F})
	})

	t.Run("machinery down", func(t *testing.T) {
		f := healthy()
		f.SetDown(true)
		conds := clusterhealth.Check(ctx, cluster(true), f, nil)
		check(t, conds, want{clusterhealth.MachineryReachable: F, clusterhealth.FluxReady: U, clusterhealth.CrossplaneHealthy: U, clusterhealth.Ready: F})
		if !strings.Contains(ready(conds).Message, "unavailable") {
			t.Errorf("Ready message %q", ready(conds).Message)
		}
	})

	t.Run("kustomization not ready", func(t *testing.T) {
		f := healthy()
		f.Set("Kustomization", "flux-system", "machinery-orders-xrs", machinery.Resource{
			Conditions: []machinery.Condition{cond("Ready", "False", "BuildFailed", "kustomize build failed: accumulating resources")},
		})
		conds := clusterhealth.Check(ctx, cluster(true), f, nil)
		check(t, conds, want{clusterhealth.MachineryReachable: T, clusterhealth.FluxReady: F, clusterhealth.CrossplaneHealthy: T, clusterhealth.Ready: F})
		if r := ready(conds); r.Reason != "KustomizationNotReady" || !strings.Contains(r.Message, "kustomize build failed") {
			t.Errorf("Ready %s: %s", r.Reason, r.Message)
		}
	})

	t.Run("kustomization reconciling", func(t *testing.T) {
		f := healthy()
		f.Set("Kustomization", "flux-system", "machinery-orders-xrs", machinery.Resource{
			Conditions: []machinery.Condition{cond("Ready", "Unknown", "Progressing", "Reconciliation in progress")},
		})
		check(t, clusterhealth.Check(ctx, cluster(true), f, nil), want{clusterhealth.FluxReady: U, clusterhealth.Ready: T})
	})

	t.Run("kustomization missing", func(t *testing.T) {
		f := healthy()
		f.Delete("Kustomization", "flux-system", "machinery-orders-xrs")
		check(t, clusterhealth.Check(ctx, cluster(true), f, nil), want{clusterhealth.FluxReady: F, clusterhealth.Ready: F})
	})

	t.Run("no flux in spec", func(t *testing.T) {
		check(t, clusterhealth.Check(ctx, cluster(false), healthy(), nil), want{clusterhealth.FluxReady: U, clusterhealth.Ready: T})
	})

	t.Run("unhealthy packages", func(t *testing.T) {
		f := healthy()
		for _, n := range []string{"p1", "p2", "p3", "p4"} {
			f.Set("Provider", "", n, machinery.Resource{Conditions: []machinery.Condition{
				cond("Healthy", "False", "UnhealthyPackageRevision", "cannot resolve package dependencies: node already exists"),
			}})
		}
		// just installed: no Healthy condition yet, not a failure
		f.Set("Function", "", "function-new", machinery.Resource{})
		conds := clusterhealth.Check(ctx, cluster(true), f, nil)
		check(t, conds, want{clusterhealth.CrossplaneHealthy: F, clusterhealth.Ready: F})
		m := meta.FindStatusCondition(conds, clusterhealth.CrossplaneHealthy).Message
		if !strings.HasPrefix(m, "4 of 8 packages unhealthy: Provider/p1: cannot resolve") || !strings.HasSuffix(m, "; and 1 more") {
			t.Errorf("message %q", m)
		}
	})

	t.Run("flux and packages both failing", func(t *testing.T) {
		f := healthy()
		f.Delete("Kustomization", "flux-system", "machinery-orders-xrs")
		f.Set("Configuration", "", "platform", machinery.Resource{Conditions: []machinery.Condition{cond("Healthy", "False", "X", "broken")}})
		r := ready(clusterhealth.Check(ctx, cluster(true), f, nil))
		if r.Status != F || !strings.Contains(r.Message, "FluxReady") || !strings.Contains(r.Message, "CrossplaneHealthy") {
			t.Errorf("Ready %s: %s", r.Status, r.Message)
		}
	})

	t.Run("package kinds not watched", func(t *testing.T) {
		f := healthy()
		f.Unwatch(clusterhealth.PackageKinds...)
		conds := clusterhealth.Check(ctx, cluster(true), f, nil)
		check(t, conds, want{clusterhealth.MachineryReachable: T, clusterhealth.CrossplaneHealthy: U, clusterhealth.Ready: T})
		if m := ready(conds).Message; !strings.Contains(m, "CrossplaneHealthy unknown (KindsNotWatched)") {
			t.Errorf("Ready message %q", m)
		}
	})

	t.Run("some package kinds not watched", func(t *testing.T) {
		f := healthy()
		f.Unwatch("Function")
		conds := clusterhealth.Check(ctx, cluster(true), f, nil)
		check(t, conds, want{clusterhealth.CrossplaneHealthy: T})
		if m := meta.FindStatusCondition(conds, clusterhealth.CrossplaneHealthy).Message; m != "2 packages healthy; not watched: Function" {
			t.Errorf("message %q", m)
		}
	})

	t.Run("no packages", func(t *testing.T) {
		f := &machinerytest.Fake{}
		check(t, clusterhealth.Check(ctx, cluster(false), f, nil), want{clusterhealth.CrossplaneHealthy: U, clusterhealth.Ready: T})
	})
}

func TestBlocking(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) *metav1.Time { m := metav1.NewTime(now.Add(-d)); return &m }
	mc := func(s metav1.ConditionStatus, checked *metav1.Time) *v1alpha1.ManagementCluster {
		c := cluster(true)
		if s != "" {
			c.Status.Conditions = []metav1.Condition{{Type: clusterhealth.Ready, Status: s, Reason: "R", Message: "Kustomization flux-system/x: broken"}}
		}
		c.Status.CheckedAt = checked
		return c
	}
	for _, tc := range []struct {
		name  string
		mc    *v1alpha1.ManagementCluster
		block bool
		eff   metav1.ConditionStatus
	}{
		{"never checked", mc("", nil), false, metav1.ConditionUnknown},
		{"ready", mc(metav1.ConditionTrue, at(time.Minute)), false, metav1.ConditionTrue},
		{"unknown", mc(metav1.ConditionUnknown, at(time.Minute)), false, metav1.ConditionUnknown},
		{"not ready", mc(metav1.ConditionFalse, at(time.Minute)), true, metav1.ConditionFalse},
		{"not ready but stale", mc(metav1.ConditionFalse, at(clusterhealth.MaxAge+time.Second)), false, metav1.ConditionUnknown},
		{"not ready, no checkedAt", mc(metav1.ConditionFalse, nil), false, metav1.ConditionUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block, why := clusterhealth.Blocking(tc.mc, now)
			if block != tc.block {
				t.Errorf("Blocking = %v (%q)", block, why)
			}
			if block && why != "management cluster machinery is not ready: Kustomization flux-system/x: broken" {
				t.Errorf("why %q", why)
			}
			if eff, _ := clusterhealth.Effective(tc.mc, now, clusterhealth.MaxAge); eff != tc.eff {
				t.Errorf("Effective = %s, want %s", eff, tc.eff)
			}
		})
	}
}
