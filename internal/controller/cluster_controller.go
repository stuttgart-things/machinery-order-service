package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/clusterhealth"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
)

// ClusterReconciler checks each ManagementCluster through machinery and
// writes the result to its status (#31). It only reads: like the order
// controller it has no kubeconfig for the cluster and changes nothing on it.
type ClusterReconciler struct {
	client.Client
	// APIReader reads Secrets uncached.
	APIReader client.Reader
	Machinery machinery.Dialer
	// Namespace is the service's namespace, the default for secret refs.
	Namespace string
	// Poll is how often a cluster is checked (default 1m). It must stay
	// well below clusterhealth.MaxAge, or every check counts as stale.
	Poll time.Duration
}

// +kubebuilder:rbac:groups=order.machinery.stuttgart-things.com,resources=managementclusters/status,verbs=get;update;patch

// SetupWithManager registers the reconciler. Status writes do not trigger
// it (generation predicate); the periodic requeue does the checking.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Poll == 0 {
		r.Poll = time.Minute
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ManagementCluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("managementcluster").
		Complete(r)
}

// Reconcile checks one cluster.
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	mc := &v1alpha1.ManagementCluster{}
	if err := r.Get(ctx, req.NamespacedName, mc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	cl, dialErr := machineryClient(ctx, reader, r.Machinery, r.Namespace, mc)
	if dialErr == nil && cl == nil && mc.Spec.Machinery != nil {
		// a cluster with spec.machinery but a service without a dialer
		// (never in serve): nothing to check against.
		return ctrl.Result{}, nil
	}
	conds := clusterhealth.Check(ctx, mc, cl, dialErr)

	before := mc.Status.DeepCopy()
	for _, c := range conds {
		meta.SetStatusCondition(&mc.Status.Conditions, c)
	}
	mc.Status.ObservedGeneration = mc.Generation
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	mc.Status.CheckedAt = &now
	if !equality.Semantic.DeepEqual(before, &mc.Status) {
		if err := r.Status().Update(ctx, mc); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: r.Poll}, nil
}
