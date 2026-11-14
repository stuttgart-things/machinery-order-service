// Package render orders a ClaimTemplate from claim-machinery-api
// (POST /api/v1/claim-templates/{name}/order) and returns the rendered YAML.
//
// The orderer's own bearer token is forwarded when there is one, so
// claim-machinery-api binds requested_by/owner and checks orderRoles against
// the real login (claim-machinery-api#122), exactly as for the claims CLI.
// Without one (an Order created with kubectl) the service's own token is
// used: a static CLAIM_API_TOKEN or OAuth2 client credentials.
package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// Environment variables of the service's own claim-machinery-api identity.
const (
	EnvURL          = "CLAIM_API_URL"
	EnvToken        = "CLAIM_API_TOKEN"
	EnvClientID     = "CLAIM_API_OIDC_CLIENT_ID"
	EnvClientSecret = "CLAIM_API_OIDC_CLIENT_SECRET"
	EnvTokenURL     = "CLAIM_API_OIDC_TOKEN_URL"
	EnvScope        = "CLAIM_API_OIDC_SCOPE"
)

// TokenSource returns the bearer token of the service's own identity; ""
// means none.
type TokenSource func(ctx context.Context) (string, error)

// Client talks to claim-machinery-api.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Service is the service's own token, used when the caller has none.
	Service TokenSource
}

// FromEnv builds a client from CLAIM_API_*; an error when CLAIM_API_URL is
// unset or the OIDC settings are incomplete.
func FromEnv() (*Client, error) {
	base := strings.TrimRight(os.Getenv(EnvURL), "/")
	if base == "" {
		return nil, fmt.Errorf("%s is required", EnvURL)
	}
	c := &Client{BaseURL: base, HTTP: &http.Client{Timeout: 60 * time.Second}}
	if tok := os.Getenv(EnvToken); tok != "" {
		c.Service = func(context.Context) (string, error) { return tok, nil }
		return c, nil
	}
	id, secret, tokenURL := os.Getenv(EnvClientID), os.Getenv(EnvClientSecret), os.Getenv(EnvTokenURL)
	if id == "" && secret == "" && tokenURL == "" {
		return c, nil
	}
	if id == "" || secret == "" || tokenURL == "" {
		return nil, fmt.Errorf("%s, %s and %s go together", EnvClientID, EnvClientSecret, EnvTokenURL)
	}
	cc := &clientcredentials.Config{ClientID: id, ClientSecret: secret, TokenURL: tokenURL, Scopes: strings.Fields(os.Getenv(EnvScope))}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: 15 * time.Second})
	src := cc.TokenSource(ctx) // reuses the token until shortly before it expires
	c.Service = func(context.Context) (string, error) {
		t, err := src.Token()
		if err != nil {
			return "", fmt.Errorf("claim-machinery-api token: %w", err)
		}
		return t.AccessToken, nil
	}
	return c, nil
}

// APIError is a non-2xx answer of claim-machinery-api.
type APIError struct {
	Status  int
	Message string
	// Fields name what is wrong per parameter (a 400 "invalid parameters").
	Fields map[string]string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("claim-machinery-api answered %d: %s", e.Status, e.Message)
	if len(e.Fields) > 0 {
		keys := make([]string, 0, len(e.Fields))
		for k := range e.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+": "+e.Fields[k])
		}
		msg += " (" + strings.Join(parts, "; ") + ")"
	}
	return msg
}

// IsCallerError reports whether err is the orderer's fault (400, 401, 403,
// 404): retrying does not help.
func IsCallerError(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.Status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

// Render orders template with params. callerToken is the orderer's bearer
// token; "" uses the service's own.
func (c *Client) Render(ctx context.Context, template string, params map[string]any, callerToken string) (string, error) {
	body, err := json.Marshal(map[string]any{"parameters": params})
	if err != nil {
		return "", err
	}
	u := c.BaseURL + "/api/v1/claim-templates/" + url.PathEscape(template) + "/order"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	token := callerToken
	if token == "" && c.Service != nil {
		if token, err = c.Service(ctx); err != nil {
			return "", err
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("claim-machinery-api: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("claim-machinery-api: reading the answer: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error  string            `json:"error"`
			Fields map[string]string `json:"fields"`
		}
		ae := &APIError{Status: resp.StatusCode}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			ae.Message, ae.Fields = e.Error, e.Fields
		} else {
			ae.Message = strings.TrimSpace(string(raw))
			if len(ae.Message) > 300 {
				ae.Message = ae.Message[:300] + "…"
			}
		}
		return "", ae
	}
	var out struct {
		Rendered string `json:"rendered"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("claim-machinery-api: decoding the answer: %w", err)
	}
	if strings.TrimSpace(out.Rendered) == "" {
		return "", errors.New("claim-machinery-api rendered nothing")
	}
	return out.Rendered, nil
}

// Template is the part of a ClaimTemplate the service needs.
type Template struct {
	Name string
	// Type is spec.type, e.g. xr-native-vm.
	Type string
	// Defaults are the parameters' defaults.
	Defaults map[string]any
}

// Template reads a ClaimTemplate (GET /api/v1/claim-templates/{name}).
func (c *Client) Template(ctx context.Context, name, callerToken string) (*Template, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/claim-templates/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	token := callerToken
	if token == "" && c.Service != nil {
		if token, err = c.Service(ctx); err != nil {
			return nil, err
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claim-machinery-api: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
	}
	var t struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Type       string `json:"type"`
			Parameters []struct {
				Name    string `json:"name"`
				Default any    `json:"default"`
			} `json:"parameters"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&t); err != nil {
		return nil, fmt.Errorf("claim-machinery-api: decoding template %s: %w", name, err)
	}
	out := &Template{Name: t.Metadata.Name, Type: t.Spec.Type, Defaults: map[string]any{}}
	for _, p := range t.Spec.Parameters {
		if p.Default != nil {
			out.Defaults[p.Name] = p.Default
		}
	}
	return out, nil
}
