package main

import (
	"context"
	"dagger/dagger/internal/dagger"
	"fmt"
)

// BuildAndTest builds the binary and runs every test, the envtest suites
// included (controller, serve end to end): setup-envtest downloads the
// kube-apiserver and etcd binaries into the container.
func (m *Dagger) BuildAndTest(
	ctx context.Context,
	src *dagger.Directory,
	// +optional
	// +default="1.27.4"
	goVersion string,
	// +optional
	// +default="1.37.0"
	envtestK8sVersion string,
	// +optional
	// +default="./..."
	testArg string,
) (string, error) {
	// The binary first: a build error is clearer than a test error.
	if _, err := m.Build(ctx, src, goVersion, "linux", "amd64", "main.go", "machinery-order-service", "bookworm", "", "").
		Entries(ctx); err != nil {
		return "", fmt.Errorf("build: %w", err)
	}

	ctr := dag.Container().
		From("golang:"+goVersion+"-bookworm").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("go-mod-"+goVersion)).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("go-build-"+goVersion)).
		WithMountedCache("/envtest", dag.CacheVolume("envtest-"+envtestK8sVersion)).
		WithEnvVariable("GOTOOLCHAIN", "local").
		WithDirectory("/src", src).
		WithWorkdir("/src").
		WithExec([]string{"go", "install", "sigs.k8s.io/controller-runtime/tools/setup-envtest@latest"}).
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			`set -e
assets="$(setup-envtest use %s --bin-dir /envtest -p path)"
KUBEBUILDER_ASSETS="$assets" go test -count=1 -cover %s`, envtestK8sVersion, testArg)})

	out, err := ctr.Stdout(ctx)
	if err != nil {
		if e, ok := err.(*dagger.ExecError); ok {
			return "", fmt.Errorf("tests failed:\n%s\n%s", e.Stdout, e.Stderr)
		}
		return "", err
	}
	return out, nil
}
