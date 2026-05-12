package salt_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/stanimir-kozarev/vcf-salt-operator/pkg/salt"
)

const (
	testXSRF = "2|testtoken|abcdef123456|1234567890"
	testJWT  = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.test.signature"
)

// mockRaaSServer creates a test HTTP server that simulates the VCF Salt RaaS API.
// Flow: GET /account/login → _xsrf cookie;
//
//	POST /account/login (JSON) → JWT;
//	POST /rpc → Bearer JWT + X-Xsrftoken.
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

func TestLogin_BadCredentials(t *testing.T) {
	// Server sets _xsrf cookie but rejects wrong credentials with 401
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

	// Use wrong credentials: /rpc returns 401, rawPost retries by calling Login,
	// Login also returns 401 (bad creds) → AcceptKey must return an error.
	client := salt.NewRaaSClient(server.URL, "wrong-user", "wrong-pass", "test-master", true, logr.Discard())

	if err := client.AcceptKey(context.Background(), "vm-01"); err == nil {
		t.Fatal("expected AcceptKey to fail when re-login fails after 401, got nil")
	}
}

func TestLogin_NoCookieAfterLogin(t *testing.T) {
	// Server that never sets the _xsrf cookie
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := salt.NewRaaSClient(server.URL, "admin", "password", "test-master", true, logr.Discard())
	if err := client.Login(context.Background()); err == nil {
		t.Fatal("expected Login to fail when server sets no _xsrf cookie, got nil")
	}
}

// mockRaaSServerWithKeyStateError returns a server that responds to set_minion_key_state
// with HTTP 200 but an error payload in the body — the real RaaS behaviour on failure.
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
		// HTTP 200 with error payload — the RaaS API behaviour on set_minion_key_state failure.
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
	// Verify Bearer JWT and X-Xsrftoken are both sent on /rpc calls
	server := mockRaaSServer(t, []string{})
	defer server.Close()

	client := salt.NewRaaSClient(server.URL, "admin", "password", "test-master", true, logr.Discard())
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("login failed: %v", err)
	}
	// ListPendingKeys calls /rpc — mock returns 401 if no Bearer, 403 if no X-Xsrftoken
	if _, err := client.ListPendingKeys(context.Background()); err != nil {
		t.Errorf("expected /rpc to succeed with Bearer JWT and X-Xsrftoken, got: %v", err)
	}
}
