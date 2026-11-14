package ghauth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stuttgart-things/machinery-order-service/internal/ghauth"
)

func TestAppToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	parsed, err := ghauth.ParseKey(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/app/installations/7/access_tokens" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour)})
	}))
	defer srv.Close()
	app := &ghauth.App{AppID: "1", InstallationID: "7", Key: parsed, APIURL: srv.URL}
	for range 2 {
		tok, err := app.Token(context.Background())
		if err != nil || tok != "ghs_x" {
			t.Fatalf("token %q %v", tok, err)
		}
	}
	if calls != 1 {
		t.Errorf("token not cached: %d calls", calls)
	}
}
