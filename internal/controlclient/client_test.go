package controlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shift.dev/shift/internal/config"
)

// The tests here drive the same flows the desktop client exercises: login
// persists the pair, refresh persists the rotation, a 401 triggers exactly one
// refresh and one replay, and a token file from another control plane is
// never replayed against this one.

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func testClient(roundTrip roundTripFunc) *Client {
	return &Client{baseURL: "http://control-plane", http: &http.Client{Transport: roundTrip, Timeout: time.Second}}
}

func testResponse(status int, value any) *http.Response {
	var body []byte
	if value != nil {
		body, _ = json.Marshal(value)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d", status),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// TestLoginPersistsTheSession proves Session.Login writes the pair (URL keyed)
// so the next command can use it without asking again.
func TestLoginPersistsTheSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli-session.json")
	expiry := time.Now().Add(time.Hour).UTC()
	client := testClient(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/auth/login" {
			return nil, fmt.Errorf("unexpected path %s", request.URL.Path)
		}
		return testResponse(http.StatusOK, map[string]any{
			"user":   map[string]any{"id": "user-1", "email": "operator@example.com", "display_name": "Operator", "created_at": time.Now().UTC()},
			"tokens": map[string]any{"session_id": "session-1", "access_token": "shift_at_1", "refresh_token": "shift_rt_1", "token_type": "Bearer", "expires_at": expiry},
		}), nil
	})
	store, err := OpenTokenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{client: client, store: store}
	user, err := session.Login(context.Background(), "operator@example.com", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "operator@example.com" {
		t.Fatalf("unexpected user: %#v", user)
	}
	stored, err := store.Load("http://control-plane")
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "shift_at_1" || stored.RefreshToken != "shift_rt_1" || stored.Email != "operator@example.com" {
		t.Fatalf("session was not persisted as returned: %#v", stored)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("token file permissions are %o, want 0600", mode)
	}
}

// TestLoadRejectsSessionsFromAnotherControlPlane proves the URL is part of the
// session's identity: a file written for a different server reads as absent
// rather than being replayed against this one.
func TestLoadRejectsSessionsFromAnotherControlPlane(t *testing.T) {
	store, err := OpenTokenStore(filepath.Join(t.TempDir(), "cli-session.json"))
	if err != nil {
		t.Fatal(err)
	}
	err = store.Save(StoredSession{ControlPlaneURL: "https://other.example.com", AccessToken: "shift_at_1", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("https://this.example.com"); err != ErrNoSession {
		t.Fatalf("expected ErrNoSession for a foreign control plane, got %v", err)
	}
}

// TestCallRefreshesOnceAndReplaysOnce proves the 401 path: one forced
// refresh, one replay of the original request, and the rotated pair persisted
// before the replay runs.
func TestCallRefreshesOnceAndReplaysOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli-session.json")
	store, err := OpenTokenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Save(StoredSession{
		ControlPlaneURL: "http://control-plane",
		AccessToken:     "shift_at_stale",
		RefreshToken:    "shift_rt_1",
		Email:           "operator@example.com",
		ExpiresAt:       time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	refreshes := 0
	machineRequests := 0
	client := testClient(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/v1/auth/refresh":
			refreshes++
			if request.Header.Get("Authorization") != "" {
				t.Error("refresh must not send the access token")
			}
			return testResponse(http.StatusOK, map[string]any{"session_id": "session-1", "access_token": "shift_at_fresh", "refresh_token": "shift_rt_2", "token_type": "Bearer", "expires_at": time.Now().Add(time.Hour)}), nil
		case "/v1/organizations/org-1/machines":
			machineRequests++
			if request.Header.Get("Authorization") == "Bearer shift_at_stale" {
				return testResponse(http.StatusUnauthorized, map[string]any{"code": "UNAUTHORIZED", "message": "token rejected"}), nil
			}
			if request.Header.Get("Authorization") != "Bearer shift_at_fresh" {
				t.Errorf("replay used an unexpected token: %q", request.Header.Get("Authorization"))
			}
			return testResponse(http.StatusOK, []Machine{{ID: "machine-1", MachineID: "workstation"}}), nil
		default:
			return nil, fmt.Errorf("unexpected path %s", request.URL.Path)
		}
	})
	// Build the session the way NewSession does — current loaded from the
	// store, not zero — so the first call carries the stale token and the 401
	// path (not the proactive-expiry path) is what runs.
	current, err := store.Load("http://control-plane")
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{client: client, store: store, current: current}
	machines, err := session.Machines(context.Background(), "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 1 || machines[0].MachineID != "workstation" {
		t.Fatalf("machines were not returned: %#v", machines)
	}
	if refreshes != 1 || machineRequests != 2 {
		t.Fatalf("expected one refresh and two machine requests, got %d refreshes and %d requests", refreshes, machineRequests)
	}
	stored, err := store.Load("http://control-plane")
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "shift_at_fresh" || stored.RefreshToken != "shift_rt_2" {
		t.Fatalf("rotated pair was not persisted: %#v", stored)
	}
}

// TestFailedRefreshClearsTheStore proves a rejected rotation drops the file:
// rotation may have already consumed the old refresh token, so keeping it
// would promise a session that no longer exists.
func TestFailedRefreshClearsTheStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli-session.json")
	store, err := OpenTokenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// An expiry in the past forces the refresh path before any call.
	err = store.Save(StoredSession{ControlPlaneURL: "http://control-plane", AccessToken: "shift_at_stale", RefreshToken: "shift_rt_used", ExpiresAt: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	// Load the stored (expired) session the way NewSession does, so it is the
	// stored expiry — not a zero value — that forces the refresh path.
	current, err := store.Load("http://control-plane")
	if err != nil {
		t.Fatal(err)
	}
	client := testClient(func(request *http.Request) (*http.Response, error) {
		return testResponse(http.StatusUnauthorized, map[string]any{"code": "REFRESH_INVALID", "message": "refresh token was already used"}), nil
	})
	session := &Session{client: client, store: store, current: current}
	if _, err := session.Machines(context.Background(), "org-1"); err == nil {
		t.Fatal("expected an error when the refresh token is rejected")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("token store was not cleared after a failed refresh: %v", err)
	}
}

// TestNewDefaultsToThePlatformControlPlane pins the hosted behavior: an empty
// URL means the platform's control plane, the way a hosted service's SDK
// carries its endpoint, so operators never have to name it.
func TestNewDefaultsToThePlatformControlPlane(t *testing.T) {
	client, err := New("", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if client.URL() != config.DefaultControlPlaneURL {
		t.Fatalf("client URL = %q, want the platform default %q", client.URL(), config.DefaultControlPlaneURL)
	}

	// An explicit URL still wins — private control planes keep working — and
	// keeps working without a trailing slash.
	client, err = New("https://control.example.test/", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if client.URL() != "https://control.example.test" {
		t.Fatalf("client URL = %q, want the explicit URL", client.URL())
	}

	// A URL without a scheme stays a configuration error, not a runtime one.
	if _, err := New("control.example.test", time.Second); err == nil {
		t.Fatal("expected an error for a URL without a scheme")
	}
}
