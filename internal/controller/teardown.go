package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery"
	"github.com/stuttgart-things/machinery-order-service/internal/intake"
)

// DeleteXRCheck is the check run the fleet repo's delete-claim-xr.yaml
// reports on the head commit of a merged delete-claim pull request.
const DeleteXRCheck = "Delete-XR"

func (r *OrderReconciler) setTeardown(o *v1alpha1.Order, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
		Type: ConditionTeardown, Status: status, Reason: reason, Message: msg, ObservedGeneration: o.Generation,
	})
}

// startTeardown opens the delete-claim pull request of spec.teardown
// (docs/adr/0003-teardown.md). A teardown that cannot be delivered does not
// fail the order: it stays where it is and the condition says why.
func (r *OrderReconciler) startTeardown(ctx context.Context, o *v1alpha1.Order) (ctrl.Result, error) {
	t := o.Spec.Teardown
	ts := &v1alpha1.TeardownStatus{Attempt: t.Attempt, FromPhase: o.Status.Phase}
	failed := func(msg string) (ctrl.Result, error) {
		ts.Outcome = v1alpha1.TeardownFailed
		o.Status.Teardown = ts
		r.setTeardown(o, metav1.ConditionFalse, "Failed", msg)
		o.Status.Message = "teardown failed: " + msg
		return ctrl.Result{}, nil
	}
	d, x := o.Status.Delivery, o.Status.XRRef
	if d == nil || x == nil {
		return failed("the order has no delivery or XR to remove")
	}
	files := o.Status.Files
	if len(files) == 0 {
		if rd := o.EffectiveRendered(); rd != nil {
			files = intake.Paths(rd)
		}
	}
	if len(files) == 0 {
		return failed("the order has no files to remove")
	}
	mc, err := r.cluster(ctx, o.Spec.Placement.ManagementCluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	conv := intake.TeardownConventionFor(o, files)
	body := conv.Body
	if am := t.AutoMerge; am != nil {
		body += fmt.Sprintf("\n**Auto-merge:** the service merges this pull request once its checks pass "+
			"(rule `%s` of ManagementCluster %s, asked for by %s). Close it to stop the teardown.\n",
			am.Rule, mc.Name, t.RequestedBy)
	}
	req := delivery.Request{
		Repository: d.Repository, BaseBranch: d.BaseBranch, Branch: conv.Branch,
		Title: conv.Title, Body: body, OrderID: o.Name, Labels: []string{intake.TeardownLabel},
	}
	for _, f := range files {
		req.Files = append(req.Files, delivery.File{Path: f, Delete: true})
	}
	res, err := r.Git.Open(ctx, req)
	if err != nil {
		var ce *delivery.ConflictError
		if errors.As(err, &ce) {
			return failed(ce.Msg)
		}
		return ctrl.Result{}, fmt.Errorf("teardown delivery: %w", err)
	}
	ts.Delivery = &v1alpha1.DeliveryStatus{
		Repository: d.Repository, BaseBranch: d.BaseBranch, Branch: conv.Branch, CommitSHA: res.CommitSHA,
		PullRequest:    &v1alpha1.PullRequest{Number: res.PRNumber, URL: res.PRURL, State: "open"},
		Kustomizations: res.Kustomizations,
	}
	o.Status.Teardown = ts
	msg := "teardown pull request " + res.PRURL
	if t.RequestedBy == ServiceRequester {
		msg += " (" + t.Reason + ")"
	}
	r.setTeardown(o, metav1.ConditionUnknown, "PROpen", msg)
	SetPhase(o, v1alpha1.PhaseTeardownPROpen, msg)
	return ctrl.Result{RequeueAfter: r.PRPoll}, nil
}

// followTeardownPR follows the open teardown pull request: merged moves on
// to TearingDown, closed withdraws the teardown.
func (r *OrderReconciler) followTeardownPR(ctx context.Context, o *v1alpha1.Order) (ctrl.Result, error) {
	ts := o.Status.Teardown
	if ts == nil || ts.Delivery == nil || ts.Delivery.PullRequest == nil {
		// Lost track (status edited); start the teardown over.
		from := v1alpha1.PhaseReady
		if ts != nil && ts.FromPhase != "" {
			from = ts.FromPhase
		}
		o.Status.Teardown = nil
		SetPhase(o, from, "teardown pull request unknown, opening it again")
		return ctrl.Result{}, nil
	}
	d := ts.Delivery
	pr, err := r.Git.PullRequest(ctx, d.Repository, d.PullRequest.Number)
	if err != nil {
		return ctrl.Result{}, err
	}
	d.PullRequest.State = pr.State
	switch pr.State {
	case "merged":
		d.MergeSHA = pr.MergeSHA
		r.deleteBranch(ctx, d)
		msg := fmt.Sprintf("teardown merged as %s; delete-claim-xr.yaml deletes %s %s", short(pr.MergeSHA), o.Status.XRRef.Kind, o.Status.XRRef.Name)
		r.setTeardown(o, metav1.ConditionUnknown, "Deleting", msg)
		SetPhase(o, v1alpha1.PhaseTearingDown, msg)
		return ctrl.Result{}, nil
	case "closed":
		r.deleteBranch(ctx, d)
		ts.Outcome = v1alpha1.TeardownWithdrawn
		from := ts.FromPhase
		if from == "" {
			from = v1alpha1.PhaseReady
		}
		r.setTeardown(o, metav1.ConditionFalse, "Withdrawn", "teardown pull request closed without a merge")
		SetPhase(o, from, "teardown withdrawn: pull request closed without a merge")
		return ctrl.Result{}, nil
	}
	if t := o.Spec.Teardown; t != nil && t.AutoMerge != nil {
		return r.autoMerge(ctx, o, pr, mergeJob{
			d: d, rule: t.AutoMerge.Rule, action: v1alpha1.ActionTeardown,
			condition: ConditionTeardownAutoMerge, opened: v1alpha1.PhaseTeardownPROpen, what: "teardown pull request",
		})
	}
	o.Status.Message = "teardown pull request " + d.PullRequest.URL + "; a human merges"
	return ctrl.Result{RequeueAfter: r.PRPoll}, nil
}

// followTeardown waits until machinery no longer sees the XR. The fleet
// repo deletes it; the service only watches, and says so when the
// Delete-XR check failed.
func (r *OrderReconciler) followTeardown(ctx context.Context, o *v1alpha1.Order) (ctrl.Result, error) {
	mc, err := r.cluster(ctx, o.Spec.Placement.ManagementCluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	mcl, err := r.machineryFor(ctx, mc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if mcl == nil {
		o.Status.Message = fmt.Sprintf("teardown merged; ManagementCluster %s names no machinery, so whether the XR is gone is unknown", mc.Name)
		return ctrl.Result{RequeueAfter: r.ReadyPoll}, nil
	}
	x := o.Status.XRRef
	res, err := mcl.Get(ctx, x.Kind, x.Name, x.Namespace)
	if err != nil {
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
			Type: ConditionLive, Status: metav1.ConditionUnknown, Reason: "MachineryError", Message: err.Error(),
			ObservedGeneration: o.Generation,
		})
		return ctrl.Result{}, err
	}
	now := metav1.Now()
	if !res.Found {
		live := &v1alpha1.LiveStatus{}
		if o.Status.Live != nil {
			live = o.Status.Live.DeepCopy()
		}
		live.Found, live.Ready, live.StatusMessage, live.ObservedAt = false, false, "", &now
		o.Status.Live = live
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
			Type: ConditionLive, Status: metav1.ConditionFalse, Reason: "Gone",
			Message: fmt.Sprintf("machinery no longer sees %s %s", x.Kind, x.Name), ObservedGeneration: o.Generation,
		})
		o.Status.Teardown.Outcome = v1alpha1.TeardownDeleted
		msg := fmt.Sprintf("torn down: machinery no longer sees %s %s", x.Kind, x.Name)
		r.setTeardown(o, metav1.ConditionTrue, "Deleted", msg)
		SetPhase(o, v1alpha1.PhaseDeleted, msg)
		return ctrl.Result{}, nil
	}
	if o.Status.Live != nil {
		o.Status.Live.Found, o.Status.Live.Ready, o.Status.Live.StatusMessage = true, res.Ready, res.StatusMessage
		o.Status.Live.ObservedAt, o.Status.Live.LastSeenAt = &now, &now
	}

	d := o.Status.Teardown.Delivery
	n := d.PullRequest.Number
	msg := fmt.Sprintf("teardown merged; waiting for delete-claim-xr.yaml (#%d) to delete %s %s", n, x.Kind, x.Name)
	if checks, err := r.Git.Checks(ctx, d.Repository, d.CommitSHA); err == nil {
		if failed := checkNamed(checks.Failed, DeleteXRCheck); failed != "" {
			msg = fmt.Sprintf("delete-claim-xr.yaml failed for #%d (%s); %s %s is still there. "+
				"Someone with repo access retries it: gh workflow run delete-claim-xr.yaml -R %s -f pr=%d",
				n, failed, x.Kind, x.Name, d.Repository, n)
			r.setTeardown(o, metav1.ConditionFalse, "DeleteFailed", msg)
			o.Status.Message = msg
			return ctrl.Result{RequeueAfter: r.PRPoll}, nil
		}
		if checkNamed(checks.Pending, DeleteXRCheck) != "" {
			msg += " (Delete-XR running)"
		}
	}
	r.setTeardown(o, metav1.ConditionUnknown, "Deleting", msg)
	o.Status.Message = msg
	return ctrl.Result{RequeueAfter: r.LivePoll}, nil
}

// checkNamed returns the entry of list that is check name ("name" or
// "name (conclusion)"), or "".
func checkNamed(list []string, name string) string {
	for _, c := range list {
		if c == name || strings.HasPrefix(c, name+" (") {
			return c
		}
	}
	return ""
}
