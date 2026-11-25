// Package capabilities derives what a management cluster can build from
// what is installed on it, through machinery (#32): its XRDs (kind, group,
// versions and the Configuration they come from) and its EnvironmentConfigs
// with their environment labels (machinery#117).
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
	fieldLabels   = "Labels"

	// environmentKeySuffix ends the label key an XR's Composition selects
	// its EnvironmentConfig by: <configuration>.<group>/environment.
	environmentKeySuffix = "/environment"
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
			labels, ok := e.Info[fieldLabels]
			if !ok {
				continue
			}
			if caps.Environments == nil {
				caps.Environments = map[string][]string{}
			}
			for key, value := range parseLabels(labels) {
				if strings.HasSuffix(key, environmentKeySuffix) && !slices.Contains(caps.Environments[key], value) {
					caps.Environments[key] = append(caps.Environments[key], value)
				}
			}
		}
		sort.Strings(caps.EnvironmentConfigs)
		for k := range caps.Environments {
			sort.Strings(caps.Environments[k])
		}
	}
	return caps, nil
}

// parseLabels reads machinery's rendering of a map ("k=v, k2=v2",
// machinery#117). Label values never contain "," or "=".
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for _, p := range strings.Split(s, ", ") {
		if k, v, ok := strings.Cut(p, "="); ok && k != "" {
			out[k] = v
		}
	}
	return out
}

// XR is what Check needs of an order's rendered XR.
type XR struct {
	APIVersion, Kind string
	// Environment is its spec.environmentConfig, "" when unset.
	Environment string
}

// Check says why the cluster cannot build the XR, or "" when it can or
// nothing is known: the kind (group/version) must be offered, and an
// spec.environmentConfig must match an EnvironmentConfig's environment
// label (<configuration>.<group>/environment) when the cluster has that
// label key at all.
func Check(mc *v1alpha1.ManagementCluster, xr XR, now time.Time) string {
	caps := mc.Status.Capabilities
	if caps == nil || now.Sub(caps.ObservedAt.Time) > MaxAge {
		return ""
	}
	group, version, ok := strings.Cut(xr.APIVersion, "/")
	if !ok {
		// core group: never an XR
		return ""
	}
	for _, k := range caps.Kinds {
		if k.Kind != xr.Kind || k.Group != group {
			continue
		}
		if len(k.Versions) > 0 && !slices.Contains(k.Versions, version) {
			return fmt.Sprintf("%s offers %s.%s in %s, not %s%s", mc.Name, xr.Kind, group,
				strings.Join(k.Versions, ", "), version, from(k))
		}
		return checkEnvironment(mc.Name, caps, k, xr.Environment)
	}
	return fmt.Sprintf("%s.%s is not installed on %s (no XRD)", xr.Kind, group, mc.Name)
}

// checkEnvironment matches the XR's spec.environmentConfig against the
// environment label of its Configuration. The label key's prefix is the
// package's short name (proxmox-vm), the Configuration's name may be the
// package manager's long one (stuttgart-things-crossplane-configurations-
// proxmox-vm). No label key for it: the convention does not apply here.
func checkEnvironment(cluster string, caps *v1alpha1.Capabilities, k v1alpha1.OfferedKind, env string) string {
	if env == "" || k.Configuration == "" || caps.Environments == nil {
		return ""
	}
	suffix := "." + k.Group + environmentKeySuffix
	for key, values := range caps.Environments {
		prefix, ok := strings.CutSuffix(key, suffix)
		if !ok || (prefix != k.Configuration && !strings.HasSuffix(k.Configuration, "-"+prefix)) {
			continue
		}
		if slices.Contains(values, env) {
			return ""
		}
		return fmt.Sprintf("no EnvironmentConfig on %s has %s=%s (it has: %s); the XR would build with the defaults",
			cluster, key, env, strings.Join(values, ", "))
	}
	return ""
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
