// Package controller reconciles Orders: placement (pinned), render, delivery
// as a pull request, and the status from git, Flux and machinery. It also
// checks every ManagementCluster through machinery (cluster_controller.go,
// #31); an order waits in Ordered/Placed while its cluster is not ready:
//
//	Ordered → Placed → PROpen → Merged → Applied → Building → Ready
//	                     └→ Cancelled (cancel, or the PR closed unmerged)
//	Failed: the order cannot go on as it is (bad parameters, a taken name)
//
// An order with spec.autoMerge.expiresAt (the rule's maxLifetime, #27) is
// cancelled when it runs out before its merge, and torn down after it.
//
// A teardown (spec.teardown, docs/adr/0003-teardown.md) opens a
// delete-claim pull request; the fleet repo deletes the XR after its merge:
//
//	Merged…Ready|Deleted → TeardownPROpen → TearingDown → Deleted
//
// The controller never applies anything to a management cluster. Its only
// write path is git; Flux brings the files onto the cluster. Deleting an
// Order never tears anything down: before the merge its pull request is
// closed, after the merge only the record goes.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/automerge"
	"github.com/stuttgart-things/machinery-order-service/internal/capabilities"
	"github.com/stuttgart-things/machinery-order-service/internal/clusterhealth"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery"
	"github.com/stuttgart-things/machinery-order-service/internal/intake"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
	"github.com/stuttgart-things/machinery-order-service/internal/recovery"
	"github.com/stuttgart-things/machinery-order-service/internal/render"
)

// Condition types on an Order.
const (
	// ConditionDelivered: the files are on a branch with an open or merged PR.
	ConditionDelivered = "Delivered"
	// ConditionLive: machinery answered for the XR.
	ConditionLive = "Live"
	// ConditionAutoMerge: where the service's own merge of the PR stands
	// (orders with spec.autoMerge only).
	ConditionAutoMerge = "AutoMerge"
	// ConditionTeardown: where the teardown stands (spec.teardown).
	ConditionTeardown = "Teardown"
	// ConditionTeardownAutoMerge: the service's own merge of the teardown PR.
	ConditionTeardownAutoMerge = "TeardownAutoMerge"
)

// AutoMergeSettle is how long an auto-merge order waits for checks to show
// up on a PR before it treats "no checks" as passed.
const AutoMergeSettle = time.Minute

// OrderReconciler reconciles Orders.
type OrderReconciler struct {
	client.Client
	// APIReader reads Secrets uncached.
	APIReader client.Reader
	Renderer  intake.Renderer
	Git       delivery.Deliverer
	Machinery machinery.Dialer
	// Namespace is the service's namespace, the default for secret refs.
	Namespace string
	// PRPoll is how often an open PR or a pending Flux apply is checked;
	// LivePoll how often machinery is asked while the XR builds; ReadyPoll
	// how often a Ready order is refreshed.
	PRPoll, LivePoll, ReadyPoll time.Duration
	// AutoMergeSettle overrides AutoMergeSettle (tests).
	AutoMergeSettle time.Duration
}

// +kubebuilder:rbac:groups=order.machinery.stuttgart-things.com,resources=orders,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=order.machinery.stuttgart-things.com,resources=orders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=order.machinery.stuttgart-things.com,resources=orders/finalizers,verbs=update
// +kubebuilder:rbac:groups=order.machinery.stuttgart-things.com,resources=managementclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// SetupWithManager registers the reconciler.
func (r *OrderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.PRPoll == 0 {
		r.PRPoll = time.Minute
	}
	if r.LivePoll == 0 {
		r.LivePoll = 30 * time.Second
	}
	if r.ReadyPoll == 0 {
		r.ReadyPoll = 5 * time.Minute
	}
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.Order{}).Named("order").Complete(r)
}

// SetPhase moves the order to phase p and records it in the timeline.
func SetPhase(o *v1alpha1.Order, p v1alpha1.Phase, msg string) {
	o.Status.Message = msg
	if o.Status.Phase == p {
		return
	}
	o.Status.Phase = p
	o.Status.History = append(o.Status.History, v1alpha1.PhaseTransition{Phase: p, Time: metav1.Now(), Message: msg})
}

// Reconcile moves one order one or more steps along.
func (r *OrderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	o := &v1alpha1.Order{}
	if err := r.Get(ctx, req.NamespacedName, o); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !o.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, o)
	}
	if controllerutil.AddFinalizer(o, v1alpha1.Finalizer) {
		if err := r.Update(ctx, o); err != nil {
			return ctrl.Result{}, err
		}
	}

	if changed, err := r.expire(ctx, o); err != nil || changed {
		// The spec update brings the order back with the teardown.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	before := o.Status.DeepCopy()
	res, err := r.step(ctx, o)
	o.Status.ObservedGeneration = o.Generation
	if err != nil {
		o.Status.Message = err.Error()
	} else {
		withExpiry(o)
		res = untilExpiry(o, res)
	}
	if !equality.Semantic.DeepEqual(before, &o.Status) {
		if uerr := r.Status().Update(ctx, o); uerr != nil {
			if apierrors.IsConflict(uerr) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, uerr
		}
	}
	return res, err
}

// finalize lets the Order go. Before the merge its pull request is closed,
// so nothing is created; after the merge nothing is touched.
func (r *OrderReconciler) finalize(ctx context.Context, o *v1alpha1.Order) error {
	if !controllerutil.ContainsFinalizer(o, v1alpha1.Finalizer) {
		return nil
	}
	var d *v1alpha1.DeliveryStatus
	switch o.Status.Phase {
	case v1alpha1.PhasePROpen:
		d = o.Status.Delivery
	case v1alpha1.PhaseTeardownPROpen:
		// An open teardown nobody follows any more is closed, not merged.
		if o.Status.Teardown != nil {
			d = o.Status.Teardown.Delivery
		}
	}
	if d != nil && d.PullRequest != nil {
		if err := r.Git.Close(ctx, d.Repository, d.PullRequest.Number, d.Branch); err != nil {
			return fmt.Errorf("closing the pull request of the deleted order: %w", err)
		}
		log.FromContext(ctx).Info("closed the pull request of a deleted order", "pr", d.PullRequest.URL)
	}
	controllerutil.RemoveFinalizer(o, v1alpha1.Finalizer)
	return r.Update(ctx, o)
}

func (r *OrderReconciler) step(ctx context.Context, o *v1alpha1.Order) (ctrl.Result, error) {
	if o.Status.OrderID == "" {
		o.Status.OrderID = o.Name
	}
	if o.Status.Phase == "" {
		if o.Spec.Recovered != nil {
			return r.recovered(ctx, o)
		}
		SetPhase(o, v1alpha1.PhaseOrdered, "accepted")
	}
	if o.Status.Phase.Terminal() {
		return ctrl.Result{}, nil
	}
	if o.Spec.Cancel && !o.Status.Phase.BeforeMerge() {
		o.Status.Message = "cancel refused: the order is merged; tear it down instead"
	}

	for {
		phase := o.Status.Phase
		res, err := r.advance(ctx, o)
		if err != nil || o.Status.Phase == phase || o.Status.Phase.Terminal() {
			return res, err
		}
	}
}

// advance runs the step of the current phase.
func (r *OrderReconciler) advance(ctx context.Context, o *v1alpha1.Order) (ctrl.Result, error) {
	switch o.Status.Phase {
	case v1alpha1.PhaseOrdered:
		if why := cancelNote(o); why != "" {
			SetPhase(o, v1alpha1.PhaseCancelled, why+" before the delivery")
			return ctrl.Result{}, nil
		}
		mc, err := r.cluster(ctx, o.Spec.Placement.ManagementCluster)
		if apierrors.IsNotFound(err) {
			o.Status.Message = fmt.Sprintf("waiting for ManagementCluster %q", o.Spec.Placement.ManagementCluster)
			return ctrl.Result{RequeueAfter: r.PRPoll}, nil
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		if mc.Spec.Cordoned {
			SetPhase(o, v1alpha1.PhaseFailed, fmt.Sprintf("ManagementCluster %s is cordoned", mc.Name))
			return ctrl.Result{}, nil
		}
		if blocked, why := clusterhealth.Blocking(mc, time.Now()); blocked {
			// A health problem passes; wait for it instead of failing.
			o.Status.Message = "waiting: " + why
			return ctrl.Result{RequeueAfter: r.PRPoll}, nil
		}
		p := o.Spec.Placement
		o.Status.Placement = &p
		SetPhase(o, v1alpha1.PhasePlaced, "pinned to "+p.ManagementCluster)
		return ctrl.Result{}, nil

	case v1alpha1.PhasePlaced:
		if why := cancelNote(o); why != "" {
			SetPhase(o, v1alpha1.PhaseCancelled, why+" before the delivery")
			return ctrl.Result{}, nil
		}
		// Placed is left in the same step that opens the PR, so this only
		// holds back orders whose cluster turned unhealthy before delivery.
		mc, err := r.cluster(ctx, o.Spec.Placement.ManagementCluster)
		if err != nil {
			return ctrl.Result{}, err
		}
		if blocked, why := clusterhealth.Blocking(mc, time.Now()); blocked {
			o.Status.Message = "waiting: " + why
			return ctrl.Result{RequeueAfter: r.PRPoll}, nil
		}
		return r.deliver(ctx, o)

	case v1alpha1.PhasePROpen:
		return r.followPR(ctx, o)

	case v1alpha1.PhaseMerged:
		if o.TeardownPending() {
			return r.startTeardown(ctx, o)
		}
		return r.followApply(ctx, o)

	case v1alpha1.PhaseApplied, v1alpha1.PhaseRecovered, v1alpha1.PhaseBuilding, v1alpha1.PhaseReady, v1alpha1.PhaseDeleted:
		if o.TornDown() {
			// Final: a new order of the same name must not be taken for
			// this one.
			return ctrl.Result{}, nil
		}
		if o.TeardownPending() {
			return r.startTeardown(ctx, o)
		}
		return r.followLive(ctx, o)

	case v1alpha1.PhaseTeardownPROpen:
		return r.followTeardownPR(ctx, o)

	case v1alpha1.PhaseTearingDown:
		return r.followTeardown(ctx, o)
	}
	return ctrl.Result{}, nil
}

// recovered starts an Order rebuilt from git (spec.recovered, #4823): the
// status a merged order would have, then phase Recovered, from where the
// live XR decides (Building, Ready; a missing XR is "not yet").
func (r *OrderReconciler) recovered(ctx context.Context, o *v1alpha1.Order) (ctrl.Result, error) {
	rc := o.Spec.Recovered
	rd := o.EffectiveRendered()
	if rd == nil {
		SetPhase(o, v1alpha1.PhaseFailed, "recovered without files (spec.rendered)")
		return ctrl.Result{}, nil
	}
	xr, err := intake.XRRef(rd)
	if err != nil {
		SetPhase(o, v1alpha1.PhaseFailed, "recovered: "+err.Error())
		return ctrl.Result{}, nil
	}
	mc, err := r.cluster(ctx, o.Spec.Placement.ManagementCluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	repo, base := recovery.Repository(mc)
	p := o.Spec.Placement
	o.Status.Placement = &p
	o.Status.XRRef = xr
	o.Status.Files = intake.Paths(rd)
	by := ""
	if rc.By != "" {
		by = " by " + rc.By
	}
	if pr := rc.PullRequest; pr != nil && pr.State == "open" {
		// Not merged yet: follow the open pull request (PROpen).
		cp := *pr
		o.Status.Delivery = &v1alpha1.DeliveryStatus{Repository: repo, BaseBranch: base, Branch: rc.Branch,
			CommitSHA: rc.CommitSHA, PullRequest: &cp}
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
			Type: ConditionDelivered, Status: metav1.ConditionTrue, Reason: "PullRequestOpened", Message: pr.URL,
			ObservedGeneration: o.Generation,
		})
		SetPhase(o, v1alpha1.PhasePROpen, "recovered with its open pull request "+pr.URL+by)
		return r.step(ctx, o)
	}
	d := &v1alpha1.DeliveryStatus{Repository: repo, BaseBranch: base, MergeSHA: rc.CommitSHA}
	if rc.PullRequest != nil {
		pr := *rc.PullRequest
		d.PullRequest = &pr
	}
	o.Status.Delivery = d
	msg := "recovered from " + rc.Path
	switch {
	case rc.PullRequest != nil:
		msg += " (" + rc.PullRequest.URL + ")"
	case rc.CommitSHA != "":
		msg += " (commit " + short(rc.CommitSHA) + ")"
	}
	SetPhase(o, v1alpha1.PhaseRecovered, msg+by)
	if td, t := rc.Teardown, o.Spec.Teardown; td != nil && t != nil {
		// Its teardown's pull request is open: follow that (TeardownPROpen);
		// closed unmerged goes back to Recovered.
		pr := td.PullRequest
		o.Status.Teardown = &v1alpha1.TeardownStatus{Attempt: t.Attempt, FromPhase: v1alpha1.PhaseRecovered,
			Delivery: &v1alpha1.DeliveryStatus{Repository: repo, BaseBranch: base, Branch: td.Branch, CommitSHA: td.CommitSHA, PullRequest: &pr}}
		msg := "recovered with its open teardown pull request " + pr.URL
		r.setTeardown(o, metav1.ConditionUnknown, "PROpen", msg)
		SetPhase(o, v1alpha1.PhaseTeardownPROpen, msg)
	}
	return r.step(ctx, o)
}

func (r *OrderReconciler) cluster(ctx context.Context, name string) (*v1alpha1.ManagementCluster, error) {
	mc := &v1alpha1.ManagementCluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, mc); err != nil {
		return nil, err
	}
	return mc, nil
}

// fail ends the order on errors retrying does not fix; any other error is
// returned for a retry with backoff.
func fail(o *v1alpha1.Order, what string, err error) (ctrl.Result, error) {
	var ie *intake.InvalidError
	var ce *delivery.ConflictError
	if errors.As(err, &ie) || errors.As(err, &ce) || render.IsCallerError(err) {
		SetPhase(o, v1alpha1.PhaseFailed, what+": "+err.Error())
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, fmt.Errorf("%s: %w", what, err)
}

func (r *OrderReconciler) deliver(ctx context.Context, o *v1alpha1.Order) (ctrl.Result, error) {
	mc, err := r.cluster(ctx, o.Spec.Placement.ManagementCluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	rd := o.EffectiveRendered()
	if rd == nil {
		rd, err = intake.Render(ctx, r.Renderer, o, mc, "")
		if err != nil {
			return fail(o, "render", err)
		}
		o.Status.Rendered = rd
	}
	xr, err := intake.XRRef(rd)
	if err != nil {
		return fail(o, "render", &intake.InvalidError{Msg: err.Error()})
	}
	x := capabilities.XR{APIVersion: xr.APIVersion, Kind: xr.Kind, Environment: intake.XREnvironment(rd)}
	// allowedKinds also for Orders created with kubectl, which skip the API.
	if why := capabilities.Allowed(mc, x); why != "" {
		return fail(o, "placement", &intake.InvalidError{Msg: why})
	}
	if why := capabilities.Check(mc, x, time.Now()); why != "" {
		return fail(o, "placement", &intake.InvalidError{Msg: why})
	}
	o.Status.XRRef = xr
	o.Status.Files = intake.Paths(rd)

	var conv intake.Convention
	if o.Spec.Adopted != nil {
		// adopt: the template may be a placeholder (adopted:<Kind>)
		conv = intake.AdoptConventionFor(o, xr)
	} else {
		tmpl, err := intake.For(r.Renderer, mc).Template(ctx, o.Spec.Template, "")
		if err != nil {
			return fail(o, "reading the template", err)
		}
		if conv, err = intake.ConventionFor(o, rd, tmpl); err != nil {
			return fail(o, "pull request", err)
		}
	}
	repo := mc.Spec.GitOps.Repository
	if repo == "" {
		repo = "stuttgart-things/stuttgart-things"
	}
	base := mc.Spec.GitOps.BaseBranch
	if base == "" {
		base = "main"
	}
	body := conv.Body
	if am := o.Spec.AutoMerge; am != nil {
		body += fmt.Sprintf("\n\n**Auto-merge:** the service merges this pull request once its checks pass "+
			"(rule `%s` of ManagementCluster %s, asked for by %s). Close it to stop the order.\n",
			am.Rule, mc.Name, o.Spec.RequestedBy)
	}
	req := delivery.Request{
		Repository: repo, BaseBranch: base, Branch: conv.Branch,
		Title: conv.Title, Body: body, OrderID: o.Name,
	}
	for _, f := range rd.Files {
		df := delivery.File{Path: f.Path, Content: f.Content}
		if a := o.Spec.Adopted; a != nil && f.Path == a.Path {
			df.Replace, df.BaseSHA256 = true, a.ContentSHA256
		}
		req.Files = append(req.Files, df)
	}
	res, err := r.Git.Open(ctx, req)
	if err != nil {
		return fail(o, "delivery", err)
	}
	o.Status.Delivery = &v1alpha1.DeliveryStatus{
		Repository: repo, BaseBranch: base, Branch: conv.Branch, CommitSHA: res.CommitSHA,
		PullRequest:    &v1alpha1.PullRequest{Number: res.PRNumber, URL: res.PRURL, State: "open"},
		Kustomizations: res.Kustomizations,
	}
	meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
		Type: ConditionDelivered, Status: metav1.ConditionTrue, Reason: "PullRequestOpened", Message: res.PRURL,
		ObservedGeneration: o.Generation,
	})
	SetPhase(o, v1alpha1.PhasePROpen, "pull request "+res.PRURL)
	return ctrl.Result{RequeueAfter: r.PRPoll}, nil
}

func (r *OrderReconciler) followPR(ctx context.Context, o *v1alpha1.Order) (ctrl.Result, error) {
	d := o.Status.Delivery
	if d == nil || d.PullRequest == nil {
		SetPhase(o, v1alpha1.PhasePlaced, "pull request unknown, delivering again")
		return ctrl.Result{}, nil
	}
	if why := cancelNote(o); why != "" {
		if err := r.Git.Close(ctx, d.Repository, d.PullRequest.Number, d.Branch); err != nil {
			return ctrl.Result{}, fmt.Errorf("closing the pull request: %w", err)
		}
		d.PullRequest.State = "closed"
		SetPhase(o, v1alpha1.PhaseCancelled, why+": pull request closed")
		return ctrl.Result{}, nil
	}
	pr, err := r.Git.PullRequest(ctx, d.Repository, d.PullRequest.Number)
	if err != nil {
		return ctrl.Result{}, err
	}
	d.PullRequest.State = pr.State
	switch pr.State {
	case "merged":
		d.MergeSHA = pr.MergeSHA
		r.deleteBranch(ctx, d)
		SetPhase(o, v1alpha1.PhaseMerged, "merged as "+short(pr.MergeSHA))
		return ctrl.Result{}, nil
	case "closed":
		r.deleteBranch(ctx, d)
		SetPhase(o, v1alpha1.PhaseCancelled, "pull request closed without a merge")
		return ctrl.Result{}, nil
	}
	if o.Spec.AutoMerge != nil {
		return r.autoMerge(ctx, o, pr, mergeJob{
			d: d, rule: o.Spec.AutoMerge.Rule, action: v1alpha1.ActionOrder,
			condition: ConditionAutoMerge, opened: v1alpha1.PhasePROpen, what: "pull request",
		})
	}
	return ctrl.Result{RequeueAfter: r.PRPoll}, nil
}

// deleteBranch removes the branch of a merged or closed pull request, so the
// name can be ordered again (#20). Best effort: a branch left behind is
// healed by the next Open of the same name.
func (r *OrderReconciler) deleteBranch(ctx context.Context, d *v1alpha1.DeliveryStatus) {
	if err := r.Git.DeleteBranch(ctx, d.Repository, d.Branch); err != nil {
		log.FromContext(ctx).Info("branch not deleted", "branch", d.Branch, "error", err.Error())
	}
}

// mergeJob is one pull request the service may merge itself: the order's or
// its teardown's.
type mergeJob struct {
	d         *v1alpha1.DeliveryStatus
	rule      string
	action    v1alpha1.AutoMergeAction
	condition string
	// opened is the phase the PR opened in (the checks' settle time).
	opened v1alpha1.Phase
	// what names the PR in status.message.
	what string
}

// autoMerge merges an open PR of an order that asked for it, once its
// checks pass and the rule still covers it (docs/adr/0002-auto-merge.md).
// Anything else leaves the PR to a human; the condition says why.
func (r *OrderReconciler) autoMerge(ctx context.Context, o *v1alpha1.Order, pr *delivery.PRState, job mergeJob) (ctrl.Result, error) {
	d := job.d
	wait := ctrl.Result{RequeueAfter: r.PRPoll}
	// set records the state in the condition and, for API clients that only
	// read the message (claims orders get/watch), in status.message.
	set := func(status metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
			Type: job.condition, Status: status, Reason: reason, Message: msg, ObservedGeneration: o.Generation,
		})
	}
	note := func(msg string) { o.Status.Message = job.what + " " + d.PullRequest.URL + "; auto-merge: " + msg }
	mc, err := r.cluster(ctx, o.Spec.Placement.ManagementCluster)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	rule := automerge.Rule(mc.Spec.AutoMerge, job.rule)
	if rule == nil {
		set(metav1.ConditionFalse, "RuleGone", fmt.Sprintf("rule %s is no longer on ManagementCluster %s; a human merges", job.rule, mc.Name))
		note(fmt.Sprintf("stopped, rule %s is gone; a human merges", job.rule))
		return wait, nil
	}
	if !automerge.AllowsAction(*rule, job.action) {
		set(metav1.ConditionFalse, "RuleGone", fmt.Sprintf("rule %s no longer allows %ss; a human merges", rule.Name, job.action))
		note(fmt.Sprintf("stopped, rule %s no longer allows %ss; a human merges", rule.Name, job.action))
		return wait, nil
	}
	if ok, why := automerge.Covers(*rule, o.Spec.Template, automerge.Params(o.EffectiveRendered())); !ok {
		set(metav1.ConditionFalse, "RuleGone", fmt.Sprintf("rule %s no longer covers the order (%s); a human merges", rule.Name, why))
		note(fmt.Sprintf("stopped, rule %s no longer covers the order (%s); a human merges", rule.Name, why))
		return wait, nil
	}
	checks, err := r.Git.Checks(ctx, d.Repository, pr.HeadSHA)
	if err != nil {
		set(metav1.ConditionUnknown, "Waiting", err.Error())
		note("cannot read checks: " + checksError(err))
		return wait, nil
	}
	switch {
	case len(checks.Failed) > 0:
		set(metav1.ConditionFalse, "ChecksFailed", "not merging, failed: "+strings.Join(checks.Failed, ", ")+"; a human decides")
		note("stopped, failed: " + strings.Join(checks.Failed, ", ") + "; a human decides")
		return wait, nil
	case len(checks.Pending) > 0:
		set(metav1.ConditionUnknown, "Waiting", "waiting for checks: "+strings.Join(checks.Pending, ", "))
		note("waiting for checks: " + strings.Join(checks.Pending, ", "))
		return wait, nil
	case checks.Total == 0 && time.Since(r.prOpenedAt(o, job.opened)) < r.settle():
		set(metav1.ConditionUnknown, "Waiting", "waiting for checks to show up")
		note("waiting for checks to show up")
		return wait, nil
	}
	method := automerge.Method(mc.Spec.AutoMerge)
	if err := r.Git.Merge(ctx, d.Repository, d.PullRequest.Number, pr.HeadSHA, method); err != nil {
		var nm *delivery.NotMergeableError
		if errors.As(err, &nm) {
			set(metav1.ConditionFalse, "NotMergeable", nm.Msg+"; a human decides")
			note("stopped, GitHub refuses the merge; a human decides")
			return wait, nil
		}
		return ctrl.Result{}, err
	}
	passed := fmt.Sprintf("%d checks passed", checks.Total)
	if checks.Total == 0 {
		passed = "no checks"
	}
	msg := fmt.Sprintf("auto-merged (%s, rule %s, %s)", method, rule.Name, passed)
	set(metav1.ConditionTrue, "Merged", msg)
	o.Status.Message = msg
	log.FromContext(ctx).Info("auto-merged", "order", o.Name, "pr", d.PullRequest.URL, "rule", rule.Name)
	// The next read of the PR sees it merged and moves the order on.
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// checksError shortens GitHub's answer when the checks cannot be read; a 403
// means the GitHub App lacks the permissions.
func checksError(err error) string {
	if strings.Contains(err.Error(), "403") {
		return "403 Resource not accessible by integration (the GitHub App needs checks: read and statuses: read)"
	}
	return err.Error()
}

func (r *OrderReconciler) settle() time.Duration {
	if r.AutoMergeSettle > 0 {
		return r.AutoMergeSettle
	}
	return AutoMergeSettle
}

// prOpenedAt is when the order last reached phase p.
func (r *OrderReconciler) prOpenedAt(o *v1alpha1.Order, p v1alpha1.Phase) time.Time {
	for i := len(o.Status.History) - 1; i >= 0; i-- {
		if o.Status.History[i].Phase == p {
			return o.Status.History[i].Time.Time
		}
	}
	return o.CreationTimestamp.Time
}

// machineryFor returns the cluster's machinery client, or nil when none is
// configured.
func (r *OrderReconciler) machineryFor(ctx context.Context, mc *v1alpha1.ManagementCluster) (machinery.Client, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	return machineryClient(ctx, reader, r.Machinery, r.Namespace, mc)
}

// machineryClient dials the cluster's machinery, reading its token Secret
// with reader (namespace defaults to ns). nil, nil when the cluster has no
// spec.machinery or there is no dialer.
func machineryClient(ctx context.Context, reader client.Reader, d machinery.Dialer, ns string, mc *v1alpha1.ManagementCluster) (machinery.Client, error) {
	m := mc.Spec.Machinery
	if m == nil || d == nil {
		return nil, nil
	}
	ep := machinery.Endpoint{Address: m.Endpoint, Insecure: m.Insecure}
	if ref := m.TokenSecretRef; ref != nil {
		sns := ref.Namespace
		if sns == "" {
			sns = ns
		}
		key := ref.Key
		if key == "" {
			key = "token"
		}
		sec := &corev1.Secret{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: sns, Name: ref.Name}, sec); err != nil {
			return nil, fmt.Errorf("machinery token of %s: %w", mc.Name, err)
		}
		ep.Token = strings.TrimSpace(string(sec.Data[key]))
	}
	return d.Dial(ep)
}

// RevisionSHA takes the commit out of a Flux revision (main@sha1:<sha>,
// sha1:<sha> or a bare SHA).
func RevisionSHA(rev string) string {
	if i := strings.LastIndex(rev, ":"); i >= 0 {
		return rev[i+1:]
	}
	if i := strings.LastIndex(rev, "@"); i >= 0 {
		return rev[i+1:]
	}
	return rev
}

func (r *OrderReconciler) followApply(ctx context.Context, o *v1alpha1.Order) (ctrl.Result, error) {
	mc, err := r.cluster(ctx, o.Spec.Placement.ManagementCluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	mcl, err := r.machineryFor(ctx, mc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if mcl == nil {
		o.Status.Message = fmt.Sprintf("merged; ManagementCluster %s names no machinery, so the live status is unknown", mc.Name)
		return ctrl.Result{RequeueAfter: r.ReadyPoll}, nil
	}
	f := mc.Spec.Flux
	if f == nil {
		// No Flux check: Building as soon as machinery sees the XR.
		return r.live(ctx, o, mcl)
	}
	kind, ns, field := f.Kind, f.Namespace, f.RevisionField
	if kind == "" {
		kind = "Kustomization"
	}
	if ns == "" {
		ns = "flux-system"
	}
	if field == "" {
		field = "Revision"
	}
	k, err := mcl.Get(ctx, kind, f.Name, ns)
	if err != nil {
		return ctrl.Result{}, err
	}
	rev := ""
	if k.Found {
		rev = k.Info[field]
	}
	if rev == "" {
		o.Status.Message = fmt.Sprintf("merged; machinery reports no %s for %s %s/%s", field, kind, ns, f.Name)
		return ctrl.Result{RequeueAfter: r.PRPoll}, nil
	}
	if o.Status.Flux == nil {
		o.Status.Flux = &v1alpha1.FluxStatus{}
	}
	o.Status.Flux.LastAppliedRevision = rev
	d := o.Status.Delivery
	ok, err := r.Git.Contains(ctx, d.Repository, d.MergeSHA, RevisionSHA(rev))
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ok {
		o.Status.Message = fmt.Sprintf("merged; Flux is at %s, waiting for %s", short(RevisionSHA(rev)), short(d.MergeSHA))
		return ctrl.Result{RequeueAfter: r.PRPoll}, nil
	}
	now := metav1.Now()
	o.Status.Flux.AppliedAt = &now
	SetPhase(o, v1alpha1.PhaseApplied, "Flux applied "+short(RevisionSHA(rev)))
	return ctrl.Result{}, nil
}

func (r *OrderReconciler) followLive(ctx context.Context, o *v1alpha1.Order) (ctrl.Result, error) {
	mc, err := r.cluster(ctx, o.Spec.Placement.ManagementCluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	mcl, err := r.machineryFor(ctx, mc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if mcl == nil {
		o.Status.Message = fmt.Sprintf("ManagementCluster %s names no machinery, so the live status is unknown", mc.Name)
		return ctrl.Result{RequeueAfter: r.ReadyPoll}, nil
	}
	return r.live(ctx, o, mcl)
}

// live asks machinery for the XR and moves the order to Building or Ready.
func (r *OrderReconciler) live(ctx context.Context, o *v1alpha1.Order, mcl machinery.Client) (ctrl.Result, error) {
	x := o.Status.XRRef
	if x == nil {
		return ctrl.Result{}, errors.New("the order has no XR reference")
	}
	res, err := mcl.Get(ctx, x.Kind, x.Name, x.Namespace)
	if err != nil {
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
			Type: ConditionLive, Status: metav1.ConditionUnknown, Reason: "MachineryError", Message: err.Error(),
			ObservedGeneration: o.Generation,
		})
		return ctrl.Result{}, err
	}
	now := metav1.Now()
	// Once the XR was Ready, its absence means it is gone, not "not yet".
	wasReady := o.Status.Phase == v1alpha1.PhaseReady || o.Status.Phase == v1alpha1.PhaseDeleted
	if res.Found {
		o.Status.Live = &v1alpha1.LiveStatus{
			Found: true, Ready: res.Ready, StatusMessage: res.StatusMessage,
			Address: res.Address, Info: res.Info, ObservedAt: &now, LastSeenAt: &now,
		}
	} else {
		// Keep the last address and info: a gone VM's IP is still worth
		// knowing (DNS, inventory, ssh known_hosts).
		live := &v1alpha1.LiveStatus{}
		if wasReady && o.Status.Live != nil {
			live = o.Status.Live.DeepCopy()
		}
		live.Found, live.Ready, live.StatusMessage, live.ObservedAt = false, false, "", &now
		o.Status.Live = live
	}
	cond := metav1.Condition{Type: ConditionLive, Status: metav1.ConditionFalse, Reason: "NotFound",
		Message: fmt.Sprintf("machinery does not see %s %s yet", x.Kind, x.Name), ObservedGeneration: o.Generation}
	switch {
	case res.Found:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, "Observed", res.StatusMessage
	case wasReady:
		cond.Reason, cond.Message = "Gone", fmt.Sprintf("machinery no longer sees %s %s", x.Kind, x.Name)
	}
	meta.SetStatusCondition(&o.Status.Conditions, cond)

	switch {
	case res.Found && res.Ready:
		if o.Status.Phase != v1alpha1.PhaseReady {
			SetPhase(o, v1alpha1.PhaseReady, fmt.Sprintf("%s %s is Ready", x.Kind, x.Name))
		}
		return ctrl.Result{RequeueAfter: r.ReadyPoll}, nil
	case res.Found:
		if o.Status.Phase != v1alpha1.PhaseReady {
			SetPhase(o, v1alpha1.PhaseBuilding, res.StatusMessage)
		}
		return ctrl.Result{RequeueAfter: r.LivePoll}, nil
	case wasReady:
		SetPhase(o, v1alpha1.PhaseDeleted, fmt.Sprintf("%s %s is gone: machinery no longer sees it", x.Kind, x.Name))
		return ctrl.Result{RequeueAfter: r.ReadyPoll}, nil
	}
	return ctrl.Result{RequeueAfter: r.LivePoll}, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
