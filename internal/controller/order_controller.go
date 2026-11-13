// Package controller reconciles Orders: placement (pinned), render, delivery
// as a pull request, and the status from git, Flux and machinery:
//
//	Ordered → Placed → PROpen → Merged → Applied → Building → Ready
//	                     └→ Cancelled (cancel, or the PR closed unmerged)
//	Failed: the order cannot go on as it is (bad parameters, a taken name)
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
	"github.com/stuttgart-things/machinery-order-service/internal/delivery"
	"github.com/stuttgart-things/machinery-order-service/internal/intake"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
	"github.com/stuttgart-things/machinery-order-service/internal/render"
)

// Condition types on an Order.
const (
	// ConditionDelivered: the files are on a branch with an open or merged PR.
	ConditionDelivered = "Delivered"
	// ConditionLive: machinery answered for the XR.
	ConditionLive = "Live"
)

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

	before := o.Status.DeepCopy()
	res, err := r.step(ctx, o)
	o.Status.ObservedGeneration = o.Generation
	if err != nil {
		o.Status.Message = err.Error()
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
	if d := o.Status.Delivery; d != nil && d.PullRequest != nil && o.Status.Phase == v1alpha1.PhasePROpen {
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
		SetPhase(o, v1alpha1.PhaseOrdered, "accepted")
	}
	if o.Status.Phase.Terminal() {
		return ctrl.Result{}, nil
	}
	if o.Spec.Cancel && !o.Status.Phase.BeforeMerge() {
		o.Status.Message = "cancel refused: the order is merged; removing it needs a teardown"
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
		if o.Spec.Cancel {
			SetPhase(o, v1alpha1.PhaseCancelled, "cancelled before the delivery")
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
		p := o.Spec.Placement
		o.Status.Placement = &p
		SetPhase(o, v1alpha1.PhasePlaced, "pinned to "+p.ManagementCluster)
		return ctrl.Result{}, nil

	case v1alpha1.PhasePlaced:
		if o.Spec.Cancel {
			SetPhase(o, v1alpha1.PhaseCancelled, "cancelled before the delivery")
			return ctrl.Result{}, nil
		}
		return r.deliver(ctx, o)

	case v1alpha1.PhasePROpen:
		return r.followPR(ctx, o)

	case v1alpha1.PhaseMerged:
		return r.followApply(ctx, o)

	case v1alpha1.PhaseApplied, v1alpha1.PhaseBuilding, v1alpha1.PhaseReady:
		return r.followLive(ctx, o)
	}
	return ctrl.Result{}, nil
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
	o.Status.XRRef = xr
	o.Status.Files = intake.Paths(rd)

	tmpl, err := r.Renderer.Template(ctx, o.Spec.Template, "")
	if err != nil {
		return fail(o, "reading the template", err)
	}
	conv, err := intake.ConventionFor(o, rd, tmpl)
	if err != nil {
		return fail(o, "pull request", err)
	}
	repo := mc.Spec.GitOps.Repository
	if repo == "" {
		repo = "stuttgart-things/stuttgart-things"
	}
	base := mc.Spec.GitOps.BaseBranch
	if base == "" {
		base = "main"
	}
	req := delivery.Request{
		Repository: repo, BaseBranch: base, Branch: conv.Branch,
		Title: conv.Title, Body: conv.Body, OrderID: o.Name,
	}
	for _, f := range rd.Files {
		req.Files = append(req.Files, delivery.File{Path: f.Path, Content: f.Content})
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
	if o.Spec.Cancel {
		if err := r.Git.Close(ctx, d.Repository, d.PullRequest.Number, d.Branch); err != nil {
			return ctrl.Result{}, fmt.Errorf("closing the pull request: %w", err)
		}
		d.PullRequest.State = "closed"
		SetPhase(o, v1alpha1.PhaseCancelled, "cancelled: pull request closed")
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
		SetPhase(o, v1alpha1.PhaseMerged, "merged as "+short(pr.MergeSHA))
		return ctrl.Result{}, nil
	case "closed":
		SetPhase(o, v1alpha1.PhaseCancelled, "pull request closed without a merge")
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: r.PRPoll}, nil
}

// machineryFor returns the cluster's machinery client, or nil when none is
// configured.
func (r *OrderReconciler) machineryFor(ctx context.Context, mc *v1alpha1.ManagementCluster) (machinery.Client, error) {
	m := mc.Spec.Machinery
	if m == nil || r.Machinery == nil {
		return nil, nil
	}
	ep := machinery.Endpoint{Address: m.Endpoint, Insecure: m.Insecure}
	if ref := m.TokenSecretRef; ref != nil {
		ns := ref.Namespace
		if ns == "" {
			ns = r.Namespace
		}
		key := ref.Key
		if key == "" {
			key = "token"
		}
		sec := &corev1.Secret{}
		reader := r.APIReader
		if reader == nil {
			reader = r.Client
		}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, sec); err != nil {
			return nil, fmt.Errorf("machinery token of %s: %w", mc.Name, err)
		}
		ep.Token = strings.TrimSpace(string(sec.Data[key]))
	}
	return r.Machinery.Dial(ep)
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
		field = "lastAppliedRevision"
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
	o.Status.Live = &v1alpha1.LiveStatus{
		Found: res.Found, Ready: res.Ready, StatusMessage: res.StatusMessage,
		Address: res.Address, Info: res.Info, ObservedAt: &now,
	}
	cond := metav1.Condition{Type: ConditionLive, Status: metav1.ConditionFalse, Reason: "NotFound",
		Message: fmt.Sprintf("machinery does not see %s %s yet", x.Kind, x.Name), ObservedGeneration: o.Generation}
	if res.Found {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, "Observed", res.StatusMessage
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
	}
	return ctrl.Result{RequeueAfter: r.LivePoll}, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
