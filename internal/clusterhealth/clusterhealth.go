// Package clusterhealth decides whether a management cluster can take
// orders (#31). It reads the cluster only through machinery, like the rest
// of the service: whether machinery answers, the Flux Kustomization that
// applies the orders directory, and Crossplane's packages.
//
// The result is a set of conditions on ManagementCluster.status. Ready=False
// refuses new orders like spec.cordoned does; Unknown (no machinery, kinds
// not watched, a stale check) never blocks. The service does not cordon a
// cluster itself: spec.cordoned stays a human decision.
package clusterhealth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
)

// Condition types on a ManagementCluster.
const (
	Ready              = "Ready"
	MachineryReachable = "MachineryReachable"
	FluxReady          = "FluxReady"
	CrossplaneHealthy  = "CrossplaneHealthy"
)

// PackageKinds are the Crossplane package kinds, as machinery's watch set
// names them (stuttgart-things/flux cicd/machinery). A package with
// Healthy=False breaks every XR that depends on it; a Lock collision takes
// all of them down at once (crossplane-configurations CLAUDE.md).
var PackageKinds = []string{"Configuration", "Provider", "Function"}

// MaxAge is how old a check may be before its conditions count as
// Unknown: the controller checks every minute, so a check this old means
// the service is not checking (leader gone, controller stuck).
const MaxAge = 5 * time.Minute

// maxListed caps the unhealthy packages named in a message.
const maxListed = 3

// Check asks machinery about the cluster and returns its conditions, Ready
// last. cl is nil when the cluster has no spec.machinery; dialErr is set
// when the client could not be built (e.g. its token Secret is missing).
func Check(ctx context.Context, mc *v1alpha1.ManagementCluster, cl machinery.Client, dialErr error) []metav1.Condition {
	gen := mc.Generation
	cond := func(t string, s metav1.ConditionStatus, reason, msg string) metav1.Condition {
		return metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: gen}
	}
	if mc.Spec.Machinery == nil {
		why := "spec.machinery is not set; the service cannot see the cluster"
		return []metav1.Condition{
			cond(MachineryReachable, metav1.ConditionUnknown, "NoMachinery", why),
			cond(FluxReady, metav1.ConditionUnknown, "NoMachinery", why),
			cond(CrossplaneHealthy, metav1.ConditionUnknown, "NoMachinery", why),
			cond(Ready, metav1.ConditionUnknown, "NoMachinery", why),
		}
	}
	if dialErr != nil {
		why := dialErr.Error()
		return []metav1.Condition{
			cond(MachineryReachable, metav1.ConditionFalse, "DialFailed", why),
			cond(FluxReady, metav1.ConditionUnknown, "MachineryUnreachable", "machinery did not answer"),
			cond(CrossplaneHealthy, metav1.ConditionUnknown, "MachineryUnreachable", "machinery did not answer"),
			cond(Ready, metav1.ConditionFalse, "MachineryUnreachable", "machinery: "+why),
		}
	}

	// Every call below doubles as the reachability probe: an answer of any
	// kind (also "kind not watched") means machinery is there.
	var callErr error
	flux := checkFlux(ctx, mc, cl, &callErr)
	xp := checkPackages(ctx, cl, &callErr)
	flux.ObservedGeneration, xp.ObservedGeneration = gen, gen
	if callErr != nil {
		why := callErr.Error()
		return []metav1.Condition{
			cond(MachineryReachable, metav1.ConditionFalse, "Unreachable", why),
			cond(FluxReady, metav1.ConditionUnknown, "MachineryUnreachable", "machinery did not answer"),
			cond(CrossplaneHealthy, metav1.ConditionUnknown, "MachineryUnreachable", "machinery did not answer"),
			cond(Ready, metav1.ConditionFalse, "MachineryUnreachable", "machinery: "+why),
		}
	}
	reach := cond(MachineryReachable, metav1.ConditionTrue, "Answered", "machinery at "+mc.Spec.Machinery.Endpoint+" answered")

	ready := cond(Ready, metav1.ConditionTrue, "Healthy", "")
	var notes []string
	for _, c := range []metav1.Condition{flux, xp} {
		switch c.Status {
		case metav1.ConditionFalse:
			if ready.Status == metav1.ConditionTrue {
				ready = cond(Ready, metav1.ConditionFalse, c.Reason, c.Type+": "+c.Message)
			} else {
				ready.Message += "; " + c.Type + ": " + c.Message
			}
		case metav1.ConditionUnknown:
			notes = append(notes, c.Type+" unknown ("+c.Reason+")")
		}
	}
	if ready.Status == metav1.ConditionTrue {
		ready.Message = "machinery answers"
		if len(notes) > 0 {
			ready.Message += "; " + strings.Join(notes, ", ")
		}
	}
	return []metav1.Condition{reach, flux, xp, ready}
}

// checkFlux reads the Kustomization named in spec.flux.
func checkFlux(ctx context.Context, mc *v1alpha1.ManagementCluster, cl machinery.Client, callErr *error) metav1.Condition {
	f := mc.Spec.Flux
	if f == nil {
		return metav1.Condition{Type: FluxReady, Status: metav1.ConditionUnknown, Reason: "NotConfigured", Message: "spec.flux is not set"}
	}
	kind, ns := f.Kind, f.Namespace
	if kind == "" {
		kind = "Kustomization"
	}
	if ns == "" {
		ns = "flux-system"
	}
	name := ns + "/" + f.Name
	res, err := cl.Get(ctx, kind, f.Name, ns)
	if err != nil {
		*callErr = err
		return metav1.Condition{Type: FluxReady, Status: metav1.ConditionUnknown, Reason: "MachineryUnreachable"}
	}
	if !res.Found {
		return metav1.Condition{Type: FluxReady, Status: metav1.ConditionFalse, Reason: "KustomizationNotFound",
			Message: fmt.Sprintf("%s %s not found (or not in machinery's watch set)", kind, name)}
	}
	c := res.Condition("Ready")
	switch {
	case c != nil && c.Status == "True":
		return metav1.Condition{Type: FluxReady, Status: metav1.ConditionTrue, Reason: "Ready", Message: kind + " " + name + " is Ready"}
	case c != nil && c.Status == "False":
		return metav1.Condition{Type: FluxReady, Status: metav1.ConditionFalse, Reason: "KustomizationNotReady",
			Message: fmt.Sprintf("%s %s: %s", kind, name, firstNonEmpty(c.Message, c.Reason, "Ready=False"))}
	case c != nil:
		// Ready=Unknown while Flux reconciles: not a failure.
		return metav1.Condition{Type: FluxReady, Status: metav1.ConditionUnknown, Reason: "Reconciling",
			Message: fmt.Sprintf("%s %s: %s", kind, name, firstNonEmpty(c.Message, c.Reason, "Ready=Unknown"))}
	case res.Ready:
		return metav1.Condition{Type: FluxReady, Status: metav1.ConditionTrue, Reason: "Ready", Message: kind + " " + name + " is Ready"}
	default:
		return metav1.Condition{Type: FluxReady, Status: metav1.ConditionUnknown, Reason: "NoReadyCondition",
			Message: kind + " " + name + " reports no Ready condition yet"}
	}
}

// checkPackages lists Crossplane's packages: Healthy=False on any of them is
// a failure; a missing Healthy condition (just installed) is not.
func checkPackages(ctx context.Context, cl machinery.Client, callErr *error) metav1.Condition {
	var unwatched, unhealthy []string
	total := 0
	for _, kind := range PackageKinds {
		items, err := cl.List(ctx, kind)
		if errors.Is(err, machinery.ErrKindNotWatched) {
			unwatched = append(unwatched, kind)
			continue
		}
		if err != nil {
			*callErr = err
			return metav1.Condition{Type: CrossplaneHealthy, Status: metav1.ConditionUnknown, Reason: "MachineryUnreachable"}
		}
		total += len(items)
		for i := range items {
			if h := items[i].Condition("Healthy"); h != nil && h.Status == "False" {
				unhealthy = append(unhealthy, fmt.Sprintf("%s/%s: %s", kind, items[i].Name, firstNonEmpty(h.Message, h.Reason, "Healthy=False")))
			}
		}
	}
	if len(unwatched) == len(PackageKinds) {
		return metav1.Condition{Type: CrossplaneHealthy, Status: metav1.ConditionUnknown, Reason: "KindsNotWatched",
			Message: "machinery does not watch " + strings.Join(PackageKinds, ", ")}
	}
	if len(unhealthy) > 0 {
		sort.Strings(unhealthy)
		msg := fmt.Sprintf("%d of %d packages unhealthy: ", len(unhealthy), total)
		if len(unhealthy) > maxListed {
			msg += strings.Join(unhealthy[:maxListed], "; ") + fmt.Sprintf("; and %d more", len(unhealthy)-maxListed)
		} else {
			msg += strings.Join(unhealthy, "; ")
		}
		return metav1.Condition{Type: CrossplaneHealthy, Status: metav1.ConditionFalse, Reason: "PackagesUnhealthy", Message: truncate(msg, 1024)}
	}
	if total == 0 {
		return metav1.Condition{Type: CrossplaneHealthy, Status: metav1.ConditionUnknown, Reason: "NoPackages",
			Message: "machinery reports no Crossplane packages"}
	}
	msg := fmt.Sprintf("%d packages healthy", total)
	if len(unwatched) > 0 {
		msg += "; not watched: " + strings.Join(unwatched, ", ")
	}
	return metav1.Condition{Type: CrossplaneHealthy, Status: metav1.ConditionTrue, Reason: "Healthy", Message: msg}
}

// Effective is the cluster's Ready as an orderer should see it: the Ready
// condition, or Unknown when there is none or the last check is older than
// maxAge.
func Effective(mc *v1alpha1.ManagementCluster, now time.Time, maxAge time.Duration) (metav1.ConditionStatus, string) {
	c := meta.FindStatusCondition(mc.Status.Conditions, Ready)
	if c == nil {
		return metav1.ConditionUnknown, "not checked yet"
	}
	if at := mc.Status.CheckedAt; at == nil || now.Sub(at.Time) > maxAge {
		return metav1.ConditionUnknown, "last check is stale"
	}
	return c.Status, c.Message
}

// Blocking says whether the cluster refuses new orders for its health, and
// why. Only a recent Ready=False blocks.
func Blocking(mc *v1alpha1.ManagementCluster, now time.Time) (bool, string) {
	s, msg := Effective(mc, now, MaxAge)
	if s != metav1.ConditionFalse {
		return false, ""
	}
	return true, fmt.Sprintf("management cluster %s is not ready: %s", mc.Name, msg)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-len("…")] + "…"
}
