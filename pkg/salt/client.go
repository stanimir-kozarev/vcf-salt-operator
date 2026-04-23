package salt

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// Client is the interface for Salt key management operations.
// Implemented by RaaSClient; can be mocked in operator tests.
type Client interface {
	Login(ctx context.Context) error
	ListPendingKeys(ctx context.Context) ([]string, error)
	ListAcceptedKeys(ctx context.Context) ([]string, error)
	AcceptKey(ctx context.Context, minionID string) error
	DeleteKey(ctx context.Context, minionID string) error
}

// RaaSClient implements Client against the VCF Salt RaaS HTTP API.
type RaaSClient struct {
	baseURL    string
	username   string
	password   string
	masterID   string
	httpClient *http.Client
	log        logr.Logger
	mu         sync.RWMutex
	jwt        string
}

// NewRaaSClient creates a new RaaS client.
// baseURL is the root of the VCF Salt RaaS API (e.g. "https://aria-config.corp:443").
// masterID is the Salt master identifier (e.g. "saltstack_enterprise_installer").
// Set skipTLSVerify=true for environments with self-signed certificates.
// log is used for V(1) debug messages (API payloads/responses); pass logr.Discard() to suppress.
func NewRaaSClient(baseURL, username, password, masterID string, skipTLSVerify bool, log logr.Logger) *RaaSClient {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: skipTLSVerify}, //nolint:gosec
	}
	jar, _ := cookiejar.New(nil)
	return &RaaSClient{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		masterID: masterID,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
			Jar:       jar,
		},
		log: log,
	}
}

// Login authenticates against the VCF Salt RaaS API.
// Step 1: GET /account/login to obtain the Tornado _xsrf cookie.
// Step 2: POST /account/login with JSON credentials to obtain a JWT.
// All subsequent /rpc calls use the JWT as a Bearer token.
func (c *RaaSClient) Login(ctx context.Context) error {
	c.log.V(1).Info("Fetching _xsrf cookie", "url", c.baseURL+"/account/login")

	c.fetchCookies(ctx, "/account/login")

	xsrf := c.xsrfToken()
	if xsrf == "" {
		return fmt.Errorf("login failed: no _xsrf cookie returned by %s/account/login (check URL and TLS)", c.baseURL)
	}

	loginPayload := map[string]string{
		"username":    c.username,
		"password":    c.password,
		"config_name": "internal",
		"token_type":  "jwt",
	}
	encoded, err := json.Marshal(loginPayload)
	if err != nil {
		return fmt.Errorf("login: encode payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/account/login", bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("login: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Xsrftoken", xsrf)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	c.log.V(1).Info("POST /account/login", "payload", string(encoded))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("login: POST /account/login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("login: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("login failed: HTTP %d (check username/password): %s", resp.StatusCode, string(respBody))
	}

	var loginResp map[string]any
	if err := json.Unmarshal(respBody, &loginResp); err != nil {
		return fmt.Errorf("login: parse response: %w", err)
	}
	jwt, ok := loginResp["jwt"].(string)
	if !ok || jwt == "" {
		return fmt.Errorf("login failed: no JWT in response: %s", string(respBody))
	}
	c.mu.Lock()
	c.jwt = jwt
	c.mu.Unlock()
	c.log.V(1).Info("Login successful")
	return nil
}

// ListPendingKeys returns the list of minion IDs with pending (unaccepted) Salt keys.
func (c *RaaSClient) ListPendingKeys(ctx context.Context) ([]string, error) {
	respBody, statusCode, err := c.rawPost(ctx, map[string]any{
		"resource": "minions",
		"method":   "get_minion_key_state",
		"kwarg":    map[string]any{"limit": 0, "page": 0, "key_state": "pending"},
	})
	if err != nil {
		return nil, fmt.Errorf("list_pending: %w", err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, fmt.Errorf("list_pending: HTTP %d: %s", statusCode, string(respBody))
	}
	c.log.V(1).Info("list_pending response", "body", string(respBody))
	return extractMinions(respBody)
}

// ListAcceptedKeys returns the list of minion IDs with accepted (live) Salt keys.
func (c *RaaSClient) ListAcceptedKeys(ctx context.Context) ([]string, error) {
	respBody, statusCode, err := c.rawPost(ctx, map[string]any{
		"resource": "minions",
		"method":   "get_minion_key_state",
		"kwarg":    map[string]any{"limit": 0, "page": 0, "key_state": "accepted"},
	})
	if err != nil {
		return nil, fmt.Errorf("list_accepted: %w", err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, fmt.Errorf("list_accepted: HTTP %d: %s", statusCode, string(respBody))
	}
	c.log.V(1).Info("list_accepted response", "body", string(respBody))
	return extractMinions(respBody)
}

// AcceptKey accepts a pending Salt minion key by minion ID.
func (c *RaaSClient) AcceptKey(ctx context.Context, minionID string) error {
	respBody, statusCode, err := c.rawPost(ctx, map[string]any{
		"resource": "minions",
		"method":   "set_minion_key_state",
		"kwarg": map[string]any{
			"minions":          []any{[]any{c.masterID, minionID}},
			"state":            "accept",
			"include_rejected": true,
			"include_denied":   true,
		},
	})
	if err != nil {
		return fmt.Errorf("accept key for %q: %w", minionID, err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return fmt.Errorf("accept key for %q: HTTP %d: %s", minionID, statusCode, string(respBody))
	}
	c.log.V(1).Info("accept response", "minionID", minionID, "body", string(respBody))
	return nil
}

// DeleteKey deletes (revokes) a Salt minion key by minion ID.
// This is intentionally best-effort: errors are returned but callers should treat them as non-fatal.
func (c *RaaSClient) DeleteKey(ctx context.Context, minionID string) error {
	respBody, statusCode, err := c.rawPost(ctx, map[string]any{
		"resource": "minions",
		"method":   "set_minion_key_state",
		"kwarg": map[string]any{
			"minions": []any{[]any{c.masterID, minionID}},
			"state":   "delete",
		},
	})
	if err != nil {
		return fmt.Errorf("delete key for %q: %w", minionID, err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return fmt.Errorf("delete key for %q: HTTP %d: %s", minionID, statusCode, string(respBody))
	}
	c.log.V(1).Info("delete response", "minionID", minionID, "body", string(respBody))
	return nil
}

// xsrfToken returns the current _xsrf cookie value from the jar, or empty string.
func (c *RaaSClient) xsrfToken() string {
	if c.httpClient.Jar == nil {
		return ""
	}
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return ""
	}
	for _, cookie := range c.httpClient.Jar.Cookies(u) {
		if cookie.Name == "_xsrf" {
			return cookie.Value
		}
	}
	return ""
}

// fetchCookies does a GET to the given path to prime the cookie jar.
// This is required for Tornado-based backends that enforce XSRF protection:
// the server sets a _xsrf cookie on GET which must be echoed back as
// X-Xsrftoken on every subsequent POST.
func (c *RaaSClient) fetchCookies(ctx context.Context, path string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
}

// rawPost sends a JSON POST to /rpc, retrying once with a fresh login on HTTP 401 (JWT expiry).
func (c *RaaSClient) rawPost(ctx context.Context, fields map[string]any) ([]byte, int, error) {
	body, status, err := c.doPost(ctx, fields)
	if err != nil || status != http.StatusUnauthorized {
		return body, status, err
	}
	// JWT likely expired — re-login and retry once.
	if loginErr := c.Login(ctx); loginErr != nil {
		return nil, status, fmt.Errorf("JWT expired; re-login failed: %w", loginErr)
	}
	return c.doPost(ctx, fields)
}

// doPost sends a single authenticated JSON POST to /rpc without retry logic.
func (c *RaaSClient) doPost(ctx context.Context, fields map[string]any) ([]byte, int, error) {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, 0, fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/rpc", bytes.NewReader(encoded))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if xsrf := c.xsrfToken(); xsrf != "" {
		req.Header.Set("X-Xsrftoken", xsrf)
	}
	c.mu.RLock()
	jwtVal := c.jwt
	c.mu.RUnlock()
	if jwtVal != "" {
		req.Header.Set("Authorization", "Bearer "+jwtVal)
	}
	c.log.V(1).Info("POST /rpc", "payload", string(encoded))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("POST /rpc: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}
	return respBody, resp.StatusCode, nil
}

// extractMinions parses a minions.get_minion_key_state response and returns the minion IDs.
// Response structure: {"ret": {"count": N, "results": [{"minion": "id", ...}]}}
func extractMinions(body []byte) ([]string, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("response is not valid JSON: %w", err)
	}

	// Surface API-level errors
	if errObj, ok := raw["error"]; ok && errObj != nil {
		if m, ok := errObj.(map[string]any); ok {
			if msg, ok := m["message"].(string); ok && msg != "" {
				return nil, fmt.Errorf("RaaS API error: %s", msg)
			}
		}
		return nil, fmt.Errorf("RaaS API error: %v", errObj)
	}

	ret, ok := raw["ret"].(map[string]any)
	if !ok {
		return nil, nil
	}
	results, ok := ret["results"].([]any)
	if !ok {
		return nil, nil
	}

	out := make([]string, 0, len(results))
	for _, item := range results {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if minion, ok := m["minion"].(string); ok && minion != "" {
			out = append(out, minion)
		}
	}
	return out, nil
}
