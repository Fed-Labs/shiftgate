package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// commandSettleFake is a control-plane stub for the command channel: it hands
// out one command and counts settlement attempts.
type commandSettleFake struct {
	mu            sync.Mutex
	command       string
	completeCalls int
	completeBody  map[string]any
	failCompletes int
	heartbeats    int
}

func (fake *commandSettleFake) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	switch {
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/heartbeat"):
		fake.heartbeats++
		writer.WriteHeader(http.StatusOK)
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/commands/poll"):
		if fake.command == "" {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		command := fake.command
		fake.command = ""
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(command))
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/complete"):
		content, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(content, &fake.completeBody)
		fake.completeCalls++
		if fake.completeCalls <= fake.failCompletes {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"status":"completed"}`))
	default:
		http.NotFound(writer, request)
	}
}

func (fake *commandSettleFake) completions() (int, map[string]any) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.completeCalls, fake.completeBody
}

// A command handler slower than the heartbeat budget must still settle: the
// claim runs on a deadline of its own, not the heartbeat's shared one.
func TestReporterSettlesCommandPastHeartbeatBudget(t *testing.T) {
	fake := &commandSettleFake{command: `{"id":"command-1","action":"relay.start","payload":{},"claim_token":"token-1","created_at":"2026-01-01T00:00:00Z"}`}
	server := httptest.NewServer(fake)
	defer server.Close()
	reporter := NewReporter(server.URL, "org-1", "machine-1", "Source", "", "shift_ak_secret", time.Second, 25*time.Millisecond, slog.New(slog.DiscardHandler))
	handlerRan := make(chan struct{})
	reporter.SetCommandHandler(func(context.Context, AgentCommand) error {
		time.Sleep(75 * time.Millisecond)
		close(handlerRan)
		return nil
	})
	reporter.report(context.Background())
	select {
	case <-handlerRan:
	case <-time.After(2 * time.Second):
		t.Fatal("command handler never ran")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		calls, body := fake.completions()
		if calls > 0 {
			if body["failed"] != false {
				t.Fatalf("completion reported failure: %v", body)
			}
			if body["claim_token"] != "token-1" {
				t.Fatalf("completion lost the claim token: %v", body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command was never settled: a handler past the heartbeat budget must still complete its claim")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A failed settlement POST is retried until acknowledged instead of being
// dropped (which would strand the claim until the lease requeues it).
func TestReporterRetriesCommandSettlement(t *testing.T) {
	fake := &commandSettleFake{
		command:       `{"id":"command-2","action":"relay.join","payload":{},"claim_token":"token-2","created_at":"2026-01-01T00:00:00Z"}`,
		failCompletes: 2,
	}
	server := httptest.NewServer(fake)
	defer server.Close()
	reporter := NewReporter(server.URL, "org-1", "machine-1", "Source", "", "shift_ak_secret", time.Second, time.Second, slog.New(slog.DiscardHandler))
	reporter.SetCommandHandler(func(context.Context, AgentCommand) error { return nil })
	reporter.report(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for {
		calls, _ := fake.completions()
		if calls >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("settlement was attempted %d times, want retries until acknowledged", calls)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A command whose payload cannot be decoded still settles (as failed) when
// its identity decoded: an unsettleable decode error must not strand the
// claim silently.
func TestReporterSettlesUndecodableCommand(t *testing.T) {
	var settled atomic.Int32
	fake := &commandSettleFake{command: `{"id":"command-3","claim_token":"token-3","created_at":"not-a-time"}`}
	server := httptest.NewServer(fake)
	defer server.Close()
	reporter := NewReporter(server.URL, "org-1", "machine-1", "Source", "", "shift_ak_secret", time.Second, time.Second, slog.New(slog.DiscardHandler))
	reporter.SetCommandHandler(func(context.Context, AgentCommand) error {
		settled.Add(1)
		return nil
	})
	reporter.report(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for {
		calls, body := fake.completions()
		if calls > 0 {
			if settled.Load() != 0 {
				t.Fatal("the undecodable command reached the handler")
			}
			if body["failed"] != true {
				t.Fatalf("undecodable command settled as success: %v", body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("an undecodable command was never settled")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The claim runs on its own settlement budget, never the heartbeat's.
func TestReporterPollCommandBudgetIsIndependent(t *testing.T) {
	if commandSettleBudget <= 2*time.Second {
		t.Fatalf("command settle budget %s is not independent of the heartbeat budget", commandSettleBudget)
	}
	reporter := NewReporter("http://example.invalid", "org-1", "machine-1", "Source", "", "key", time.Second, 10*time.Millisecond, slog.New(slog.DiscardHandler))
	if reporter.timeout != 10*time.Millisecond {
		t.Fatalf("heartbeat timeout = %s", reporter.timeout)
	}
}
