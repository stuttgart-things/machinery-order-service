// Package auth checks who may order (stuttgart-things/stuttgart-things#4297).
//
// It is a copy of claim-machinery-api's internal/auth (#119, #122): the same
// environment variables, the same ZITADEL JWT check and the same
// sthings-portal forwarded identity. Keep both in sync.
//
// Off by default: without AUTH_OIDC_ISSUER every request may order. With it,
// the write endpoints need a JWT of that issuer as bearer token: signature
// against the issuer's JWKS (OIDC discovery), iss, exp/nbf and aud are checked
// by github.com/coreos/go-oidc/v3, the roles by this package. The forwarded
// headers (X-Forwarded-User / X-Forwarded-Groups) are only accepted by the
// web UI, and only with AUTH_TRUST_FORWARDED_USER.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Environment variables of the order auth.
const (
	EnvIssuer             = "AUTH_OIDC_ISSUER"
	EnvAudience           = "AUTH_OIDC_AUDIENCE"
	EnvCAFile             = "AUTH_OIDC_CA_FILE"
	EnvOrderRoles         = "AUTH_ORDER_ROLES"
	EnvRolesClaim         = "AUTH_ROLES_CLAIM"
	EnvTrustForwardedUser = "AUTH_TRUST_FORWARDED_USER"
	EnvAdminRoles         = "AUTH_ADMIN_ROLES"
	EnvRoleGroups         = "AUTH_ROLE_GROUPS"
)

// ZitadelRolesClaim is where ZITADEL puts a user's project roles (an object
// whose keys are the roles). Project-specific variants
// (urn:zitadel:iam:org:project:<id>:roles) are read as well.
const ZitadelRolesClaim = "urn:zitadel:iam:org:project:roles"

// The identity headers sthings-portal sets for the services behind it
// (sthings-portal internal/auth/auth.go), after removing what a client sent
// under these names. Groups are comma-separated.
const (
	HeaderForwardedUser   = "X-Forwarded-User"
	HeaderForwardedGroups = "X-Forwarded-Groups"
)

// maxForwardedUser bounds the name taken from X-Forwarded-User.
const maxForwardedUser = 128

// maxCachedNames bounds the userinfo name cache; when it is full after
// dropping the expired entries it starts over.
const maxCachedNames = 1024

// maxUserinfoBody bounds the userinfo response read.
const maxUserinfoBody = 1 << 20

// retryDiscovery is how long a failed discovery is remembered before the
// next request tries again.
const retryDiscovery = 5 * time.Second

// Config is the order auth's configuration. The zero value is off.
type Config struct {
	// Issuer is the OIDC issuer URL (AUTH_OIDC_ISSUER); empty turns auth off.
	Issuer string
	// Audiences are the accepted "aud" values (AUTH_OIDC_AUDIENCE, comma
	// list): ZITADEL project ID and/or client IDs. A token needs one of them.
	Audiences []string
	// CAFile is a PEM bundle trusted for the issuer in addition to the
	// system roots (AUTH_OIDC_CA_FILE), e.g. a mounted trust-manager bundle.
	CAFile string
	// OrderRoles, when not empty, are the roles of which an orderer needs at
	// least one (AUTH_ORDER_ROLES, comma list); empty: any valid token.
	OrderRoles []string
	// RolesClaim is the claim holding the roles (AUTH_ROLES_CLAIM); default
	// ZitadelRolesClaim.
	RolesClaim string
	// TrustForwardedUser lets the web UI's order accept X-Forwarded-User /
	// X-Forwarded-Groups from sthings-portal (AUTH_TRUST_FORWARDED_USER).
	TrustForwardedUser bool
	// AdminRoles may order for any owner and any template, whatever its
	// orderRoles (AUTH_ADMIN_ROLES, comma list) (#122).
	AdminRoles []string
	// RoleGroups maps a role to the Backstage group it makes the orderer a
	// member of (AUTH_ROLE_GROUPS, "role=group:default/x" comma list);
	// nil: DefaultRoleGroups. See Groups.
	RoleGroups map[string]string
}

// Enabled reports whether orders need authentication.
func (c Config) Enabled() bool { return c.Issuer != "" }

// LoadConfig reads the configuration from the environment.
func LoadConfig() (Config, error) {
	trust, err := parseBool(os.Getenv(EnvTrustForwardedUser))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", EnvTrustForwardedUser, err)
	}
	roleGroups, err := ParseRoleGroups(os.Getenv(EnvRoleGroups))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", EnvRoleGroups, err)
	}
	return Config{
		AdminRoles:         SplitList(os.Getenv(EnvAdminRoles)),
		RoleGroups:         roleGroups,
		Issuer:             strings.TrimSpace(os.Getenv(EnvIssuer)),
		Audiences:          SplitList(os.Getenv(EnvAudience)),
		CAFile:             strings.TrimSpace(os.Getenv(EnvCAFile)),
		OrderRoles:         SplitList(os.Getenv(EnvOrderRoles)),
		RolesClaim:         strings.TrimSpace(os.Getenv(EnvRolesClaim)),
		TrustForwardedUser: trust,
	}, nil
}

func parseBool(v string) (bool, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return false, nil
	}
	switch strings.ToLower(v) {
	case "yes", "on":
		return true, nil
	case "no", "off":
		return false, nil
	}
	return strconv.ParseBool(v)
}

// SplitList splits a comma-separated list, dropping blanks.
func SplitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Errors of Authorize; the API turns them into status codes.
var (
	// ErrNoToken: no bearer token (401).
	ErrNoToken = errors.New("bearer token required")
	// ErrInvalidToken: the token failed verification (401).
	ErrInvalidToken = errors.New("invalid token")
	// ErrNoIdentity: a UI order without a token or a trusted forwarded user (401).
	ErrNoIdentity = errors.New("login required")
	// ErrForbidden: valid identity without one of the order roles (403).
	ErrForbidden = errors.New("missing order role")
	// ErrUnavailable: the issuer's discovery or keys could not be fetched (503).
	ErrUnavailable = errors.New("identity provider unavailable")
)

// Identity is who orders.
type Identity struct {
	// Subject is the token's "sub"; empty for a forwarded user.
	Subject string
	// Name is preferred_username or email (token, else the issuer's
	// userinfo), else sub; or X-Forwarded-User.
	Name  string
	Roles []string
	// Source is "token" or "forwarded".
	Source string
}

// Authenticator verifies orderers. Use New; a nil *Authenticator is never
// returned for an enabled config.
type Authenticator struct {
	cfg    Config
	client *http.Client
	// now is the clock of the token checks (tests set it).
	now func() time.Time

	mu          sync.Mutex
	verifier    *oidc.IDTokenVerifier
	userinfoURL string
	lastErr     error
	lastTry     time.Time

	// names caches the login names fetched from userinfo, by token hash,
	// until the token expires.
	namesMu sync.Mutex
	names   map[string]cachedName
}

type cachedName struct {
	name   string
	expiry time.Time
}

// New checks cfg and returns its Authenticator, or nil when auth is off.
// The issuer is only contacted on the first order (and Discover), so an
// unreachable identity provider does not keep the service from starting.
func New(cfg Config) (*Authenticator, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	if !strings.HasPrefix(cfg.Issuer, "https://") && !strings.HasPrefix(cfg.Issuer, "http://") {
		return nil, fmt.Errorf("%s must be an http(s) URL, got %q", EnvIssuer, cfg.Issuer)
	}
	if len(cfg.Audiences) == 0 {
		return nil, fmt.Errorf("%s is required when %s is set", EnvAudience, EnvIssuer)
	}
	if cfg.RolesClaim == "" {
		cfg.RolesClaim = ZitadelRolesClaim
	}
	client, err := httpClient(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	return &Authenticator{cfg: cfg, client: client, now: time.Now}, nil
}

// Config returns the effective configuration.
func (a *Authenticator) Config() Config { return a.cfg }

// httpClient trusts the system roots plus the PEM bundle in caFile.
func httpClient(caFile string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvCAFile, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no PEM certificate in %s", EnvCAFile, caFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}, nil
}

// Discover fetches the issuer's discovery document once; later calls reuse
// it. A failure is remembered for a few seconds, then retried.
func (a *Authenticator) Discover() (*oidc.IDTokenVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.verifier != nil {
		return a.verifier, nil
	}
	if a.lastErr != nil && time.Since(a.lastTry) < retryDiscovery {
		return nil, a.lastErr
	}
	a.lastTry = time.Now()
	// The provider keeps this context to fetch (and refresh) the JWKS, so it
	// must not be cancelled; the client's timeout bounds every request.
	ctx := oidc.ClientContext(context.Background(), a.client)
	provider, err := oidc.NewProvider(ctx, a.cfg.Issuer)
	if err != nil {
		a.lastErr = fmt.Errorf("%w: %v", ErrUnavailable, err)
		return nil, a.lastErr
	}
	a.verifier = provider.Verifier(&oidc.Config{
		// aud is checked against the list in verify; go-oidc takes only one.
		SkipClientIDCheck: true,
		Now:               a.now,
	})
	a.userinfoURL = provider.UserInfoEndpoint()
	a.lastErr = nil
	return a.verifier, nil
}

// BearerToken returns the token of an "Authorization: Bearer" header.
func BearerToken(r *http.Request) (string, bool) {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// Authorize checks the bearer token of an API order. The forwarded identity
// headers are never read here.
func (a *Authenticator) Authorize(r *http.Request) (Identity, error) {
	raw, ok := BearerToken(r)
	if !ok {
		return Identity{}, ErrNoToken
	}
	id, err := a.verify(r.Context(), raw)
	if err != nil {
		return Identity{}, err
	}
	return id, a.checkRoles(id)
}

// AuthorizeUI checks the web UI's order: a bearer token like the API when
// one is sent, otherwise the identity sthings-portal forwards, when trusted.
func (a *Authenticator) AuthorizeUI(r *http.Request) (Identity, error) {
	if _, ok := BearerToken(r); ok {
		return a.Authorize(r)
	}
	if !a.cfg.TrustForwardedUser {
		return Identity{}, ErrNoIdentity
	}
	name, ok := forwardedUser(r.Header.Get(HeaderForwardedUser))
	if !ok {
		return Identity{}, ErrNoIdentity
	}
	id := Identity{Name: name, Roles: SplitList(r.Header.Get(HeaderForwardedGroups)), Source: "forwarded"}
	return id, a.checkRoles(id)
}

// forwardedUser accepts a non-empty, bounded name without control characters.
func forwardedUser(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > maxForwardedUser {
		return "", false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return v, true
}

func (a *Authenticator) verify(ctx context.Context, raw string) (Identity, error) {
	verifier, err := a.Discover()
	if err != nil {
		return Identity{}, err
	}
	tok, err := verifier.Verify(oidc.ClientContext(ctx, a.client), raw)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if !intersects(tok.Audience, a.cfg.Audiences) {
		return Identity{}, fmt.Errorf("%w: audience %v not accepted", ErrInvalidToken, tok.Audience)
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	name := firstString(claims, "preferred_username", "email")
	if name == "" {
		if name, err = a.userinfoName(ctx, raw, tok.Subject, tok.Expiry); err != nil {
			return Identity{}, err
		}
	}
	if name == "" {
		name = tok.Subject
	}
	return Identity{
		Subject: tok.Subject,
		Name:    name,
		Roles:   a.roles(claims),
		Source:  "token",
	}, nil
}

// userinfoName asks the issuer's userinfo endpoint for the login name of a
// token that carries none. ZITADEL's JWT access tokens hold only sub (no
// preferred_username or email, unlike its ID tokens), and the name is what
// the order's author and the identity-bound parameters (user:default/<name>)
// are made of. The answer is cached per token until it expires. "" when the
// issuer has no userinfo endpoint or the answer names nobody.
func (a *Authenticator) userinfoName(ctx context.Context, raw, sub string, expiry time.Time) (string, error) {
	a.mu.Lock()
	endpoint := a.userinfoURL
	a.mu.Unlock()
	if endpoint == "" {
		return "", nil
	}
	sum := sha256.Sum256([]byte(raw))
	key := hex.EncodeToString(sum[:])
	if name, ok := a.cachedName(key); ok {
		return name, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("%w: userinfo: %v", ErrUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: userinfo: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// Signature and exp were fine, so the issuer revoked the token.
		return "", fmt.Errorf("%w: userinfo refused the token (%d)", ErrInvalidToken, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("%w: userinfo answered %d", ErrUnavailable, resp.StatusCode)
	}
	var info map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUserinfoBody)).Decode(&info); err != nil {
		return "", fmt.Errorf("%w: userinfo: %v", ErrUnavailable, err)
	}
	if got := firstString(info, "sub"); got != sub {
		return "", fmt.Errorf("%w: userinfo is about %q, the token about %q", ErrInvalidToken, got, sub)
	}
	name := firstString(info, "preferred_username", "email")
	a.cacheName(key, name, expiry)
	return name, nil
}

func (a *Authenticator) cachedName(key string) (string, bool) {
	a.namesMu.Lock()
	defer a.namesMu.Unlock()
	c, ok := a.names[key]
	if !ok || !a.now().Before(c.expiry) {
		return "", false
	}
	return c.name, true
}

func (a *Authenticator) cacheName(key, name string, expiry time.Time) {
	a.namesMu.Lock()
	defer a.namesMu.Unlock()
	now := a.now()
	if a.names == nil {
		a.names = map[string]cachedName{}
	}
	if len(a.names) >= maxCachedNames {
		for k, c := range a.names {
			if !now.Before(c.expiry) {
				delete(a.names, k)
			}
		}
		if len(a.names) >= maxCachedNames {
			a.names = map[string]cachedName{}
		}
	}
	a.names[key] = cachedName{name: name, expiry: expiry}
}

func (a *Authenticator) checkRoles(id Identity) error {
	if len(a.cfg.OrderRoles) == 0 || intersects(id.Roles, a.cfg.OrderRoles) {
		return nil
	}
	return fmt.Errorf("%w: %s has none of %s", ErrForbidden, id.Name, strings.Join(a.cfg.OrderRoles, ", "))
}

// roles reads the configured claim; for ZITADEL's default claim also the
// project-specific ones.
func (a *Authenticator) roles(claims map[string]any) []string {
	set := map[string]bool{}
	for _, r := range rolesFrom(claims[a.cfg.RolesClaim]) {
		set[r] = true
	}
	if a.cfg.RolesClaim == ZitadelRolesClaim {
		for k, v := range claims {
			if strings.HasPrefix(k, "urn:zitadel:iam:org:project:") && strings.HasSuffix(k, ":roles") {
				for _, r := range rolesFrom(v) {
					set[r] = true
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// rolesFrom reads a list of strings, a comma-separated string, or an object
// whose keys are the roles (ZITADEL).
func rolesFrom(v any) []string {
	switch t := v.(type) {
	case string:
		return SplitList(t)
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case map[string]any:
		out := make([]string, 0, len(t))
		for k := range t {
			out = append(out, k)
		}
		return out
	}
	return nil
}

func firstString(claims map[string]any, names ...string) string {
	for _, n := range names {
		if s, ok := claims[n].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func intersects(have, want []string) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}
