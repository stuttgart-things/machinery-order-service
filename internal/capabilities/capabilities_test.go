package capabilities_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/capabilities"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery/machinerytest"
)

func xrd(f *machinerytest.Fake, name, group, kind, versions, owners string, conds ...machinery.Condition) {
	f.Set(capabilities.KindXRD, "", name, machinery.Resource{
		Info:       map[string]string{"Group": group, "Kind": kind, "Versions": versions, "Owners": owners},
		Conditions: conds,
	})
}

func fleet() *machinerytest.Fake {
	f := &machinerytest.Fake{}
	xrd(f, "nativeproxmoxvms.resources.stuttgart-things.com", "resources.stuttgart-things.com", "NativeProxmoxVM", "v1alpha1", "proxmox-vm-5c1d2e",
		machinery.Condition{Type: "Established", Status: "True"})
	xrd(f, "clusterstacks.config.stuttgart-things.com", "config.stuttgart-things.com", "ClusterStack", "v1alpha1, v1beta1", "cluster-9a8b7c")
	// installed by hand: no owner
	xrd(f, "miniobuckets.storage.stuttgart-things.com", "storage.stuttgart-things.com", "MinioBucket", "v1alpha1", "")
	// broken: not offered
	xrd(f, "brokens.x.example.com", "x.example.com", "Broken", "v1", "",
		machinery.Condition{Type: "Established", Status: "False", Reason: "EstablishFailed"})
	f.Set("Configuration", "", "proxmox-vm", machinery.Resource{Info: map[string]string{
		"Package": "ghcr.io/stuttgart-things/crossplane-configurations/proxmox-vm:v0.3.1", "Revision": "proxmox-vm-5c1d2e"}})
	f.Set("Configuration", "", "cluster", machinery.Resource{Info: map[string]string{
		"Package": "ghcr.io/stuttgart-things/crossplane-configurations/cluster@sha256:0123456789abcdef0123", "Revision": "cluster-9a8b7c"}})
	f.Set(capabilities.KindEnvironmentConfig, "", "proxmox-vm-labul", machinery.Resource{})
	f.Set(capabilities.KindEnvironmentConfig, "", "proxmox-vm-defaults", machinery.Resource{})
	return f
}

func TestCollect(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 6, 0, 0, 500, time.UTC)

	caps, err := capabilities.Collect(ctx, fleet(), now)
	if err != nil || caps == nil {
		t.Fatalf("%v %v", caps, err)
	}
	if !caps.ObservedAt.Time.Equal(now.Truncate(time.Second)) {
		t.Errorf("observedAt %v", caps.ObservedAt)
	}
	want := []v1alpha1.OfferedKind{
		{Kind: "ClusterStack", Group: "config.stuttgart-things.com", Versions: []string{"v1alpha1", "v1beta1"}, Configuration: "cluster", ConfigurationVersion: "sha256:0123456789ab"},
		{Kind: "NativeProxmoxVM", Group: "resources.stuttgart-things.com", Versions: []string{"v1alpha1"}, Configuration: "proxmox-vm", ConfigurationVersion: "v0.3.1"},
		{Kind: "MinioBucket", Group: "storage.stuttgart-things.com", Versions: []string{"v1alpha1"}},
	}
	if !reflect.DeepEqual(caps.Kinds, want) {
		t.Errorf("kinds\n got %+v\nwant %+v", caps.Kinds, want)
	}
	if !reflect.DeepEqual(caps.EnvironmentConfigs, []string{"proxmox-vm-defaults", "proxmox-vm-labul"}) {
		t.Errorf("environmentConfigs %v", caps.EnvironmentConfigs)
	}

	t.Run("XRDs not watched", func(t *testing.T) {
		f := fleet()
		f.Unwatch(capabilities.KindXRD)
		if caps, err := capabilities.Collect(ctx, f, now); caps != nil || err != nil {
			t.Errorf("%v %v", caps, err)
		}
	})
	t.Run("EnvironmentConfigs and Configurations not watched", func(t *testing.T) {
		f := fleet()
		f.Unwatch(capabilities.KindEnvironmentConfig, "Configuration")
		caps, err := capabilities.Collect(ctx, f, now)
		if err != nil || caps.EnvironmentConfigs != nil || len(caps.Kinds) != 3 || caps.Kinds[1].Configuration != "" {
			t.Errorf("%+v %v", caps, err)
		}
	})
	t.Run("no XRDs reported", func(t *testing.T) {
		// an unserved kind lists empty: no data, not "offers nothing"
		if caps, err := capabilities.Collect(ctx, &machinerytest.Fake{}, now); caps != nil || err != nil {
			t.Errorf("%+v %v", caps, err)
		}
	})
	t.Run("machinery down", func(t *testing.T) {
		f := fleet()
		f.SetDown(true)
		if _, err := capabilities.Collect(ctx, f, now); err == nil {
			t.Error("no error")
		}
	})
}

func TestCheck(t *testing.T) {
	now := time.Now()
	caps, err := capabilities.Collect(context.Background(), fleet(), now)
	if err != nil {
		t.Fatal(err)
	}
	mc := &v1alpha1.ManagementCluster{ObjectMeta: metav1.ObjectMeta{Name: "machinery-labul"}}
	mc.Status.Capabilities = caps

	for _, tc := range []struct {
		name, apiVersion, kind, want string
	}{
		{"offered", "resources.stuttgart-things.com/v1alpha1", "NativeProxmoxVM", ""},
		{"offered, second version", "config.stuttgart-things.com/v1beta1", "ClusterStack", ""},
		{"not installed", "resources.stuttgart-things.com/v1alpha1", "NativeVsphereVM",
			"NativeVsphereVM.resources.stuttgart-things.com is not installed on machinery-labul (no XRD)"},
		{"same kind, other group", "other.example.com/v1alpha1", "NativeProxmoxVM",
			"NativeProxmoxVM.other.example.com is not installed on machinery-labul (no XRD)"},
		{"wrong version", "resources.stuttgart-things.com/v1beta1", "NativeProxmoxVM",
			"machinery-labul offers NativeProxmoxVM.resources.stuttgart-things.com in v1alpha1, not v1beta1 (Configuration proxmox-vm v0.3.1)"},
		{"not established", "x.example.com/v1", "Broken", "Broken.x.example.com is not installed on machinery-labul (no XRD)"},
		{"core group", "v1", "ConfigMap", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := capabilities.Check(mc, capabilities.XR{APIVersion: tc.apiVersion, Kind: tc.kind}, now); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}

	t.Run("stale", func(t *testing.T) {
		if got := capabilities.Check(mc, capabilities.XR{APIVersion: "resources.stuttgart-things.com/v1alpha1", Kind: "NativeVsphereVM"}, now.Add(capabilities.MaxAge+time.Second)); got != "" {
			t.Errorf("stale capabilities checked: %q", got)
		}
	})
	t.Run("none", func(t *testing.T) {
		if got := capabilities.Check(&v1alpha1.ManagementCluster{}, capabilities.XR{APIVersion: "resources.stuttgart-things.com/v1alpha1", Kind: "NativeVsphereVM"}, now); got != "" {
			t.Errorf("no capabilities checked: %q", got)
		}
	})
	t.Run("no XRDs at all refuses", func(t *testing.T) {
		empty := &v1alpha1.ManagementCluster{ObjectMeta: metav1.ObjectMeta{Name: "bare"}}
		empty.Status.Capabilities = &v1alpha1.Capabilities{ObservedAt: metav1.NewTime(now)}
		if got := capabilities.Check(empty, capabilities.XR{APIVersion: "resources.stuttgart-things.com/v1alpha1", Kind: "NativeVsphereVM"}, now); !strings.Contains(got, "not installed on bare") {
			t.Errorf("got %q", got)
		}
	})
}

// fleetWithLabels is fleet() with the environment labels machinery renders
// since machinery#117, and the Configuration under the package manager's
// long name.
func fleetWithLabels() *machinerytest.Fake {
	f := fleet()
	f.Delete("Configuration", "", "proxmox-vm")
	f.Set("Configuration", "", "stuttgart-things-crossplane-configurations-proxmox-vm", machinery.Resource{Info: map[string]string{
		"Package": "ghcr.io/stuttgart-things/crossplane-configurations/proxmox-vm:v0.3.1", "Revision": "proxmox-vm-5c1d2e"}})
	f.Set(capabilities.KindEnvironmentConfig, "", "proxmox-vm-labul", machinery.Resource{Info: map[string]string{
		"Labels": "app.kubernetes.io/managed-by=flux, proxmox-vm.resources.stuttgart-things.com/environment=labul"}})
	f.Set(capabilities.KindEnvironmentConfig, "", "proxmox-vm-defaults", machinery.Resource{Info: map[string]string{
		"Labels": "proxmox-vm.resources.stuttgart-things.com/environment=default"}})
	f.Set(capabilities.KindEnvironmentConfig, "", "proxmox-vm-defaults-copy", machinery.Resource{Info: map[string]string{
		"Labels": "proxmox-vm.resources.stuttgart-things.com/environment=default"}})
	return f
}

func TestEnvironments(t *testing.T) {
	now := time.Now()
	caps, err := capabilities.Collect(context.Background(), fleetWithLabels(), now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"proxmox-vm.resources.stuttgart-things.com/environment": {"default", "labul"}}
	if !reflect.DeepEqual(caps.Environments, want) {
		t.Errorf("environments %v", caps.Environments)
	}
	mc := &v1alpha1.ManagementCluster{ObjectMeta: metav1.ObjectMeta{Name: "machinery-labul"}}
	mc.Status.Capabilities = caps
	vm := func(env string) capabilities.XR {
		return capabilities.XR{APIVersion: "resources.stuttgart-things.com/v1alpha1", Kind: "NativeProxmoxVM", Environment: env}
	}

	for _, tc := range []struct{ name, env, want string }{
		{"matching label", "labul", ""},
		{"default", "default", ""},
		{"no environmentConfig", "", ""},
		{"no EnvironmentConfig with it", "labda",
			"no EnvironmentConfig on machinery-labul has proxmox-vm.resources.stuttgart-things.com/environment=labda (it has: default, labul); the XR would build with the defaults"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := capabilities.Check(mc, vm(tc.env), now); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}

	t.Run("no label key for the configuration", func(t *testing.T) {
		// ClusterStack comes from `cluster`; no EnvironmentConfig carries
		// cluster.config.stuttgart-things.com/environment, so no check.
		xr := capabilities.XR{APIVersion: "config.stuttgart-things.com/v1alpha1", Kind: "ClusterStack", Environment: "anything"}
		if got := capabilities.Check(mc, xr, now); got != "" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("machinery without labels", func(t *testing.T) {
		caps, err := capabilities.Collect(context.Background(), fleet(), now)
		if err != nil || caps.Environments != nil {
			t.Fatalf("%v %v", caps.Environments, err)
		}
		old := &v1alpha1.ManagementCluster{ObjectMeta: metav1.ObjectMeta{Name: "x"}}
		old.Status.Capabilities = caps
		if got := capabilities.Check(old, vm("labda"), now); got != "" {
			t.Errorf("checked without labels: %q", got)
		}
	})
	t.Run("short Configuration name", func(t *testing.T) {
		c := caps.DeepCopy()
		for i := range c.Kinds {
			if c.Kinds[i].Kind == "NativeProxmoxVM" {
				c.Kinds[i].Configuration = "proxmox-vm"
			}
		}
		short := &v1alpha1.ManagementCluster{ObjectMeta: metav1.ObjectMeta{Name: "y"}}
		short.Status.Capabilities = c
		if got := capabilities.Check(short, vm("labda"), now); !strings.Contains(got, "environment=labda") {
			t.Errorf("got %q", got)
		}
	})
}
