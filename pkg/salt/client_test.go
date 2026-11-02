package salt_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	"github.com/stanimir-kozarev/vcf-salt-operator/pkg/salt"
)

const (
	testXSRF = "2|testtoken|abcdef123456|1234567890"
	testJWT  = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.test.signature"
)

func mockRaaSServer(t *testing.T, pendingKeys []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/account/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_xsrf", Value: testXSRF, Path: "/"})
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var creds map[string]string
		if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if creds["username"] != "admin" || creds["password"] != "password" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"jwt": testJWT})
	})

	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+testJWT {
			http.Error(w, "unauthorized: missing/invalid bearer token", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Xsrftoken") != testXSRF {
			http.Error(w, "forbidden: missing xsrf", http.StatusForbidden)
			return
		}

		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		method, _ := req["method"].(string)
		w.Header().Set("Content-Type", "application/json")

		switch method {
		case "get_minion_key_state":
			kwarg, _ := req["kwarg"].(map[string]any)
			keyState, _ := kwarg["key_state"].(string)
			var results []any
			switch keyState {
			case "pending":
				for _, k := range pendingKeys {
					results = append(results, map[string]any{"minion": k, "key_state": []string{"pending"}})
				}
			case "accepted":
				results = []any{map[string]any{"minion": "accepted-minion-1", "key_state": []string{"accepted"}}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ret": map[string]any{"count": len(results), "results": results},
			})
		case "set_minion_key_state":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ret": map[string]any{"task_ids": []string{"task-123"}},
			})
		default:
			http.Error(w, "unknown method: "+method, http.StatusBadRequest)
		}
	})

	return httptest.NewServer(mux)
}

func TestLogin_Success(t *testing.T) {
	server := mockRaaSServer(t, nil)
	defer server.Close()

	client := salt.NewRaaSClient(server.URL, "admin", "password", "test-master", true, logr.Discard())
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("expected Login to succeed, got: %v", err)
	}
}

func TestLogin_DebugLogOmitsPassword(t *testing.T) {
	server := mockRaaSServer(t, nil)
	defer server.Close()

	var logged strings.Builder
	log := funcr.New(func(prefix, args string) { logged.WriteString(args + "\n") }, funcr.Options{Verbosity: 9})
	client := salt.NewRaaSClient(server.URL, "admin", "password", "test-master", true, log)
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("expected Login to succeed, got: %v", err)
	}
	if !strings.Contains(logged.String(), "/account/login") {
		t.Fatalf("expected debug output for the login, got: %s", logged.String())
	}
	if strings.Contains(logged.String(), "password") {
		t.Fatalf("debug log carries the password: %s", logged.String())
	}
}

func TestLogin_BadCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_xsrf", Value: testXSRF, Path: "/"})
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	client := salt.NewRaaSClient(server.URL, "wrong", "creds", "test-master", true, logr.Discard())
	if err := client.Login(context.Background()); err == nil {
		t.Fatal("expected Login to fail with bad credentials, got nil error")
	}
}

func TestListPendingKeys_ReturnsPendingKeys(t *testing.T) {
	expected := []string{"vm-web-01", "vm-db-02"}
	server := mockRaaSServer(t, expected)
	defer server.Close()

	client := salt.NewRaaSClient(server.URL, "admin", "password", "test-master", true, logr.Discard())
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("login failed: %v", err)
	}

	pending, err := client.ListPendingKeys(context.Background())
	if err != nil {
		t.Fatalf("ListPendingKeys failed: %v", err)
	}
	if len(pending) != len(expected) {
		t.Fatalf("expected %d pending keys, got %d: %v", len(expected), len(pending), pending)
	}
	for i, k := range expected {
		if pending[i] != k {
			t.Errorf("key[%d]: expected %q, got %q", i, k, pending[i])
		}
	}
}

func TestListPendingKeys_Empty(t *testing.T) {
	server := mockRaaSServer(t, []string{})
	defer server.Close()

	client := salt.NewRaaSClient(server.URL, "admin", "password", "test-master", true, logr.Discard())
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("login failed: %v", err)
	}

	pending, err := client.ListPendingKeys(context.Background())
	if err != nil {
		t.Fatalf("ListPendingKeys failed: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 pending keys, got: %v", pending)
	}
}

func TestListAcceptedKeys_ReturnsAcceptedKeys(t *testing.T) {
	server := mockRaaSServer(t, []string{"pending-vm"})
	defer server.Close()

	client := salt.NewRaaSClient(server.URL, "admin", "password", "test-master", true, logr.Discard())
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("login failed: %v", err)
	}

	accepted, err := client.ListAcceptedKeys(context.Background())
	if err != nil {
		t.Fatalf("ListAcceptedKeys failed: %v", err)
	}
	if len(accepted) != 1 || accepted[0] != "accepted-minion-1" {
		t.Errorf("expected [accepted-minion-1], got: %v", accepted)
	}
}

func TestAcceptKey_Success(t *testing.T) {
	server := mockRaaSServer(t, []string{"vm-web-01"})
	defer server.Close()

	client := salt.NewRaaSClient(server.URL, "admin", "password", "test-master", true, logr.Discard())
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("login failed: %v", err)
	}

	if err := client.AcceptKey(context.Background(), "vm-web-01"); err != nil {
		t.Fatalf("AcceptKey failed: %v", err)
	}
}

func TestDeleteKey_Success(t *testing.T) {
	server := mockRaaSServer(t, []string{"old-vm-01"})
	defer server.Close()

	client := salt.NewRaaSClient(server.URL, "admin", "password", "test-master", true, logr.Discard())
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("login failed: %v", err)
	}

	if err := client.DeleteKey(context.Background(), "old-vm-01"); err != nil {
		t.Fatalf("DeleteKey failed: %v", err)
	}
}

func TestAcceptKey_Unauthorized(t *testing.T) {
	server := mockRaaSServer(t, []string{"vm-01"})
	defer server.Close()

	client := salt.NewRaaSClient(server.URL, "wrong-user", "wrong-pass", "test-master", true, logr.Discard())

	if err := client.AcceptKey(context.Background(), "vm-01"); err == nil {
		t.Fatal("expected AcceptKey to fail when re-login fails after 401, got nil")
	}
}

func TestLogin_NoCookieAfterLogin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := salt.NewRaaSClient(server.URL, "admin", "password", "test-master", true, logr.Discard())
	err := client.Login(context.Background())
	if err == nil {
		t.Fatal("expected Login to fail when server sets no _xsrf cookie, got nil")
	}
	if !strings.Contains(err.Error(), "no _xsrf cookie") || !strings.Contains(err.Error(), "HTTP 200") {
		t.Errorf("error should name the missing cookie and the HTTP status, got: %v", err)
	}
}

func TestLogin_UnreachableReportsTransportError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close()

	client := salt.NewRaaSClient(url, "admin", "password", "test-master", true, logr.Discard())
	err := client.Login(context.Background())
	if err == nil {
		t.Fatal("expected Login to fail against a closed server, got nil")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error should carry the transport cause, got: %v", err)
	}
	if strings.Contains(err.Error(), "no _xsrf cookie") {
		t.Errorf("an unreachable server must not be reported as a missing cookie, got: %v", err)
	}
}

func mockRaaSServerWithKeyStateError(t *testing.T, errorMessage string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/account/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_xsrf", Value: testXSRF, Path: "/"})
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"jwt": testJWT})
	})
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ret":   nil,
			"error": map[string]any{"message": errorMessage},
		})
	})
	return httptest.NewServer(mux)
}

func TestAcceptKey_BodyLevelError(t *testing.T) {
	server := mockRaaSServerWithKeyStateError(t, "minion key not found in pending state")
	defer server.Close()

	client := salt.NewRaaSClient(server.URL, "admin", "password", "test-master", true, logr.Discard())
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("login failed: %v", err)
	}

	err := client.AcceptKey(context.Background(), "nonexistent-minion")
	if err == nil {
		t.Fatal("expected AcceptKey to return error on body-level API error, got nil")
	}
	if !strings.Contains(err.Error(), "API error") {
		t.Errorf("expected error to contain 'API error', got: %v", err)
	}
}

func TestDeleteKey_BodyLevelError(t *testing.T) {
	server := mockRaaSServerWithKeyStateError(t, "minion key not found")
	defer server.Close()

	client := salt.NewRaaSClient(server.URL, "admin", "password", "test-master", true, logr.Discard())
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("login failed: %v", err)
	}

	err := client.DeleteKey(context.Background(), "nonexistent-minion")
	if err == nil {
		t.Fatal("expected DeleteKey to return error on body-level API error, got nil")
	}
	if !strings.Contains(err.Error(), "API error") {
		t.Errorf("expected error to contain 'API error', got: %v", err)
	}
}

func TestRPC_BearerJWTAndXSRFSent(t *testing.T) {
	server := mockRaaSServer(t, []string{})
	defer server.Close()

	client := salt.NewRaaSClient(server.URL, "admin", "password", "test-master", true, logr.Discard())
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if _, err := client.ListPendingKeys(context.Background()); err != nil {
		t.Errorf("expected /rpc to succeed with Bearer JWT and X-Xsrftoken, got: %v", err)
	}
}

const (
	testPub  = "-----BEGIN PUBLIC KEY-----\nMIIBIjAN\n-----END PUBLIC KEY-----"
	testPriv = "-----BEGIN RSA PRIVATE KEY-----\nMIIEsecret\n-----END RSA PRIVATE KEY-----"
)

func mockRPCServer(t *testing.T, handle func(method string, kwarg map[string]any) any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/account/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_xsrf", Value: testXSRF, Path: "/"})
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"jwt": testJWT})
	})
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		method, _ := req["method"].(string)
		kwarg, _ := req["kwarg"].(map[string]any)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(handle(method, kwarg))
	})
	return httptest.NewServer(mux)
}

func wheelServer(t *testing.T, returns map[string]any, routed *[]map[string]any) *httptest.Server {
	t.Helper()
	return mockRPCServer(t, func(method string, kwarg map[string]any) any {
		switch method {
		case "route_cmd":
			*routed = append(*routed, kwarg)
			return map[string]any{"ret": "jid-" + kwarg["fun"].(string), "error": nil}
		case "get_jid":
			fun := strings.TrimPrefix(kwarg["jid"].(string), "jid-")
			return map[string]any{"ret": map[string]any{
				"test-master": map[string]any{"return": returns[fun], "success": true},
			}}
		}
		return map[string]any{"error": map[string]any{"message": "unexpected " + method}}
	})
}

func loggedIn(t *testing.T, url string, log logr.Logger) *salt.RaaSClient {
	t.Helper()
	c := salt.NewRaaSClient(url, "admin", "password", "test-master", true, log)
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("login failed: %v", err)
	}
	return c
}

func TestKeyStates_FiltersToExactMinion(t *testing.T) {
	var asked any
	server := mockRPCServer(t, func(method string, kwarg map[string]any) any {
		asked = kwarg["minion_id"]
		return map[string]any{"ret": map[string]any{"results": []any{
			map[string]any{"minion": "vm-a", "key_state": []string{"accepted", "denied"}},
			map[string]any{"minion": "vm-a2", "key_state": []string{"pending"}},
		}}}
	})
	defer server.Close()

	states, err := loggedIn(t, server.URL, logr.Discard()).KeyStates(context.Background(), "vm-a")
	if err != nil {
		t.Fatalf("KeyStates failed: %v", err)
	}
	if asked != "vm-a" {
		t.Errorf("expected the minion_id filter vm-a, got %v", asked)
	}
	if strings.Join(states, ",") != "accepted,denied" {
		t.Errorf("expected [accepted denied], got %v", states)
	}
}

func TestGenAcceptKey_SendsWheelJobAndReturnsPair(t *testing.T) {
	var routed []map[string]any
	server := wheelServer(t, map[string]any{"key.gen_accept": map[string]any{"pub": testPub, "priv": testPriv}}, &routed)
	defer server.Close()

	pair, err := loggedIn(t, server.URL, logr.Discard()).GenAcceptKey(context.Background(), "vm-a", true)
	if err != nil {
		t.Fatalf("GenAcceptKey failed: %v", err)
	}
	if pair == nil || pair.Pub != testPub || pair.Priv != testPriv {
		t.Fatalf("unexpected pair: %+v", pair)
	}
	got, _ := json.Marshal(routed[0])
	want := `{"arg":{"arg":["vm-a"],"kwarg":{"force":true}},` +
		`"cmd":"wheel","fun":"key.gen_accept","masters":["test-master"]}`
	if string(got) != want {
		t.Errorf("route_cmd kwarg\n got: %s\nwant: %s", got, want)
	}
}

func TestGenAcceptKey_ExistingKeyReturnsNil(t *testing.T) {
	var routed []map[string]any
	server := wheelServer(t, map[string]any{"key.gen_accept": map[string]any{}}, &routed)
	defer server.Close()

	pair, err := loggedIn(t, server.URL, logr.Discard()).GenAcceptKey(context.Background(), "vm-a", false)
	if err != nil || pair != nil {
		t.Fatalf("expected no pair and no error for an existing key, got %+v, %v", pair, err)
	}
}

func TestGenAcceptKey_WaitsForTheReturn(t *testing.T) {
	polls := 0
	server := mockRPCServer(t, func(method string, _ map[string]any) any {
		if method == "route_cmd" {
			return map[string]any{"ret": "jid-1"}
		}
		polls++
		if polls == 1 {
			return map[string]any{"ret": map[string]any{}}
		}
		return map[string]any{"ret": map[string]any{
			"test-master": map[string]any{"return": map[string]any{"pub": testPub, "priv": testPriv}, "success": true},
		}}
	})
	defer server.Close()

	pair, err := loggedIn(t, server.URL, logr.Discard()).GenAcceptKey(context.Background(), "vm-a", false)
	if err != nil || pair == nil {
		t.Fatalf("expected a pair after the second poll, got %+v, %v", pair, err)
	}
	if polls != 2 {
		t.Errorf("expected 2 polls, got %d", polls)
	}
}

func TestGenAcceptKey_FailedJobIsAnError(t *testing.T) {
	server := mockRPCServer(t, func(method string, _ map[string]any) any {
		if method == "route_cmd" {
			return map[string]any{"ret": "jid-1"}
		}
		return map[string]any{"ret": map[string]any{
			"test-master": map[string]any{"return": "Exception occurred in wheel key.gen_accept", "success": false},
		}}
	})
	defer server.Close()

	if _, err := loggedIn(t, server.URL, logr.Discard()).GenAcceptKey(context.Background(), "vm-a", false); err == nil {
		t.Fatal("expected an error for a failed wheel job")
	}
}

func TestGenAcceptKey_PrivateKeyNeverLoggedOrInErrors(t *testing.T) {
	var logged strings.Builder
	log := funcr.New(func(_, args string) { logged.WriteString(args + "\n") }, funcr.Options{Verbosity: 9})

	var routed []map[string]any
	server := wheelServer(t, map[string]any{"key.gen_accept": map[string]any{"pub": testPub, "priv": testPriv}}, &routed)
	defer server.Close()
	if _, err := loggedIn(t, server.URL, log).GenAcceptKey(context.Background(), "vm-a", false); err != nil {
		t.Fatalf("GenAcceptKey failed: %v", err)
	}
	if !strings.Contains(logged.String(), "key.gen_accept") {
		t.Fatalf("expected debug output for the wheel job, got: %s", logged.String())
	}
	if strings.Contains(logged.String(), "MIIEsecret") {
		t.Fatalf("debug log carries the private key: %s", logged.String())
	}

	half := wheelServer(t, map[string]any{"key.gen_accept": map[string]any{"priv": testPriv}}, &routed)
	defer half.Close()
	_, err := loggedIn(t, half.URL, logr.Discard()).GenAcceptKey(context.Background(), "vm-a", false)
	if err == nil {
		t.Fatal("expected an error for a pair without a public key")
	}
	if strings.Contains(err.Error(), "MIIEsecret") {
		t.Fatalf("error carries the private key: %v", err)
	}
}

func TestMasterPublicKey_FetchedOnceThenCached(t *testing.T) {
	var routed []map[string]any
	server := wheelServer(t, map[string]any{"key.master_key_str": map[string]any{
		"local": map[string]any{"master.pub": testPub},
	}}, &routed)
	defer server.Close()

	c := loggedIn(t, server.URL, logr.Discard())
	for range 2 {
		pub, err := c.MasterPublicKey(context.Background())
		if err != nil || pub != testPub {
			t.Fatalf("expected the master key, got %q, %v", pub, err)
		}
	}
	if len(routed) != 1 {
		t.Errorf("expected one wheel job, got %d", len(routed))
	}
}

func TestMasterPublicKey_RejectsAmbiguousOrNonKeyReturns(t *testing.T) {
	for name, local := range map[string]map[string]any{
		"two keys":  {"a.pub": testPub, "b.pub": testPub},
		"not a key": {"master.pub": "no key here"},
	} {
		var routed []map[string]any
		server := wheelServer(t, map[string]any{"key.master_key_str": map[string]any{"local": local}}, &routed)
		if _, err := loggedIn(t, server.URL, logr.Discard()).MasterPublicKey(context.Background()); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		server.Close()
	}
}
