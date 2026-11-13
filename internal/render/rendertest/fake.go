// Package rendertest is a fake claim-machinery-api renderer that renders a
// VM order the way KCL xr-native-vm does: the XR in source_path with the
// order-id label and annotation, and for use_case dev a ScrapeConfig with a
// target-path.
package rendertest

import (
	"context"
	"fmt"
	"sync"

	"github.com/stuttgart-things/machinery-order-service/internal/render"
)

// Fake records the calls.
type Fake struct {
	mu sync.Mutex
	// Tokens are the caller tokens seen, in order.
	Tokens []string
	// Err, when set, is returned by Render.
	Err error
}

// Template returns the VM template's type and defaults.
func (f *Fake) Template(_ context.Context, name, _ string) (*render.Template, error) {
	return &render.Template{Name: name, Type: "xr-native-vm", Defaults: map[string]any{
		"templateName": "labda-vsphere", "use_case": "baseos", "size": "medium",
	}}, nil
}

// Render renders a VM order from params.
func (f *Fake) Render(_ context.Context, _ string, params map[string]any, token string) (string, error) {
	f.mu.Lock()
	f.Tokens = append(f.Tokens, token)
	err := f.Err
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	s := func(k, def string) string {
		if v, ok := params[k].(string); ok && v != "" {
			return v
		}
		return def
	}
	name := s("name", "demo")
	src := s("source_path", "clusters/labda/vsphere/machinery-orders-xrs/"+name+".yaml")
	id := s("order_id", "")
	out := fmt.Sprintf(`---
apiVersion: resources.stuttgart-things.com/v1alpha1
kind: NativeVsphereVM
metadata:
  name: %[1]s-vm
  namespace: default
  labels:
    stuttgart-things.com/order-id: %[2]s
  annotations:
    stuttgart-things.com/management-cluster: %[3]s
    stuttgart-things.com/source-path: %[4]s
    stuttgart-things.com/order-id: %[2]s
spec:
  environmentConfig: %[5]s
  vm:
    name: %[1]s
`, name, id, s("management_cluster", "machinery"), src, s("environment_config", "labda"))
	if s("use_case", "baseos") == "dev" {
		out += fmt.Sprintf(`---
apiVersion: monitoring.coreos.com/v1alpha1
kind: ScrapeConfig
metadata:
  name: host-%[1]s
  annotations:
    stuttgart-things.com/target-path: clusters/labda/vsphere/sthings-infra/monitoring/host-%[1]s.yaml
    stuttgart-things.com/order-id: %[2]s
`, name, id)
	}
	return out, nil
}
