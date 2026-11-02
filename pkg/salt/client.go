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

type JIDResult struct {
	MinionID string
	JID      string
	Fun      string
	Return   json.RawMessage
	Retcode  int
	Success  bool
}

func (r *JIDResult) BoolReturn() (bool, bool) {
	var b bool
	if err := json.Unmarshal(r.Return, &b); err != nil {
		return false, false
	}
	return b, true
}

func (r *JIDResult) HighstateOK() bool {
	return r.Success && r.Retcode == 0
}

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

type Client interface {
	Login(ctx context.Context) error
	ListPendingKeys(ctx context.Context) ([]string, error)
	ListAcceptedKeys(ctx context.Context) ([]string, error)
	AcceptKey(ctx context.Context, minionID string) error
	DeleteKey(ctx context.Context, minionID string) error
	TestPing(ctx context.Context, minionID string) (bool, error)
	RefreshPillar(ctx context.Context, minionID string) (bool, error)
	DispatchHighstate(ctx context.Context, minionID string) (string, error)
	PollJID(ctx context.Context, minionID, jid string) (*JIDResult, bool, error)
	MasterLastSeen(ctx context.Context) (string, error)
	KeyStates(ctx context.Context, minionID string) ([]string, error)
	GenAcceptKey(ctx context.Context, minionID string, force bool) (*KeyPair, error)
	MasterPublicKey(ctx context.Context) (string, error)
}

type jobArg struct {
	Arg   []any          `json:"arg"`
	Kwarg map[string]any `json:"kwarg"`
}

type KeyPair struct {
	Pub  string `json:"pub"`
	Priv string `json:"priv"`
}

type RaaSClient struct {
	baseURL    string
	username   string
	password   string
	masterID   string
	httpClient *http.Client
	log        logr.Logger
	mu         sync.RWMutex
	jwt        string
	masterPub  string
}

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

func (c *RaaSClient) Login(ctx context.Context) error {
	c.log.V(1).Info("Fetching _xsrf cookie", "url", c.baseURL+"/account/login")

	status, err := c.fetchCookies(ctx, "/account/login")
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	xsrf := c.xsrfToken()
	if xsrf == "" {
		return fmt.Errorf("login failed: no _xsrf cookie returned by %s/account/login "+
			"(HTTP %d, check that the URL is the RaaS API)", c.baseURL, status)
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

	c.log.V(1).Info("POST /account/login", "username", c.username)

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

func (c *RaaSClient) ListPendingKeys(ctx context.Context) ([]string, error) {
	respBody, statusCode, err := c.rawPost(ctx, rpc("minions", "get_minion_key_state",
		map[string]any{"limit": 0, "page": 0, "key_state": "pending"}))
	if err != nil {
		return nil, fmt.Errorf("list_pending: %w", err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, fmt.Errorf("list_pending: HTTP %d: %s", statusCode, string(respBody))
	}
	c.log.V(1).Info("list_pending response", "body", string(respBody))
	return extractMinions(respBody)
}

func (c *RaaSClient) ListAcceptedKeys(ctx context.Context) ([]string, error) {
	respBody, statusCode, err := c.rawPost(ctx, rpc("minions", "get_minion_key_state",
		map[string]any{"limit": 0, "page": 0, "key_state": "accepted"}))
	if err != nil {
		return nil, fmt.Errorf("list_accepted: %w", err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, fmt.Errorf("list_accepted: HTTP %d: %s", statusCode, string(respBody))
	}
	c.log.V(1).Info("list_accepted response", "body", string(respBody))
	return extractMinions(respBody)
}

func (c *RaaSClient) AcceptKey(ctx context.Context, minionID string) error {
	respBody, statusCode, err := c.rawPost(ctx, rpc("minions", "set_minion_key_state", map[string]any{
		"minions":          []any{[]any{c.masterID, minionID}},
		"state":            "accept",
		"include_rejected": true,
		"include_denied":   true,
	}))
	if err != nil {
		return fmt.Errorf("accept key for %q: %w", minionID, err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return fmt.Errorf("accept key for %q: HTTP %d: %s", minionID, statusCode, string(respBody))
	}
	c.log.V(1).Info("accept response", "minionID", minionID, "body", string(respBody))
	if err := checkBodyError(respBody); err != nil {
		return fmt.Errorf("accept key for %q: %w", minionID, err)
	}
	return nil
}

func rpc(resource, method string, kwarg map[string]any) map[string]any {
	return map[string]any{"resource": resource, "method": method, "kwarg": kwarg}
}

func (c *RaaSClient) Call(ctx context.Context, resource, method string, kwarg map[string]any) ([]byte, error) {
	body, status, err := c.rawPost(ctx, rpc(resource, method, kwarg))
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", status, body)
	}
	return body, nil
}

func (c *RaaSClient) TestPing(ctx context.Context, minionID string) (bool, error) {
	jid, err := c.dispatch(ctx, minionID, "test.ping", nil)
	if err != nil {
		return false, err
	}
	return c.waitBoolResult(ctx, minionID, jid, "test.ping")
}

func (c *RaaSClient) RefreshPillar(ctx context.Context, minionID string) (bool, error) {
	jid, err := c.dispatch(ctx, minionID, "saltutil.refresh_pillar", nil)
	if err != nil {
		return false, err
	}
	return c.waitBoolResult(ctx, minionID, jid, "saltutil.refresh_pillar")
}

func (c *RaaSClient) DispatchHighstate(ctx context.Context, minionID string) (string, error) {
	return c.dispatch(ctx, minionID, "state.highstate", nil)
}

func (c *RaaSClient) PollJID(ctx context.Context, minionID, jid string) (*JIDResult, bool, error) {
	body, status, err := c.rawPost(ctx, rpc("ret", "get_jid", map[string]any{"jid": jid}))
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
		return nil, false, nil
	}
	minionData, ok := ret[minionID]
	if !ok {
		return nil, false, nil
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

func (c *RaaSClient) saveJob(ctx context.Context, name, fun string, kwarg map[string]any) (string, error) {
	body, status, err := c.rawPost(ctx, rpc("job", "save_job", map[string]any{
		"name": name,
		"cmd":  "local",
		"fun":  fun,
		"arg":  jobArg{Arg: []any{}, Kwarg: kwarg},
	}))
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

func (c *RaaSClient) routeCmd(ctx context.Context, jobUUID, minionID string) (string, error) {
	body, status, err := c.rawPost(ctx, rpc("cmd", "route_cmd", map[string]any{
		"job_uuid": jobUUID,
		"tgt": map[string]any{
			"salt": map[string]any{
				"tgt":      "L@" + minionID,
				"tgt_type": "compound",
			},
		},
	}))
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

func (c *RaaSClient) MasterLastSeen(ctx context.Context) (string, error) {
	body, statusCode, err := c.rawPost(ctx, rpc("master", "get_master_presence", map[string]any{}))
	if err != nil {
		return "", fmt.Errorf("get_master_presence: %w", err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return "", fmt.Errorf("get_master_presence: HTTP %d: %s", statusCode, string(body))
	}
	if err := checkBodyError(body); err != nil {
		return "", fmt.Errorf("get_master_presence: %w", err)
	}
	var resp struct {
		Ret struct {
			Results []struct {
				MasterID string `json:"master_id"`
				LastSeen string `json:"last_seen"`
			} `json:"results"`
		} `json:"ret"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("get_master_presence: %w", err)
	}
	for _, m := range resp.Ret.Results {
		if m.MasterID == c.masterID {
			return m.LastSeen, nil
		}
	}
	return "", nil
}

func (c *RaaSClient) DeleteKey(ctx context.Context, minionID string) error {
	respBody, statusCode, err := c.rawPost(ctx, rpc("minions", "set_minion_key_state", map[string]any{
		"minions": []any{[]any{c.masterID, minionID}},
		"state":   "delete",
	}))
	if err != nil {
		return fmt.Errorf("delete key for %q: %w", minionID, err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return fmt.Errorf("delete key for %q: HTTP %d: %s", minionID, statusCode, string(respBody))
	}
	c.log.V(1).Info("delete response", "minionID", minionID, "body", string(respBody))
	if err := checkBodyError(respBody); err != nil {
		return fmt.Errorf("delete key for %q: %w", minionID, err)
	}
	return nil
}

func (c *RaaSClient) KeyStates(ctx context.Context, minionID string) ([]string, error) {
	body, status, err := c.rawPost(ctx, rpc("minions", "get_minion_key_state",
		map[string]any{"minion_id": minionID}))
	if err != nil {
		return nil, fmt.Errorf("key_state(%s): %w", minionID, err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("key_state(%s): HTTP %d: %s", minionID, status, body)
	}
	if err := checkBodyError(body); err != nil {
		return nil, fmt.Errorf("key_state(%s): %w", minionID, err)
	}
	var resp struct {
		Ret struct {
			Results []struct {
				Minion   string   `json:"minion"`
				KeyState []string `json:"key_state"`
			} `json:"results"`
		} `json:"ret"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("key_state(%s): parse response: %w", minionID, err)
	}
	var states []string
	for _, r := range resp.Ret.Results {
		if r.Minion == minionID {
			states = append(states, r.KeyState...)
		}
	}
	return states, nil
}

func (c *RaaSClient) GenAcceptKey(ctx context.Context, minionID string, force bool) (*KeyPair, error) {
	ret, err := c.wheel(ctx, "key.gen_accept", []any{minionID}, map[string]any{"force": force})
	if err != nil {
		return nil, err
	}
	var pair KeyPair
	if err := json.Unmarshal(ret, &pair); err != nil {
		return nil, fmt.Errorf("key.gen_accept(%s): unexpected return shape", minionID)
	}
	if pair.Pub == "" && pair.Priv == "" {
		return nil, nil
	}
	if pair.Pub == "" || pair.Priv == "" {
		return nil, fmt.Errorf("key.gen_accept(%s): incomplete key pair returned", minionID)
	}
	return &pair, nil
}

func (c *RaaSClient) MasterPublicKey(ctx context.Context) (string, error) {
	c.mu.RLock()
	cached := c.masterPub
	c.mu.RUnlock()
	if cached != "" {
		return cached, nil
	}
	ret, err := c.wheel(ctx, "key.master_key_str", []any{}, map[string]any{})
	if err != nil {
		return "", err
	}
	var resp struct {
		Local map[string]string `json:"local"`
	}
	if err := json.Unmarshal(ret, &resp); err != nil {
		return "", fmt.Errorf("key.master_key_str: parse return: %w", err)
	}
	if len(resp.Local) != 1 {
		return "", fmt.Errorf("key.master_key_str: expected one master key, got %d", len(resp.Local))
	}
	var pub string
	for _, v := range resp.Local {
		pub = v
	}
	if !strings.Contains(pub, "PUBLIC KEY") {
		return "", fmt.Errorf("key.master_key_str: return is not a public key")
	}
	c.mu.Lock()
	c.masterPub = pub
	c.mu.Unlock()
	return pub, nil
}

func (c *RaaSClient) wheel(ctx context.Context, fun string, args []any, kwarg map[string]any) (json.RawMessage, error) {
	body, status, err := c.rawPost(ctx, rpc("cmd", "route_cmd", map[string]any{
		"cmd":     "wheel",
		"fun":     fun,
		"masters": []any{c.masterID},
		"arg":     jobArg{Arg: args, Kwarg: kwarg},
	}))
	if err != nil {
		return nil, fmt.Errorf("%s: route_cmd: %w", fun, err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%s: route_cmd: HTTP %d: %s", fun, status, body)
	}
	var routed struct {
		Ret   string         `json:"ret"`
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(body, &routed); err != nil {
		return nil, fmt.Errorf("%s: route_cmd: parse response: %w", fun, err)
	}
	if routed.Error != nil || routed.Ret == "" {
		return nil, fmt.Errorf("%s: route_cmd: %s", fun, body)
	}
	jid := routed.Ret
	c.log.V(1).Info("dispatched wheel job", "fun", fun, "jid", jid)

	for {
		body, status, err := c.rawPost(ctx, rpc("ret", "get_jid", map[string]any{"jid": jid}))
		if err != nil {
			return nil, fmt.Errorf("%s: get_jid(%s): %w", fun, jid, err)
		}
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("%s: get_jid(%s): HTTP %d", fun, jid, status)
		}
		var resp struct {
			Ret   map[string]json.RawMessage `json:"ret"`
			Error map[string]any             `json:"error"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("%s: get_jid(%s): parse response: %w", fun, jid, err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: get_jid(%s): API error: %v", fun, jid, resp.Error["message"])
		}
		if raw, ok := resp.Ret[c.masterID]; ok {
			var result struct {
				Return  json.RawMessage `json:"return"`
				Success bool            `json:"success"`
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				return nil, fmt.Errorf("%s: get_jid(%s): parse result: %w", fun, jid, err)
			}
			if !result.Success {
				return nil, fmt.Errorf("%s: failed on master %s (jid %s)", fun, c.masterID, jid)
			}
			return result.Return, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s: timed out waiting for result (jid=%s): %w", fun, jid, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

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

func (c *RaaSClient) fetchCookies(ctx context.Context, path string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return 0, fmt.Errorf("build GET %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func (c *RaaSClient) rawPost(ctx context.Context, fields map[string]any) ([]byte, int, error) {
	body, status, err := c.doPost(ctx, fields)
	if err != nil || status != http.StatusUnauthorized {
		return body, status, err
	}
	if loginErr := c.Login(ctx); loginErr != nil {
		return nil, status, fmt.Errorf("JWT expired; re-login failed: %w", loginErr)
	}
	return c.doPost(ctx, fields)
}

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

func checkBodyError(body []byte) error {
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
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

func extractMinions(body []byte) ([]string, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("response is not valid JSON: %w", err)
	}

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
