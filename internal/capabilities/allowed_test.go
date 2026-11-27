package capabilities

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
)

func TestAllowed(t *testing.T) {
	bucket := XR{APIVersion: "storage.stuttgart-things.com/v1alpha1", Kind: "MinioBucket"}
	vm := XR{APIVersion: "resources.stuttgart-things.com/v1alpha1", Kind: "NativeVsphereVM"}
	mc := func(allowed ...string) *v1alpha1.ManagementCluster {
		return &v1alpha1.ManagementCluster{ObjectMeta: metav1.ObjectMeta{Name: "machinery"}, Spec: v1alpha1.ManagementClusterSpec{AllowedKinds: allowed}}
	}
	for _, c := range []struct {
		name    string
		allowed []string
		xr      XR
		want    string
	}{
		{"empty allows every kind", nil, vm, ""},
		{"kind listed", []string{"MinioBucket"}, bucket, ""},
		{"kind with group listed", []string{"MinioBucket.storage.stuttgart-things.com"}, bucket, ""},
		{"one of several", []string{"NativeProxmoxVM", "MinioBucket"}, bucket, ""},
		{"kind not listed", []string{"MinioBucket"}, vm,
			"machinery takes no NativeVsphereVM.resources.stuttgart-things.com orders (allowedKinds: MinioBucket)"},
		{"same kind, other group", []string{"MinioBucket.other.example.com"}, bucket,
			"machinery takes no MinioBucket.storage.stuttgart-things.com orders (allowedKinds: MinioBucket.other.example.com)"},
		{"kind is case-sensitive", []string{"miniobucket"}, bucket,
			"machinery takes no MinioBucket.storage.stuttgart-things.com orders (allowedKinds: miniobucket)"},
		{"core group (no /)", []string{"ConfigMap"}, XR{APIVersion: "v1", Kind: "ConfigMap"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Allowed(mc(c.allowed...), c.xr); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
