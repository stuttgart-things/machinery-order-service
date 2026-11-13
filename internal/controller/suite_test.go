package controller_test

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/controller"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery/githubfake"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery/machinerytest"
	"github.com/stuttgart-things/machinery-order-service/internal/render/rendertest"
)

// env is one envtest API server with the controller running against a fake
// GitHub, a fake claim-machinery-api and a fake machinery.
type env struct {
	c         client.Client
	git       *githubfake.Server
	renderer  *rendertest.Fake
	machinery *machinerytest.Fake
}

// assetsDir finds the envtest binaries: KUBEBUILDER_ASSETS, else
// setup-envtest's cache.
func assetsDir(t *testing.T) string {
	if d := os.Getenv("KUBEBUILDER_ASSETS"); d != "" {
		return d
	}
	if p, err := exec.LookPath("setup-envtest"); err == nil {
		out, err := exec.Command(p, "use", "-i", "-p", "path").Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			return strings.TrimSpace(string(out))
		}
	}
	t.Skip("no envtest binaries: set KUBEBUILDER_ASSETS or install setup-envtest (task envtest)")
	return ""
}

const kust = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - README-keep.yaml\n"

func start(t *testing.T) *env {
	t.Helper()
	te := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: assetsDir(t),
	}
	cfg, err := te.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = te.Stop() })

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	e := &env{
		git: githubfake.New("stuttgart-things", "stuttgart-things", map[string]string{
			"clusters/labda/vsphere/machinery-orders-xrs/kustomization.yaml": kust,
		}),
		renderer:  &rendertest.Fake{},
		machinery: &machinerytest.Fake{},
	}
	t.Cleanup(e.git.Close)
	gh, err := delivery.New(e.git.URL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}

	skip := true
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		// Every test starts its own manager in this process.
		Controller: config.Controller{SkipNameValidation: &skip},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &controller.OrderReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(),
		Renderer: e.renderer, Git: gh, Machinery: e.machinery, Namespace: "default",
		PRPoll: 100 * time.Millisecond, LivePoll: 100 * time.Millisecond, ReadyPoll: 100 * time.Millisecond,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()

	e.c, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) cluster(t *testing.T, flux bool) {
	t.Helper()
	mc := &v1alpha1.ManagementCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "machinery"},
		Spec: v1alpha1.ManagementClusterSpec{
			Lab:       "labda",
			GitOps:    v1alpha1.GitOps{OrdersPath: "clusters/labda/vsphere/machinery-orders-xrs"},
			Machinery: &v1alpha1.Machinery{Endpoint: "machinery:50051", Insecure: true, TokenSecretRef: &v1alpha1.SecretKeyRef{Name: "machinery-grpc"}},
		},
	}
	if flux {
		mc.Spec.Flux = &v1alpha1.Flux{Name: "machinery-orders-xrs"}
	}
	if err := e.c.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "machinery-grpc", Namespace: "default"},
		StringData: map[string]string{"token": "s3cret"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Create(context.Background(), mc); err != nil {
		t.Fatal(err)
	}
}

func (e *env) order(t *testing.T, name string, params string) *v1alpha1.Order {
	t.Helper()
	o := &v1alpha1.Order{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.OrderSpec{
			Template:   "native-vm-labda-vsphere",
			Parameters: &runtime.RawExtension{Raw: []byte(params)},
			Placement:  v1alpha1.Placement{ManagementCluster: "machinery"},
		},
	}
	if err := e.c.Create(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	return o
}

// wait polls the order until cond holds.
func (e *env) wait(t *testing.T, name string, what string, cond func(*v1alpha1.Order) bool) *v1alpha1.Order {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	o := &v1alpha1.Order{}
	for time.Now().Before(deadline) {
		if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, o); err == nil && cond(o) {
			return o
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: timed out; phase %q message %q", what, o.Status.Phase, o.Status.Message)
	return nil
}

func phase(p v1alpha1.Phase) func(*v1alpha1.Order) bool {
	return func(o *v1alpha1.Order) bool { return o.Status.Phase == p }
}
