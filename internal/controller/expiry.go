package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/automerge"
)

// ServiceRequester is the requester of what the service asks for itself: the
// teardown of an expired order (#27).
const ServiceRequester = "service:machinery-order-service"

// expiredNote says when and under which rule the order ran out.
func expiredNote(o *v1alpha1.Order) string {
	return fmt.Sprintf("expired: the lifetime under auto-merge rule %s ran out at %s",
		o.Spec.AutoMerge.Rule, o.ExpiresAt().UTC().Format(time.RFC3339))
}

// cancelNote says why an order before its merge is to be cancelled: asked
// for, or ran out. "" when neither.
func cancelNote(o *v1alpha1.Order) string {
	switch {
	case o.Spec.Cancel:
		return "cancelled"
	case o.ExpiredAt(time.Now()):
		return expiredNote(o)
	}
	return ""
}

// expire asks for the teardown of a merged order whose lifetime ran out
// (spec.autoMerge.expiresAt), as the service, with auto-merge under the
// order's rule while it still allows teardowns. It does so once: a teardown
// that was asked for already, by anyone, is left alone. It reports whether
// it changed the order.
func (r *OrderReconciler) expire(ctx context.Context, o *v1alpha1.Order) (bool, error) {
	if !o.ExpiredAt(time.Now()) || o.Spec.Teardown != nil || !o.Status.Phase.Teardownable() {
		return false, nil
	}
	now := metav1.Now()
	t := &v1alpha1.TeardownRequest{Attempt: 1, RequestedBy: ServiceRequester, RequestedAt: &now, Reason: expiredNote(o)}
	mc, err := r.cluster(ctx, o.Spec.Placement.ManagementCluster)
	if err != nil {
		return false, err
	}
	// Without the rule (removed, or no longer for teardowns) a human merges;
	// autoMerge checks the rule again while the pull request is open.
	if rule := automerge.Rule(mc.Spec.AutoMerge, o.Spec.AutoMerge.Rule); rule != nil && automerge.AllowsAction(*rule, v1alpha1.ActionTeardown) {
		t.AutoMerge = &v1alpha1.OrderAutoMerge{Rule: rule.Name}
	}
	o.Spec.Teardown = t
	if err := r.Update(ctx, o); err != nil {
		return false, err
	}
	return true, nil
}

// untilExpiry shortens res so the order is looked at again when it runs out.
func untilExpiry(o *v1alpha1.Order, res ctrl.Result) ctrl.Result {
	e := o.ExpiresAt()
	if e == nil || o.Status.Phase.Terminal() || o.Spec.Teardown != nil {
		return res
	}
	d := time.Until(e.Time)
	if d <= 0 {
		d = time.Second
	}
	if res.RequeueAfter == 0 || d < res.RequeueAfter {
		res.RequeueAfter = d
	}
	return res
}

// withExpiry adds the expiry to the message of an order that has one and
// has not run out yet.
func withExpiry(o *v1alpha1.Order) {
	e := o.ExpiresAt()
	if e == nil || o.Status.Phase.Terminal() || o.Spec.Teardown != nil || o.ExpiredAt(time.Now()) {
		return
	}
	// Steps that keep the phase leave the message as it was, suffix included.
	if suffix := fmt.Sprintf(" (expires %s)", e.UTC().Format(time.RFC3339)); !strings.HasSuffix(o.Status.Message, suffix) {
		o.Status.Message += suffix
	}
}
