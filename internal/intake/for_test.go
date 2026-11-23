package intake_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stuttgart-things/machinery-order-service/internal/intake"
	"github.com/stuttgart-things/machinery-order-service/internal/render"
)

// claimAPI answers order and template calls and counts them.
func claimAPI(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "native-vm-labda-vsphere"}, "spec": map[string]any{"type": "xr"}})
			return
		}
		rendered := "apiVersion: x/v1\nkind: VM\nmetadata:\n  name: demo\n  annotations:\n    stuttgart-things.com/source-path: clusters/labda/vsphere/machinery-orders-xrs/demo.yaml\n"
		_ = json.NewEncoder(w).Encode(map[string]any{"rendered": rendered})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRenderUsesClusterClaimAPI(t *testing.T) {
	var global, staging int
	g, s := claimAPI(t, &global), claimAPI(t, &staging)
	c := &render.Client{BaseURL: g.URL, Service: func(context.Context) (string, error) { return "svc", nil }}

	mc := cluster()
	if _, err := intake.Render(context.Background(), c, order(t, map[string]any{"name": "demo"}), mc, "caller"); err != nil {
		t.Fatal(err)
	}
	if global != 1 || staging != 0 {
		t.Fatalf("without claimApiUrl: global %d staging %d calls, want 1/0", global, staging)
	}

	mc.Spec.Render.ClaimAPIURL = s.URL + "/"
	if _, err := intake.Render(context.Background(), c, order(t, map[string]any{"name": "demo"}), mc, "caller"); err != nil {
		t.Fatal(err)
	}
	if _, err := intake.For(c, mc).Template(context.Background(), "native-vm-labda-vsphere", ""); err != nil {
		t.Fatal(err)
	}
	if global != 1 || staging != 2 {
		t.Fatalf("with claimApiUrl: global %d staging %d calls, want 1/2", global, staging)
	}
	if c.BaseURL != g.URL {
		t.Fatalf("For changed the shared client: %s", c.BaseURL)
	}
}
