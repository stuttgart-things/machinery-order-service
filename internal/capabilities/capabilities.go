// Package capabilities derives what a management cluster can build from
// what is installed on it, through machinery (#32): its XRDs (kind, group,
// versions and the Configuration they come from) and its EnvironmentConfigs.
// Intake checks an order's rendered XR against them, so an order for a kind
// the cluster does not have fails before its pull request, not after the
// merge (Flux: "no matches for kind").
//
// No data never refuses an order: machinery does not watch the XRDs, or the
// capabilities are stale (fail-open; they only feed a comfort check). The
// scheduler (#25) will treat stale data the other way round.
package capabilities

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
)

// Kinds and info field labels as machinery's watch set names them
// (stuttgart-things/flux cicd/machinery).
const (
	KindXRD               = "CompositeResourceDefinition"
	KindEnvironmentConfig = "EnvironmentConfig"
	KindConfiguration     = "Configuration"

	fieldGroup    = "Group"
	fieldKind     = "Kind"
	fieldVersions = "Versions"
	fieldOwners   = "Owners"
	fieldPackage  = "Package"
	fieldRevision = "Revision"
)

// MaxAge is how old capabilities may be before intake ignores them.
const MaxAge = 15 * time.Minute

// Collect reads the capabilities. nil, nil: machinery does not watch the
// XRDs or reports none, so there is nothing to say. An error means machinery did not answer;
// the caller keeps what it had.
func Collect(ctx context.Context, cl machinery.Client, now time.Time) (*v1alpha1.Capabilities, error) {
	xrds, err := cl.List(ctx, KindXRD)
	if errors.Is(err, machinery.ErrKindNotWatched) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(xrds) == 0 {
		// machinery lists an unserved kind as empty, and a management
		// cluster without a single XRD has nothing to compare with either:
		// no data rather than "offers nothing", which would refuse every
		// order.
		return nil, nil
	}
	// ConfigurationRevision name -> package; an XRD's owner is the
	// revision that installed it, a Configuration's Revision info field is
	// its current one.
	pkgs := map[string]string{}
	cfgs, err := cl.List(ctx, KindConfiguration)
	if err != nil && !errors.Is(err, machinery.ErrKindNotWatched) {
		return nil, err
	}
	for _, c := range cfgs {
		if rev := c.Info[fieldRevision]; rev != "" {
			pkgs[rev] = c.Name + "|" + c.Info[fieldPackage]
		}
	}

	caps := &v1alpha1.Capabilities{ObservedAt: metav1.NewTime(now.Truncate(time.Second))}
	for _, x := range xrds {
		if c := x.Condition("Established"); c != nil && c.Status == "False" {
			continue
		}
		k := v1alpha1.OfferedKind{Kind: x.Info[fieldKind], Group: x.Info[fieldGroup], Versions: split(x.Info[fieldVersions])}
		if k.Kind == "" || k.Group == "" {
			continue
		}
		for _, owner := range split(x.Info[fieldOwners]) {
			if p, ok := pkgs[owner]; ok {
				name, ref, _ := strings.Cut(p, "|")
				k.Configuration, k.ConfigurationVersion = name, tag(ref)
				break
			}
		}
		caps.Kinds = append(caps.Kinds, k)
	}
	sort.Slice(caps.Kinds, func(i, j int) bool {
		if caps.Kinds[i].Group != caps.Kinds[j].Group {
			return caps.Kinds[i].Group < caps.Kinds[j].Group
		}
		return caps.Kinds[i].Kind < caps.Kinds[j].Kind
	})

	envs, err := cl.List(ctx, KindEnvironmentConfig)
	switch {
	case errors.Is(err, machinery.ErrKindNotWatched):
	case err != nil:
		return nil, err
	default:
		caps.EnvironmentConfigs = []string{}
		for _, e := range envs {
			caps.EnvironmentConfigs = append(caps.EnvironmentConfigs, e.Name)
		}
		sort.Strings(caps.EnvironmentConfigs)
	}
	return caps, nil
}

// Check says why the cluster cannot build the XR (apiVersion group/version,
// kind), or "" when it can or nothing is known.
func Check(mc *v1alpha1.ManagementCluster, apiVersion, kind string, now time.Time) string {
	caps := mc.Status.Capabilities
	if caps == nil || now.Sub(caps.ObservedAt.Time) > MaxAge {
		return ""
	}
	group, version, ok := strings.Cut(apiVersion, "/")
	if !ok {
		// core group: never an XR
		return ""
	}
	for _, k := range caps.Kinds {
		if k.Kind != kind || k.Group != group {
			continue
		}
		if len(k.Versions) == 0 || slices.Contains(k.Versions, version) {
			return ""
		}
		return fmt.Sprintf("%s offers %s.%s in %s, not %s%s", mc.Name, kind, group,
			strings.Join(k.Versions, ", "), version, from(k))
	}
	return fmt.Sprintf("%s.%s is not installed on %s (no XRD)", kind, group, mc.Name)
}

func from(k v1alpha1.OfferedKind) string {
	if k.Configuration == "" {
		return ""
	}
	s := " (Configuration " + k.Configuration
	if k.ConfigurationVersion != "" {
		s += " " + k.ConfigurationVersion
	}
	return s + ")"
}

// tag is the version part of a package reference: repo:tag, repo@sha256:...
func tag(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		d := ref[i+1:]
		if len(d) > len("sha256:")+12 {
			d = d[:len("sha256:")+12]
		}
		return d
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[i+1:]
	}
	return ""
}

// split reads machinery's flattening of a slice ("a, b").
func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
