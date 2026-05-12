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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// JIDResult holds the result of a Salt job execution for a single minion.
// Return is kept as raw JSON because the shape differs per function:
// test.ping / refresh_pillar return a bool; state.highstate returns a state map.
type JIDResult struct {
	MinionID string
	JID      string
	Fun      string
	Return   json.RawMessage
	Retcode  int
	Success  bool
}

// BoolReturn extracts the return value as a boolean (for test.ping, refresh_pillar).
// Returns (false, false) if the return value is not a boolean.
func (r *JIDResult) BoolReturn() (bool, bool) {
	var b bool
	if err := json.Unmarshal(r.Return, &b); err != nil {
		return false, false
	}
	return b, true
}

// HighstateOK returns true when state.highstate completed with no failed states.
func (r *JIDResult) HighstateOK() bool {
	return r.Success && r.Retcode == 0
}

// FailedStateIDs returns a sorted list of state IDs whose result was false in a
// state.highstate return map. Returns nil if the return is not a state map or all
// states succeeded. Only the map keys (state IDs) are inspected — no values are read.
func (r *JIDResult) FailedStateIDs() []string {
	var states map[string]struct {
		Result bool `json:"result"`
	}
	if err := json.Unmarshal(r.Return, &states); err != nil {
		return nil
	}
	var failed []string
	for id, s := range states {
		if !s.Result {
			failed = append(failed, id)
		}
	}
	sort.Strings(failed)
	return failed
}

// Client is the interface for Salt key management operations.
// Implemented by RaaSClient; can be mocked in operator tests.
type Client interface {
	Login(ctx context.Context) error
	ListPendingKeys(ctx context.Context) ([]string, error)
	ListAcceptedKeys(ctx context.Context) ([]string, error)
	AcceptKey(ctx context.Context, minionID string) error
	DeleteKey(ctx context.Context, minionID string) error
	// TestPing dispatches test.ping and waits for the result (context controls timeout).
	TestPing(ctx context.Context, minionID string) (bool, error)
	// RefreshPillar dispatches saltutil.refresh_pillar and waits for the result.
	RefreshPillar(ctx context.Context, minionID string) (bool, error)
	// DispatchHighstate dispatches state.highstate asynchronously and returns the JID.
	// The caller should store the JID and poll with PollJID.
	DispatchHighstate(ctx context.Context, minionID string) (string, error)
	// PollJID checks whether a dispatched job has returned a result for the given minion.
	// Returns (result, true, nil) when done; (nil, false, nil) when still running.
	PollJID(ctx context.Context, minionID, jid string) (*JIDResult, bool, error)
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
	// The RaaS API returns HTTP 200 even when the operation fails; check the body.
	if err := checkBodyError(respBody); err != nil {
		return fmt.Errorf("accept key for %q: %w", minionID, err)
	}
	return nil
}

// Call sends a raw /rpc call and returns the unparsed response body.
// Intended for CLI exploration tools; not used by the operator itself.
func (c *RaaSClient) Call(ctx context.Context, resource, method string, kwarg map[string]any) ([]byte, error) {
	body, status, err := c.rawPost(ctx, map[string]any{
		"resource": resource,
		"method":   method,
		"kwarg":    kwarg,
	})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", status, body)
	}
	return body, nil
}

// TestPing dispatches test.ping to minionID and waits for the boolean result.
// The context deadline controls the maximum wait time.
func (c *RaaSClient) TestPing(ctx context.Context, minionID string) (bool, error) {
	jid, err := c.dispatch(ctx, minionID, "test.ping", nil)
	if err != nil {
		return false, err
	}
	return c.waitBoolResult(ctx, minionID, jid, "test.ping")
}

// RefreshPillar dispatches saltutil.refresh_pillar to minionID and waits for the result.
func (c *RaaSClient) RefreshPillar(ctx context.Context, minionID string) (bool, error) {
	jid, err := c.dispatch(ctx, minionID, "saltutil.refresh_pillar", nil)
	if err != nil {
		return false, err
	}
	return c.waitBoolResult(ctx, minionID, jid, "saltutil.refresh_pillar")
}

// DispatchHighstate dispatches state.highstate asynchronously and returns the JID.
// Store the JID in an annotation and poll with PollJID on subsequent reconciles.
func (c *RaaSClient) DispatchHighstate(ctx context.Context, minionID string) (string, error) {
	return c.dispatch(ctx, minionID, "state.highstate", nil)
}

// PollJID checks whether a dispatched job has completed for the given minion.
// Returns (result, true, nil) when done; (nil, false, nil) when still running.
func (c *RaaSClient) PollJID(ctx context.Context, minionID, jid string) (*JIDResult, bool, error) {
	body, status, err := c.rawPost(ctx, map[string]any{
		"resource": "ret",
		"method":   "get_jid",
		"kwarg":    map[string]any{"jid": jid},
	})
	if err != nil {
		return nil, false, fmt.Errorf("poll_jid(%s): %w", jid, err)
	}
	if status < 200 || status >= 300 {
		return nil, false, fmt.Errorf("poll_jid(%s): HTTP %d: %s", jid, status, body)
	}
	c.log.V(1).Info("poll_jid response", "jid", jid, "minionID", minionID, "body", string(body))

	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, false, fmt.Errorf("poll_jid(%s): parse response: %w", jid, err)
	}
	if errObj, ok := resp["error"]; ok && errObj != nil {
		if m, ok := errObj.(map[string]any); ok {
			if msg, ok := m["message"].(string); ok && msg != "" {
				return nil, false, fmt.Errorf("poll_jid(%s): API error: %s", jid, msg)
			}
		}
		return nil, false, fmt.Errorf("poll_jid(%s): API error: %v", jid, errObj)
	}
	ret, ok := resp["ret"].(map[string]any)
	if !ok || len(ret) == 0 {
		return nil, false, nil // not yet complete
	}
	minionData, ok := ret[minionID]
	if !ok {
		return nil, false, nil // this minion's result not yet available
	}
	minionMap, ok := minionData.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("poll_jid(%s): unexpected result shape for %s", jid, minionID)
	}
	returnBytes, _ := json.Marshal(minionMap["return"])
	retcode := 0
	if rc, ok := minionMap["retcode"].(float64); ok {
		retcode = int(rc)
	}
	success, _ := minionMap["success"].(bool)
	fun, _ := minionMap["fun"].(string)

	return &JIDResult{
		MinionID: minionID,
		JID:      jid,
		Fun:      fun,
		Return:   json.RawMessage(returnBytes),
		Retcode:  retcode,
		Success:  success,
	}, true, nil
}

// dispatch saves a job template then routes it to a specific minion, returning the JID.
func (c *RaaSClient) dispatch(ctx context.Context, minionID, fun string, kwarg map[string]any) (string, error) {
	if kwarg == nil {
		kwarg = map[string]any{}
	}
	name := "op-" + strings.NewReplacer(".", "-", "_", "-").Replace(fun)
	jobUUID, err := c.saveJob(ctx, name, fun, kwarg)
	if err != nil {
		return "", err
	}
	c.log.V(1).Info("saved job", "fun", fun, "jobUUID", jobUUID)
	jid, err := c.routeCmd(ctx, jobUUID, minionID)
	if err != nil {
		return "", err
	}
	c.log.V(1).Info("dispatched job", "fun", fun, "minionID", minionID, "jid", jid)
	return jid, nil
}

// saveJob creates a job template in RaaS and returns its UUID.
// NOTE: masters must NOT be set for cmd=local (only valid for runner/wheel).
func (c *RaaSClient) saveJob(ctx context.Context, name, fun string, kwarg map[string]any) (string, error) {
	body, status, err := c.rawPost(ctx, map[string]any{
		"resource": "job",
		"method":   "save_job",
		"kwarg": map[string]any{
			"name": name,
			"cmd":  "local",
			"fun":  fun,
			"arg":  map[string]any{"arg": []any{}, "kwarg": kwarg},
		},
	})
	if err != nil {
		return "", fmt.Errorf("save_job(%s): %w", fun, err)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("save_job(%s): HTTP %d: %s", fun, status, body)
	}
	var resp struct {
		Ret   string         `json:"ret"`
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("save_job(%s): parse response: %w", fun, err)
	}
	if resp.Error != nil {
		if msg, ok := resp.Error["message"].(string); ok && msg != "" {
			return "", fmt.Errorf("save_job(%s): API error: %s", fun, msg)
		}
		return "", fmt.Errorf("save_job(%s): API error: %v", fun, resp.Error)
	}
	if resp.Ret == "" {
		return "", fmt.Errorf("save_job(%s): empty job UUID in response: %s", fun, body)
	}
	return resp.Ret, nil
}

// routeCmd dispatches a saved job to a specific minion and returns the JID.
func (c *RaaSClient) routeCmd(ctx context.Context, jobUUID, minionID string) (string, error) {
	body, status, err := c.rawPost(ctx, map[string]any{
		"resource": "cmd",
		"method":   "route_cmd",
		"kwarg": map[string]any{
			"job_uuid": jobUUID,
			"tgt": map[string]any{
				"salt": map[string]any{
					"tgt":      "L@" + minionID,
					"tgt_type": "compound",
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("route_cmd(%s): %w", minionID, err)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("route_cmd(%s): HTTP %d: %s", minionID, status, body)
	}
	var resp struct {
		Ret   string         `json:"ret"`
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("route_cmd(%s): parse response: %w", minionID, err)
	}
	if resp.Error != nil {
		if msg, ok := resp.Error["message"].(string); ok && msg != "" {
			return "", fmt.Errorf("route_cmd(%s): API error: %s", minionID, msg)
		}
		return "", fmt.Errorf("route_cmd(%s): API error: %v", minionID, resp.Error)
	}
	if resp.Ret == "" {
		return "", fmt.Errorf("route_cmd(%s): empty JID in response: %s", minionID, body)
	}
	return resp.Ret, nil
}

// waitBoolResult polls PollJID every 5s until the boolean result is available or ctx expires.
func (c *RaaSClient) waitBoolResult(ctx context.Context, minionID, jid, fun string) (bool, error) {
	for {
		result, done, err := c.PollJID(ctx, minionID, jid)
		if err != nil {
			return false, err
		}
		if done {
			b, ok := result.BoolReturn()
			if !ok {
				return false, fmt.Errorf("%s: unexpected non-boolean return for %s (raw: %s)", fun, minionID, result.Return)
			}
			return b, nil
		}
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("%s: timed out waiting for result (jid=%s): %w", fun, jid, ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
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
	// The RaaS API returns HTTP 200 even when the operation fails; check the body.
	if err := checkBodyError(respBody); err != nil {
		return fmt.Errorf("delete key for %q: %w", minionID, err)
	}
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

// checkBodyError inspects a RaaS /rpc response body for {"error": {...}} payloads.
// The RaaS API returns HTTP 200 for some failure conditions, so HTTP status alone is
// insufficient for set_minion_key_state (accept/delete) calls.
func checkBodyError(body []byte) error {
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil // non-JSON body; HTTP status check already handled it
	}
	errObj, ok := resp["error"]
	if !ok || errObj == nil {
		return nil
	}
	if m, ok := errObj.(map[string]any); ok {
		if msg, ok := m["message"].(string); ok && msg != "" {
			return fmt.Errorf("API error: %s", msg)
		}
	}
	return fmt.Errorf("API error: %v", errObj)
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
