package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stuttgart-things/machinery-order-service/internal/auth/authtest"
)

const testAudience = "machinery-order-service"

func TestLoadConfig(t *testing.T) {
	t.Setenv(EnvIssuer, "")
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.False(t, cfg.Enabled())
	a, err := New(cfg)
	require.NoError(t, err)
	assert.Nil(t, a, "auth off without issuer")

	t.Setenv(EnvIssuer, " https://zitadel.example ")
	t.Setenv(EnvAudience, "123, client-a ,")
	t.Setenv(EnvOrderRoles, "portal-admin,claims-order")
	t.Setenv(EnvTrustForwardedUser, "true")
	cfg, err = LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "https://zitadel.example", cfg.Issuer)
	assert.Equal(t, []string{"123", "client-a"}, cfg.Audiences)
	assert.Equal(t, []string{"portal-admin", "claims-order"}, cfg.OrderRoles)
	assert.True(t, cfg.TrustForwardedUser)

	t.Setenv(EnvTrustForwardedUser, "maybe")
	_, err = LoadConfig()
	assert.Error(t, err)
}

func TestNewValidates(t *testing.T) {
	_, err := New(Config{Issuer: "https://zitadel.example"})
	assert.ErrorContains(t, err, EnvAudience)

	_, err = New(Config{Issuer: "zitadel.example", Audiences: []string{"a"}})
	assert.ErrorContains(t, err, EnvIssuer)

	_, err = New(Config{Issuer: "https://zitadel.example", Audiences: []string{"a"}, CAFile: "/does/not/exist"})
	assert.ErrorContains(t, err, EnvCAFile)

	notPEM := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(notPEM, []byte("not a certificate"), 0o600))
	_, err = New(Config{Issuer: "https://zitadel.example", Audiences: []string{"a"}, CAFile: notPEM})
	assert.ErrorContains(t, err, "no PEM certificate")

	a, err := New(Config{Issuer: "https://zitadel.example", Audiences: []string{"a"}})
	require.NoError(t, err)
	assert.Equal(t, ZitadelRolesClaim, a.Config().RolesClaim)
}

func bearer(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/claim-templates/vm/order", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func newTestAuth(t *testing.T, iss *authtest.Issuer, mod func(*Config)) *Authenticator {
	t.Helper()
	cfg := Config{Issuer: iss.URL(), Audiences: []string{testAudience}, CAFile: iss.CAFile}
	if mod != nil {
		mod(&cfg)
	}
	a, err := New(cfg)
	require.NoError(t, err)
	return a
}

func TestAuthorizeToken(t *testing.T) {
	iss := authtest.NewIssuer(t)
	a := newTestAuth(t, iss, nil)

	id, err := a.Authorize(bearer(iss.Sign(t, iss.Claims(testAudience))))
	require.NoError(t, err)
	assert.Equal(t, "patrick@sthings.example", id.Name)
	assert.Equal(t, "123456789", id.Subject)
	assert.Equal(t, "token", id.Source)

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	expired := iss.Claims(testAudience)
	expired["exp"] = time.Now().Add(-time.Hour).Unix()
	notYet := iss.Claims(testAudience)
	notYet["nbf"] = time.Now().Add(time.Hour).Unix()
	otherIssuer := iss.Claims(testAudience)
	otherIssuer["iss"] = "https://evil.example"

	for name, token := range map[string]string{
		"bad signature": authtest.SignWith(t, otherKey, iss.KeyID(), iss.Claims(testAudience)),
		"wrong aud":     iss.Sign(t, iss.Claims("other-project")),
		"expired":       iss.Sign(t, expired),
		"not yet valid": iss.Sign(t, notYet),
		"wrong issuer":  iss.Sign(t, otherIssuer),
		"garbage":       "not.a.jwt",
	} {
		_, err := a.Authorize(bearer(token))
		assert.ErrorIs(t, err, ErrInvalidToken, name)
	}

	_, err = a.Authorize(bearer(""))
	assert.ErrorIs(t, err, ErrNoToken)

	basic := bearer("")
	basic.Header.Set("Authorization", "Basic dXNlcjpwdw==")
	_, err = a.Authorize(basic)
	assert.ErrorIs(t, err, ErrNoToken)

	// The API never takes the portal's headers.
	fwd := bearer("")
	fwd.Header.Set(HeaderForwardedUser, "patrick")
	_, err = newTestAuth(t, iss, func(c *Config) { c.TrustForwardedUser = true }).Authorize(fwd)
	assert.ErrorIs(t, err, ErrNoToken)
}

func TestAuthorizeRoles(t *testing.T) {
	iss := authtest.NewIssuer(t)
	a := newTestAuth(t, iss, func(c *Config) { c.OrderRoles = []string{"portal-admin"} })

	_, err := a.Authorize(bearer(iss.Sign(t, iss.Claims(testAudience))))
	assert.ErrorIs(t, err, ErrForbidden, "no roles claim")

	viewer := iss.Claims(testAudience)
	viewer[ZitadelRolesClaim] = map[string]any{"portal-viewer": map[string]any{"1": "org"}}
	_, err = a.Authorize(bearer(iss.Sign(t, viewer)))
	assert.ErrorIs(t, err, ErrForbidden)

	admin := iss.Claims(testAudience)
	admin[ZitadelRolesClaim] = map[string]any{"portal-admin": map[string]any{"1": "org"}}
	id, err := a.Authorize(bearer(iss.Sign(t, admin)))
	require.NoError(t, err)
	assert.Equal(t, []string{"portal-admin"}, id.Roles)

	// ZITADEL's project-specific claim counts as well.
	project := iss.Claims(testAudience)
	project["urn:zitadel:iam:org:project:42:roles"] = map[string]any{"portal-admin": map[string]any{}}
	_, err = a.Authorize(bearer(iss.Sign(t, project)))
	require.NoError(t, err)

	// A custom claim as list.
	custom := newTestAuth(t, iss, func(c *Config) { c.OrderRoles = []string{"ops"}; c.RolesClaim = "groups" })
	groups := iss.Claims(testAudience)
	groups["groups"] = []string{"dev", "ops"}
	_, err = custom.Authorize(bearer(iss.Sign(t, groups)))
	require.NoError(t, err)
}

func TestAuthorizeUI(t *testing.T) {
	iss := authtest.NewIssuer(t)
	ui := func(user, groups string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/ui/templates/vm/render", nil)
		if user != "" {
			r.Header.Set(HeaderForwardedUser, user)
		}
		if groups != "" {
			r.Header.Set(HeaderForwardedGroups, groups)
		}
		return r
	}

	strict := newTestAuth(t, iss, nil)
	_, err := strict.AuthorizeUI(ui("patrick", ""))
	assert.ErrorIs(t, err, ErrNoIdentity, "forwarded user without trust")

	trusting := newTestAuth(t, iss, func(c *Config) {
		c.TrustForwardedUser = true
		c.OrderRoles = []string{"portal-admin"}
	})
	id, err := trusting.AuthorizeUI(ui("patrick", "portal-viewer, portal-admin"))
	require.NoError(t, err)
	assert.Equal(t, Identity{Name: "patrick", Roles: []string{"portal-viewer", "portal-admin"}, Source: "forwarded"}, id)

	_, err = trusting.AuthorizeUI(ui("patrick", "portal-viewer"))
	assert.ErrorIs(t, err, ErrForbidden)
	_, err = trusting.AuthorizeUI(ui("", ""))
	assert.ErrorIs(t, err, ErrNoIdentity)
	_, err = trusting.AuthorizeUI(ui("bad\nname", "portal-admin"))
	assert.ErrorIs(t, err, ErrNoIdentity)

	// A bearer token wins and is verified like the API's.
	r := ui("someone-else", "portal-admin")
	r.Header.Set("Authorization", "Bearer not.a.jwt")
	_, err = trusting.AuthorizeUI(r)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestIssuerUnavailable(t *testing.T) {
	iss := authtest.NewIssuer(t)

	// Without the CA file the issuer's certificate is not trusted.
	a := newTestAuth(t, iss, func(c *Config) { c.CAFile = "" })
	_, err := a.Authorize(bearer(iss.Sign(t, iss.Claims(testAudience))))
	assert.True(t, errors.Is(err, ErrUnavailable), "got %v", err)

	down := newTestAuth(t, iss, func(c *Config) { c.Issuer = "https://127.0.0.1:1" })
	_, err = down.Authorize(bearer("x.y.z"))
	assert.ErrorIs(t, err, ErrUnavailable)
}

// ZITADEL's JWT access tokens carry only sub; the name comes from userinfo
// (stuttgart-things#4380).
func TestAuthorizeUserinfoName(t *testing.T) {
	iss := authtest.NewIssuer(t)
	a := newTestAuth(t, iss, nil)

	bare := iss.Claims(testAudience)
	delete(bare, "preferred_username")
	token := iss.Sign(t, bare)

	iss.SetUserinfo(0, map[string]any{"sub": "123456789", "preferred_username": "patrick@sthings.example"})
	id, err := a.Authorize(bearer(token))
	require.NoError(t, err)
	assert.Equal(t, "patrick@sthings.example", id.Name)
	assert.Equal(t, "123456789", id.Subject)
	assert.Equal(t, 1, iss.UserinfoCalls())

	// Cached until the token expires.
	_, err = a.Authorize(bearer(token))
	require.NoError(t, err)
	assert.Equal(t, 1, iss.UserinfoCalls(), "second order of the same token from the cache")

	// A name in the token wins; userinfo is not asked.
	_, err = a.Authorize(bearer(iss.Sign(t, iss.Claims(testAudience))))
	require.NoError(t, err)
	assert.Equal(t, 1, iss.UserinfoCalls())

	// email when there is no preferred_username.
	mail := iss.Claims(testAudience)
	delete(mail, "preferred_username")
	mail["jti"] = "mail"
	iss.SetUserinfo(0, map[string]any{"sub": "123456789", "email": "patrick@example.com"})
	id, err = a.Authorize(bearer(iss.Sign(t, mail)))
	require.NoError(t, err)
	assert.Equal(t, "patrick@example.com", id.Name)

	// Userinfo without a name: sub, as before.
	anon := iss.Claims(testAudience)
	delete(anon, "preferred_username")
	anon["jti"] = "anon"
	iss.SetUserinfo(0, map[string]any{"sub": "123456789"})
	id, err = a.Authorize(bearer(iss.Sign(t, anon)))
	require.NoError(t, err)
	assert.Equal(t, "123456789", id.Name)

	// Userinfo about someone else: refused.
	other := iss.Claims(testAudience)
	delete(other, "preferred_username")
	other["jti"] = "other"
	iss.SetUserinfo(0, map[string]any{"sub": "987654321", "preferred_username": "biff"})
	_, err = a.Authorize(bearer(iss.Sign(t, other)))
	assert.ErrorIs(t, err, ErrInvalidToken)

	// Revoked token: 401 from userinfo -> invalid token; issuer error -> 503.
	revoked := iss.Claims(testAudience)
	delete(revoked, "preferred_username")
	revoked["jti"] = "revoked"
	iss.SetUserinfo(http.StatusUnauthorized, nil)
	_, err = a.Authorize(bearer(iss.Sign(t, revoked)))
	assert.ErrorIs(t, err, ErrInvalidToken)

	broken := iss.Claims(testAudience)
	delete(broken, "preferred_username")
	broken["jti"] = "broken"
	iss.SetUserinfo(http.StatusBadGateway, nil)
	_, err = a.Authorize(bearer(iss.Sign(t, broken)))
	assert.ErrorIs(t, err, ErrUnavailable)
}

func TestNameCacheExpires(t *testing.T) {
	iss := authtest.NewIssuer(t)
	a := newTestAuth(t, iss, nil)
	now := time.Now()
	a.now = func() time.Time { return now }

	a.cacheName("k", "patrick", now.Add(time.Minute))
	name, ok := a.cachedName("k")
	assert.True(t, ok)
	assert.Equal(t, "patrick", name)

	now = now.Add(2 * time.Minute)
	_, ok = a.cachedName("k")
	assert.False(t, ok, "expired")

	// Full: expired entries go first, then everything.
	for i := range maxCachedNames {
		a.cacheName(string(rune(i)), "x", now.Add(time.Minute))
	}
	a.cacheName("new", "y", now.Add(time.Minute))
	_, ok = a.cachedName("new")
	assert.True(t, ok)
	assert.LessOrEqual(t, len(a.names), maxCachedNames)
}
