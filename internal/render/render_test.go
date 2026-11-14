package render_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stuttgart-things/machinery-order-service/internal/render"
)

func TestRender(t *testing.T) {
	var gotAuth string
	var gotParams map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/claim-templates/native-vm/order" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Parameters map[string]any `json:"parameters"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotParams = body.Parameters
		if body.Parameters["size"] == "huge" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid parameters","fields":{"size":"not allowed"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "x", "kind": "OrderResponse", "rendered": "kind: A\n"})
	}))
	defer srv.Close()

	c := &render.Client{BaseURL: srv.URL, Service: func(context.Context) (string, error) { return "svc", nil }}
	out, err := c.Render(context.Background(), "native-vm", map[string]any{"name": "demo"}, "")
	if err != nil || out != "kind: A\n" {
		t.Fatalf("render: %q %v", out, err)
	}
	if gotAuth != "Bearer svc" || gotParams["name"] != "demo" {
		t.Errorf("service token: auth %q params %v", gotAuth, gotParams)
	}

	if _, err := c.Render(context.Background(), "native-vm", nil, "caller"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer caller" {
		t.Errorf("caller token not forwarded: %q", gotAuth)
	}

	_, err = c.Render(context.Background(), "native-vm", map[string]any{"size": "huge"}, "")
	var ae *render.APIError
	if !errors.As(err, &ae) || ae.Status != 400 || ae.Fields["size"] != "not allowed" || !render.IsCallerError(err) {
		t.Errorf("want a 400 with fields, got %v", err)
	}
}
