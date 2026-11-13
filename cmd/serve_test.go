package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	maschinist "github.com/stuttgart-things/machinery/resourceservice"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery/githubfake"
	"github.com/stuttgart-things/machinery-order-service/internal/render/rendertest"
)

type machineryServer struct {
	maschinist.UnimplementedResourceServiceServer
	ready bool
}

func (m *machineryServer) GetResourceDetail(_ context.Context, r *maschinist.ResourceDetailRequest) (*maschinist.ResourceStatus, error) {
	if r.Kind == "NativeVsphereVM" && r.Name == "e2e-vm" {
		return &maschinist.ResourceStatus{Name: r.Name, Kind: r.Kind, Ready: m.ready, ConnectionDetails: "10.31.103.42"}, nil
	}
	return nil, status.Error(codes.NotFound, "no")
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// TestServe runs the whole service as main does, against envtest, a fake
// GitHub, a fake claim-machinery-api and a fake machinery: an order through
// the REST API ends Ready.
func TestServe(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS not set (task envtest)")
	}
	te := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "config", "crd")}, ErrorIfCRDPathMissing: true, BinaryAssetsDirectory: assets}
	cfg, err := te.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = te.Stop() }()

	// kubeconfig for ctrl.GetConfig.
	kc := clientcmdapi.NewConfig()
	kc.Clusters["e"] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData}
	kc.AuthInfos["e"] = &clientcmdapi.AuthInfo{ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData}
	kc.Contexts["e"] = &clientcmdapi.Context{Cluster: "e", AuthInfo: "e"}
	kc.CurrentContext = "e"
	kcPath := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kc, kcPath); err != nil {
		t.Fatal(err)
	}

	gh := githubfake.New("stuttgart-things", "stuttgart-things", map[string]string{
		"clusters/labda/vsphere/machinery-orders-xrs/kustomization.yaml": "kind: Kustomization\nresources:\n  - keep.yaml\n",
	})
	defer gh.Close()
	fr := &rendertest.Fake{}
	claimAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/claim-templates/")
		if strings.HasSuffix(name, "/order") {
			var body struct {
				Parameters map[string]any `json:"parameters"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			out, _ := fr.Render(r.Context(), name, body.Parameters, "")
			_ = json.NewEncoder(w).Encode(map[string]string{"rendered": out})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": name}, "spec": map[string]any{"type": "xr-native-vm"}})
	}))
	defer claimAPI.Close()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ms := &machineryServer{}
	gs := grpc.NewServer()
	maschinist.RegisterResourceServiceServer(gs, ms)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	port := freePort(t)
	for k, v := range map[string]string{
		"KUBECONFIG": kcPath, "ORDER_NAMESPACE": "default", "PORT": port, "METRICS_ADDR": "0", "PROBE_ADDR": "0",
		"CLAIM_API_URL": claimAPI.URL, "GITHUB_API_URL": gh.URL, "GITHUB_TOKEN": "t",
		"GITHUB_APP_ID": "", "GITHUB_APP_INSTALLATION_ID": "", "GITHUB_APP_PRIVATE_KEY_FILE": "", "AUTH_OIDC_ISSUER": "",
		"PR_POLL_INTERVAL": "200ms", "LIVE_POLL_INTERVAL": "200ms", "BASE_PATH": "/orders",
	} {
		t.Setenv(k, v)
	}

	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), &v1alpha1.ManagementCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "machinery"},
		Spec: v1alpha1.ManagementClusterSpec{
			GitOps:    v1alpha1.GitOps{OrdersPath: "clusters/labda/vsphere/machinery-orders-xrs"},
			Machinery: &v1alpha1.Machinery{Endpoint: lis.Addr().String(), Insecure: true},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("serve did not stop")
		}
	}()

	base := "http://127.0.0.1:" + port + "/orders"
	waitFor(t, "ready", func() bool {
		resp, err := http.Get(base + "/ready")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == 200
	})

	body, _ := json.Marshal(map[string]any{"template": "native-vm-labda-vsphere", "managementCluster": "machinery", "parameters": map[string]any{"name": "e2e"}})
	resp, err := http.Post(base+"/api/v1/orders", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var created v1alpha1.Order
	_ = json.NewDecoder(resp.Body).Decode(&created)
	_ = resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d", resp.StatusCode)
	}

	order := func() v1alpha1.Order {
		var o v1alpha1.Order
		resp, err := http.Get(base + "/api/v1/orders/" + created.Name)
		if err == nil {
			_ = json.NewDecoder(resp.Body).Decode(&o)
			_ = resp.Body.Close()
		}
		return o
	}
	waitFor(t, "PROpen", func() bool { return order().Status.Phase == v1alpha1.PhasePROpen })
	gh.Merge(1)
	waitFor(t, "Building", func() bool { return order().Status.Phase == v1alpha1.PhaseBuilding })
	ms.ready = true
	waitFor(t, "Ready", func() bool { return order().Status.Phase == v1alpha1.PhaseReady })
	if o := order(); o.Status.Live == nil || o.Status.Live.Address != "10.31.103.42" {
		t.Errorf("live %+v", o.Status.Live)
	}
	resp, err = http.Get(base + "/ui/")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("ui: %v %v", resp, err)
	}
	_ = resp.Body.Close()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
