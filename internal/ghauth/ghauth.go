// Package ghauth authenticates the service towards GitHub: a GitHub App
// installation (the bot identity, preferred) or a static token.
package ghauth

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Environment variables.
const (
	EnvAPIURL            = "GITHUB_API_URL"
	EnvToken             = "GITHUB_TOKEN"
	EnvAppID             = "GITHUB_APP_ID"
	EnvAppInstallationID = "GITHUB_APP_INSTALLATION_ID"
	EnvAppPrivateKeyFile = "GITHUB_APP_PRIVATE_KEY_FILE"

	// DefaultAPIURL is api.github.com.
	DefaultAPIURL = "https://api.github.com"
)

// Source hands out a GitHub token.
type Source interface {
	Token(ctx context.Context) (string, error)
}

// Static is a fixed token.
type Static string

// Token returns the token.
func (s Static) Token(context.Context) (string, error) { return string(s), nil }

// App mints installation tokens of a GitHub App and caches them until shortly
// before they expire.
type App struct {
	AppID          string
	InstallationID string
	Key            *rsa.PrivateKey
	APIURL         string
	HTTP           *http.Client
	Now            func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// FromEnv returns the configured source: the App when GITHUB_APP_* is set,
// else GITHUB_TOKEN; nil when neither is.
func FromEnv() (Source, string, error) {
	api := strings.TrimRight(os.Getenv(EnvAPIURL), "/")
	if api == "" {
		api = DefaultAPIURL
	}
	id, inst, keyFile := os.Getenv(EnvAppID), os.Getenv(EnvAppInstallationID), os.Getenv(EnvAppPrivateKeyFile)
	if id != "" || inst != "" || keyFile != "" {
		if id == "" || inst == "" || keyFile == "" {
			return nil, api, fmt.Errorf("%s, %s and %s go together", EnvAppID, EnvAppInstallationID, EnvAppPrivateKeyFile)
		}
		raw, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, api, fmt.Errorf("%s: %w", EnvAppPrivateKeyFile, err)
		}
		key, err := ParseKey(raw)
		if err != nil {
			return nil, api, fmt.Errorf("%s: %w", EnvAppPrivateKeyFile, err)
		}
		return &App{AppID: id, InstallationID: inst, Key: key, APIURL: api}, api, nil
	}
	if tok := os.Getenv(EnvToken); tok != "" {
		return Static(tok), api, nil
	}
	return nil, api, nil
}

// ParseKey reads a PEM RSA private key: PKCS#1 (GitHub's download) or PKCS#8.
func ParseKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("not an RSA private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA private key")
	}
	return rk, nil
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Token returns a cached installation token, minting a new one when it is
// about to expire.
func (a *App) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && a.now().Before(a.expires.Add(-5*time.Minute)) {
		return a.token, nil
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: a.Key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}
	now := a.now()
	appJWT, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer:   a.AppID,
		IssuedAt: jwt.NewNumericDate(now.Add(-60 * time.Second)),
		Expiry:   jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).Serialize()
	if err != nil {
		return "", fmt.Errorf("signing the App JWT: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(a.APIURL, "/")+"/app/installations/"+a.InstallationID+"/access_tokens", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	hc := a.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("installation token: GitHub answered %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		return "", fmt.Errorf("installation token: unreadable answer")
	}
	a.token, a.expires = out.Token, out.ExpiresAt
	return a.token, nil
}

// Transport adds the source's token to every request.
type Transport struct {
	Source Source
	Base   http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.Source == nil {
		return base.RoundTrip(r)
	}
	tok, err := t.Source.Token(r.Context())
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return base.RoundTrip(r)
}
