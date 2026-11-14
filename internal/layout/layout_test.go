package layout_test

import (
	"strings"
	"testing"

	"github.com/stuttgart-things/machinery-order-service/internal/layout"
)

const rendered = `---
apiVersion: resources.stuttgart-things.com/v1alpha1
kind: NativeVsphereVM
metadata:
  name: demo-vm
  namespace: default
  labels:
    stuttgart-things.com/order-id: ord-1
  annotations:
    stuttgart-things.com/source-path: clusters/labda/vsphere/machinery-orders-xrs/demo.yaml
spec: {}
---
apiVersion: monitoring.coreos.com/v1alpha1
kind: ScrapeConfig
metadata:
  name: host-demo
  annotations:
    stuttgart-things.com/target-path: clusters/labda/vsphere/sthings-infra/monitoring/host-demo.yaml
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: follows-the-xr
`

func TestPlan(t *testing.T) {
	files, err := layout.Plan(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	if files[0].Path != "clusters/labda/vsphere/machinery-orders-xrs/demo.yaml" || len(files[0].Documents) != 2 {
		t.Errorf("first file: %s with %d documents", files[0].Path, len(files[0].Documents))
	}
	if files[1].Path != "clusters/labda/vsphere/sthings-infra/monitoring/host-demo.yaml" {
		t.Errorf("second file: %s", files[1].Path)
	}
	if !strings.HasPrefix(files[0].Content(), "---\napiVersion: resources.stuttgart-things.com/v1alpha1") {
		t.Errorf("content: %q", files[0].Content())
	}
	xr, ok := layout.XR(files)
	if !ok || xr.Kind != "NativeVsphereVM" || xr.Name != "demo-vm" || xr.Namespace != "default" {
		t.Errorf("xr: %+v", xr)
	}
}

func TestPlanRefuses(t *testing.T) {
	for name, in := range map[string]string{
		"no path":  "kind: A\nmetadata:\n  name: x\n",
		"absolute": "kind: A\nmetadata:\n  name: x\n  annotations:\n    stuttgart-things.com/source-path: /etc/x.yaml\n",
		"escapes":  "kind: A\nmetadata:\n  name: x\n  annotations:\n    stuttgart-things.com/source-path: a/../../x.yaml\n",
		"not yaml": "kind: A\nmetadata:\n  name: x\n  annotations:\n    stuttgart-things.com/source-path: a/x.txt\n",
		"empty":    "---\n# nothing\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := layout.Plan(in); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestAppendEntry(t *testing.T) {
	raw := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - a.yaml\n"
	after, line, err := layout.AppendEntry("k.yaml", raw, "b.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if line != "  - b.yaml" || after != raw+"  - b.yaml\n" {
		t.Errorf("got %q / %q", line, after)
	}
	same, line, err := layout.AppendEntry("k.yaml", after, "b.yaml")
	if err != nil || line != "" || same != after {
		t.Errorf("not idempotent: %q %q %v", same, line, err)
	}
	if _, _, err := layout.AppendEntry("k.yaml", "resources:\n  - a.yaml\nnamespace: x\n", "b.yaml"); err == nil {
		t.Error("want an error when resources: is not the last key")
	}
}
