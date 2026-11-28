package capabilities

import (
	"fmt"
	"strings"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
)

// Allowed says why the cluster does not take orders for the XR's kind, or ""
// when it does: spec.allowedKinds is empty (every kind) or lists the kind as
// `Kind` or `Kind.group`.
//
// Unlike Check it does not depend on what machinery reports: it is the
// cluster's configuration, so it always applies (fail-closed), also while
// the capabilities are unknown or stale. The scheduler (#25) is meant to use
// it as a filter when it picks a cluster.
func Allowed(mc *v1alpha1.ManagementCluster, xr XR) string {
	if len(mc.Spec.AllowedKinds) == 0 || Listed(mc.Spec.AllowedKinds, xr) {
		return ""
	}
	what := Name(xr)
	return fmt.Sprintf("%s takes no %s orders (allowedKinds: %s)", mc.Name, what, strings.Join(mc.Spec.AllowedKinds, ", "))
}

// Listed reports whether a list written like allowedKinds (`Kind` or
// `Kind.group`) names the XR's kind.
func Listed(list []string, xr XR) bool {
	group := "" // core group: apiVersion without "/"
	if g, _, ok := strings.Cut(xr.APIVersion, "/"); ok {
		group = g
	}
	for _, a := range list {
		kind, g, hasGroup := strings.Cut(a, ".")
		if kind == xr.Kind && (!hasGroup || g == group) {
			return true
		}
	}
	return false
}

// Name is Kind.group (Kind for the core group).
func Name(xr XR) string {
	if g, _, ok := strings.Cut(xr.APIVersion, "/"); ok {
		return xr.Kind + "." + g
	}
	return xr.Kind
}
