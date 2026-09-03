package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLoginThenFleetMachines drives the whole control-plane flow through the
// real command surface: login persists a session against a live control plane,
// and `shift machines` — with a control plane configured — answers with the
// fleet view under the session's access token.
func TestLoginThenFleetMachines(t *testing.T) {
	var mutex sync.Mutex
	machineToken := ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/auth/login":
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"user": map[string]any{"id": "user-1", "email": "operator@example.com", "display_name": "Operator", "created_at": time.Now().UTC()},
				"tokens": map[string]any{
					"session_id":    "session-1",
					"access_token":  "shift_at_test",
					"refresh_token": "shift_rt_test",
					"token_type":    "Bearer",
					"expires_at":    time.Now().Add(time.Hour).UTC(),
				},
			})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/organizations":
			if request.Header.Get("Authorization") != "Bearer shift_at_test" {
				http.Error(writer, "missing access token", http.StatusUnauthorized)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode([]map[string]any{{"id": "org-1", "name": "Example", "role": "owner", "created_at": time.Now().UTC()}})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/organizations/org-1/machines":
			mutex.Lock()
			machineToken = request.Header.Get("Authorization")
			mutex.Unlock()
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode([]map[string]any{{
				"id": "machine-record-1", "organization_id": "org-1", "machine_id": "workstation",
				"name": "Workstation", "agent_url": "https://workstation:8443", "capabilities": map[string]any{},
				"status": "online", "created_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
			}})
		default:
			http.Error(writer, "unexpected request "+request.Method+" "+request.URL.Path, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	tokenStore := filepath.Join(t.TempDir(), "cli-session.json")

	// The password comes from stdin, never a flag: hand the command a file so
	// the read works without a terminal.
	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte("correct horse battery staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalStdin := os.Stdin
	file, err := os.Open(passwordFile)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = file
	defer func() {
		os.Stdin = originalStdin
		_ = file.Close()
	}()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run([]string{"--control-url", server.URL, "--token-store", tokenStore, "login", "--email", "operator@example.com"}, &stdout, &stderr); err != nil {
		t.Fatalf("login failed: %v: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Logged in as Operator") {
		t.Fatalf("login output missing identity: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := run([]string{"--control-url", server.URL, "--token-store", tokenStore, "--json", "machines"}, &stdout, &stderr); err != nil {
		t.Fatalf("fleet machines failed: %v: %s", err, stderr.String())
	}
	var machines []struct {
		MachineID string `json:"machine_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &machines); err != nil {
		t.Fatalf("machines output was not JSON: %v: %s", err, stdout.String())
	}
	if len(machines) != 1 || machines[0].MachineID != "workstation" || machines[0].Status != "online" {
		t.Fatalf("unexpected machines: %+v", machines)
	}
	mutex.Lock()
	token := machineToken
	mutex.Unlock()
	if token != "Bearer shift_at_test" {
		t.Fatalf("machines request was sent with %q", token)
	}
}

// TestFleetMachinesRequiresLogin proves the fleet commands fail with a pointer
// to login rather than a raw unauthorized when no session is stored.
func TestFleetMachinesRequiresLogin(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	tokenStore := filepath.Join(t.TempDir(), "cli-session.json")
	err := run([]string{"--control-url", "http://127.0.0.1:1", "--token-store", tokenStore, "machines"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected an error when no session is stored")
	}
	if !strings.Contains(err.Error(), "shift login") {
		t.Fatalf("error does not point at login: %v", err)
	}
}
