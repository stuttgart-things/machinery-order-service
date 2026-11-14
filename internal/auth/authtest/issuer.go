// Package authtest is a fake OIDC issuer for tests: discovery, JWKS and
// RS256 tokens, served over TLS with a certificate tests trust via a CA file.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Issuer is a running fake issuer.
type Issuer struct {
	Server *httptest.Server
	// CAFile is a PEM file with the server's certificate.
	CAFile string
	key    *rsa.PrivateKey
	keyID  string

	userinfoMu     sync.Mutex
	userinfo       map[string]any
	userinfoStatus int
	userinfoCalls  atomic.Int32
}

// NewIssuer starts an issuer that is stopped with the test.
func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss := &Issuer{key: key, keyID: "test-key"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                iss.URL(),
			"jwks_uri":                              iss.URL() + "/keys",
			"authorization_endpoint":                iss.URL() + "/authorize",
			"token_endpoint":                        iss.URL() + "/token",
			"userinfo_endpoint":                     iss.URL() + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: iss.keyID, Algorithm: "RS256", Use: "sig"},
		}})
	})
	// Userinfo answers SetUserinfo's claims to any bearer token (it does not
	// check the token: the verifier did) and counts the calls.
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		iss.userinfoCalls.Add(1)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		iss.userinfoMu.Lock()
		status, claims := iss.userinfoStatus, iss.userinfo
		iss.userinfoMu.Unlock()
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(claims)
	})
	iss.Server = httptest.NewTLSServer(mux)
	t.Cleanup(iss.Server.Close)

	iss.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: iss.Server.Certificate().Raw})
	if err := os.WriteFile(iss.CAFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return iss
}

// URL is the issuer URL.
func (i *Issuer) URL() string { return i.Server.URL }

// Claims returns valid claims for audience: iss, sub, aud, iat, nbf and an
// exp an hour from now. Tests change them before Sign.
func (i *Issuer) Claims(audience string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":                i.URL(),
		"sub":                "123456789",
		"aud":                []string{audience},
		"iat":                now.Unix(),
		"nbf":                now.Add(-time.Minute).Unix(),
		"exp":                now.Add(time.Hour).Unix(),
		"preferred_username": "patrick@sthings.example",
	}
}

// Sign returns claims as an RS256 JWT signed with the issuer's key.
func (i *Issuer) Sign(t testing.TB, claims map[string]any) string {
	t.Helper()
	return SignWith(t, i.key, i.keyID, claims)
}

// SignWith signs with another key (for bad-signature tests).
func SignWith(t testing.TB, key *rsa.PrivateKey, keyID string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", keyID))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// SetUserinfo sets what /userinfo answers: status (0 = 200) and claims.
func (i *Issuer) SetUserinfo(status int, claims map[string]any) {
	i.userinfoMu.Lock()
	defer i.userinfoMu.Unlock()
	i.userinfoStatus, i.userinfo = status, claims
}

// UserinfoCalls is how often /userinfo was asked.
func (i *Issuer) UserinfoCalls() int { return int(i.userinfoCalls.Load()) }

// KeyID is the kid of the issuer's key.
func (i *Issuer) KeyID() string { return i.keyID }
