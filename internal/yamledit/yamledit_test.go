package yamledit

import (
	"strings"
	"testing"
)

const xr = `
apiVersion: resources.stuttgart-things.com/v1alpha1
kind: NativeVsphereVM
metadata:
  name: demo-vm
  namespace: default
  annotations:
    stuttgart-things.com/source-path: a/demo.yaml # keep
spec:
  vm:
    name: demo
`

func TestSetMetadata(t *testing.T) {
	out, changed, err := SetMetadata(xr, "annotations", "stuttgart-things.com/template", "native-vm-labda-vsphere")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	want := `
apiVersion: resources.stuttgart-things.com/v1alpha1
kind: NativeVsphereVM
metadata:
  name: demo-vm
  namespace: default
  annotations:
    stuttgart-things.com/template: native-vm-labda-vsphere
    stuttgart-things.com/source-path: a/demo.yaml # keep
spec:
  vm:
    name: demo
`
	if out != want {
		t.Errorf("annotation:\n%s", out)
	}

	// no labels yet: the map is added under metadata
	out, _, err = SetMetadata(out, "labels", "stuttgart-things.com/order-id", "ord-0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	want = `
apiVersion: resources.stuttgart-things.com/v1alpha1
kind: NativeVsphereVM
metadata:
  labels:
    stuttgart-things.com/order-id: ord-0123456789ab
  name: demo-vm
  namespace: default
  annotations:
    stuttgart-things.com/template: native-vm-labda-vsphere
    stuttgart-things.com/source-path: a/demo.yaml # keep
spec:
  vm:
    name: demo
`
	if out != want {
		t.Errorf("label:\n%s", out)
	}

	// the same value again: unchanged; another value: refused
	if again, changed, err := SetMetadata(out, "labels", "stuttgart-things.com/order-id", "ord-0123456789ab"); err != nil || changed || again != out {
		t.Errorf("same value: changed=%v err=%v", changed, err)
	}
	if _, _, err := SetMetadata(out, "labels", "stuttgart-things.com/order-id", "ord-ffffffffffff"); err == nil {
		t.Error("another value accepted")
	}
	// a value that needs quoting
	q, _, err := SetMetadata(xr, "annotations", "x", "yes: no")
	if err != nil || !strings.Contains(q, "    x: 'yes: no'\n") {
		t.Errorf("quoted %v:\n%s", err, q)
	}
	for _, bad := range []string{"kind: X\n", "metadata: {name: a}\n", "metadata:\n  annotations: {a: b}\n"} {
		if _, _, err := SetMetadata(bad, "annotations", "k", "v"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
