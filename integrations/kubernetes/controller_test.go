package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidateSpecRejectsUnscopedWorkloads(t *testing.T) {
	if err := validateSpec(migrationSpec{WorkloadID: "", SourceMachineID: "a", DestinationMachineID: "b"}); err == nil {
		t.Fatal("empty workload must be rejected")
	}
	if err := validateSpec(migrationSpec{WorkloadID: "w", SourceMachineID: "a", DestinationMachineID: "b", Mode: "hot"}); err == nil {
		t.Fatal("invalid mode must be rejected")
	}
	if err := validateSpec(migrationSpec{WorkloadID: "w", SourceMachineID: "a", DestinationMachineID: "b", Mode: "cold"}); err != nil {
		t.Fatal(err)
	}
}

func TestOperationKeyIsStablePerGeneration(t *testing.T) {
	if operationKey("u1", 2) != operationKey("u1", 2) {
		t.Fatal("operation key changed")
	}
	if operationKey("u1", 2) == operationKey("u1", 3) {
		t.Fatal("generation did not change key")
	}
}

func TestObservedGenerationNormalizesJSONNumbers(t *testing.T) {
	if got := observedGeneration(map[string]any{"observedGeneration": float64(3)}); got != 3 {
		t.Fatalf("float64 generation = %d", got)
	}
	if got := observedGeneration(map[string]any{"observedGeneration": int64(3)}); got != 3 {
		t.Fatalf("int64 generation = %d", got)
	}
	if got := observedGeneration(map[string]any{}); got != -1 {
		t.Fatalf("missing generation = %d", got)
	}
}

func writeCertPEM(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type recorder struct {
	mu       sync.Mutex
	requests []string
}

func (r *recorder) add(entry string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, entry)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *recorder) entries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

func newTestController(t *testing.T, kubeHandler http.Handler, shiftHandler http.Handler) *Controller {
	t.Helper()
	kubeServer := httptest.NewTLSServer(kubeHandler)
	shiftServer := httptest.NewTLSServer(shiftHandler)
	t.Cleanup(kubeServer.Close)
	t.Cleanup(shiftServer.Close)
	config := Config{
		KubernetesURL: kubeServer.URL,
		Token:         "kube-token",
		Namespace:     "default",
		ControlURL:    shiftServer.URL,
		ControlToken:  "shift-token",
		ControlCAFile: writeCertPEM(t, shiftServer.Certificate()),
		Organization:  "org",
		CAFile:        writeCertPEM(t, kubeServer.Certificate()),
		PollInterval:  time.Second,
	}
	kube, err := NewKubeClient(config)
	if err != nil {
		t.Fatal(err)
	}
	shift, err := NewShiftClient(config)
	if err != nil {
		t.Fatal(err)
	}
	return &Controller{Kube: kube, Shift: shift, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PollInterval: time.Second}
}

func migrationJSON(t *testing.T, migration ShiftMigration) map[string]any {
	t.Helper()
	data, err := json.Marshal(migration)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestGetMigrationPaginatesPastFirstPage(t *testing.T) {
	rec := &recorder{}
	controller := newTestController(t,
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			rec.add(request.URL.RequestURI())
			if request.URL.Path != "/v1/organizations/org/migrations" {
				http.NotFound(writer, request)
				return
			}
			offset, err := strconv.Atoi(request.URL.Query().Get("offset"))
			if err != nil {
				t.Fatalf("offset: %v", err)
			}
			all := make([]shiftMigration, 0, migrationsPageSize+1)
			for i := 0; i < migrationsPageSize; i++ {
				all = append(all, shiftMigration{ID: "m-old", Status: "completed"})
			}
			all = append(all, shiftMigration{ID: "m-target", Status: "running"})
			start := offset
			if start > len(all) {
				start = len(all)
			}
			end := start + migrationsPageSize
			if end > len(all) {
				end = len(all)
			}
			_ = json.NewEncoder(writer).Encode(all[start:end])
		}))
	result, err := controller.Shift.getMigration(context.Background(), "m-target")
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "m-target" || result.Status != "running" {
		t.Fatalf("result = %#v", result)
	}
	if rec.count() != 2 {
		t.Fatalf("requests = %v, want two pages (initial + continuation)", rec.entries())
	}
}

func TestGetMigrationNotFoundAfterFullScan(t *testing.T) {
	controller := newTestController(t,
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			_ = json.NewEncoder(writer).Encode([]shiftMigration{{ID: "m1", Status: "completed"}})
		}))
	if _, err := controller.Shift.getMigration(context.Background(), "m-missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want not-found after exhausting pages", err)
	}
}

func TestReconcileCreatesMigrationAfterSpecEditOnTerminalPhase(t *testing.T) {
	rec := &recorder{}
	var createdKey string
	controller := newTestController(t,
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			rec.add(request.Method + " " + request.URL.Path)
			if request.Method == http.MethodGet {
				_ = json.NewEncoder(writer).Encode(kubeList{Items: []ShiftMigration{{
					Metadata: kubeMetadata{Name: "m1", Namespace: "default", UID: "u1", Generation: 2, Finalizers: []string{finalizer}},
					Spec:     migrationSpec{WorkloadID: "w1", SourceMachineID: "s1", DestinationMachineID: "d1"},
					// status decoded from JSON: numbers arrive as float64
					Status: map[string]any{"phase": "Rejected", "reason": "old", "observedGeneration": float64(1), "operationID": ""},
				}}})
				return
			}
			_, _ = writer.Write([]byte(`{}`))
		}),
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			rec.add(request.Method + " " + request.URL.RequestURI())
			createdKey = request.Header.Get("Idempotency-Key")
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": "m-new", "status": "queued"})
		}))
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if createdKey != "kubernetes-u1-2" {
		t.Fatalf("idempotency key = %q, want the new generation's key", createdKey)
	}
	creates := 0
	for _, entry := range rec.entries() {
		if strings.HasPrefix(entry, "POST /v1/organizations/org/migrations") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("requests = %v, want exactly one create under the new generation", rec.entries())
	}
}

func TestReconcileLeavesTerminalPhaseForSameGenerationAlone(t *testing.T) {
	rec := &recorder{}
	controller := newTestController(t,
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			rec.add(request.Method + " " + request.URL.Path)
			if request.Method == http.MethodGet {
				_ = json.NewEncoder(writer).Encode(kubeList{Items: []ShiftMigration{{
					Metadata: kubeMetadata{Name: "m1", Namespace: "default", UID: "u1", Generation: 2, Finalizers: []string{finalizer}},
					Spec:     migrationSpec{WorkloadID: "w1", SourceMachineID: "s1", DestinationMachineID: "d1"},
					Status:   map[string]any{"phase": "Completed", "observedGeneration": float64(2), "operationID": "op1"},
				}}})
			}
		}),
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			rec.add(request.Method + " " + request.URL.RequestURI())
			t.Errorf("unexpected control-plane call %s %s", request.Method, request.URL.Path)
		}))
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, entry := range rec.entries() {
		if strings.Contains(entry, "PUT") {
			t.Fatalf("requests = %v, want no status rewrite for a settled generation", rec.entries())
		}
	}
}

func TestReconcileDeleteKeepsFinalizerUntilMigrationSettled(t *testing.T) {
	now := time.Now()
	build := func(cancelStatus int, migrationStatus string) (*Controller, *recorder, *recorder) {
		kubeRec := &recorder{}
		shiftRec := &recorder{}
		controller := newTestController(t,
			http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				kubeRec.add(request.Method + " " + request.URL.Path)
				_, _ = writer.Write([]byte(`{}`))
			}),
			http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				shiftRec.add(request.Method + " " + request.URL.Path)
				if strings.HasSuffix(request.URL.Path, "/cancel") {
					writer.WriteHeader(cancelStatus)
					_, _ = writer.Write([]byte(`{"message":"control plane error"}`))
					return
				}
				_ = json.NewEncoder(writer).Encode([]shiftMigration{{ID: "op1", Status: migrationStatus}})
			}))
		return controller, kubeRec, shiftRec
	}

	migration := ShiftMigration{
		Metadata: kubeMetadata{Name: "m1", Namespace: "default", UID: "u1", Generation: 1, Finalizers: []string{finalizer}, DeletionTimestamp: &now},
		Spec:     migrationSpec{WorkloadID: "w1", SourceMachineID: "s1", DestinationMachineID: "d1"},
		Status:   map[string]any{"phase": "Running", "operationID": "op1"},
	}

	t.Run("active migration keeps the finalizer", func(t *testing.T) {
		controller, kubeRec, _ := build(http.StatusInternalServerError, "running")
		if err := controller.reconcileOne(context.Background(), &migration); err == nil {
			t.Fatal("expected the uncancelled migration to be reported")
		}
		for _, entry := range kubeRec.entries() {
			if strings.Contains(entry, "PUT") {
				t.Fatalf("requests = %v, want no finalizer removal while the migration is active", kubeRec.entries())
			}
		}
	})

	t.Run("terminal migration releases the object despite cancel failure", func(t *testing.T) {
		controller, kubeRec, _ := build(http.StatusInternalServerError, "completed")
		if err := controller.reconcileOne(context.Background(), &migration); err != nil {
			t.Fatal(err)
		}
		released := false
		for _, entry := range kubeRec.entries() {
			if strings.HasPrefix(entry, "PUT ") && !strings.HasSuffix(entry, "/status") {
				released = true
			}
		}
		if !released {
			t.Fatalf("requests = %v, want the finalizer removed once the migration is terminal", kubeRec.entries())
		}
	})

	t.Run("successful cancel releases the object", func(t *testing.T) {
		controller, kubeRec, shiftRec := build(http.StatusOK, "running")
		if err := controller.reconcileOne(context.Background(), &migration); err != nil {
			t.Fatal(err)
		}
		released := false
		for _, entry := range kubeRec.entries() {
			if strings.HasPrefix(entry, "PUT ") && !strings.HasSuffix(entry, "/status") {
				released = true
			}
		}
		if !released {
			t.Fatalf("requests = %v, want the finalizer removed after a successful cancel", kubeRec.entries())
		}
		if shiftRec.count() != 1 {
			t.Fatalf("shift requests = %v, want just the cancel", shiftRec.entries())
		}
	})
}

func TestReconcileDeleteProceedsWhenSpecInvalid(t *testing.T) {
	now := time.Now()
	kubeRec := &recorder{}
	controller := newTestController(t,
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			kubeRec.add(request.Method + " " + request.URL.Path)
			_, _ = writer.Write([]byte(`{}`))
		}),
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			t.Errorf("unexpected control-plane call %s %s", request.Method, request.URL.Path)
		}))
	migration := ShiftMigration{
		Metadata: kubeMetadata{Name: "m1", Namespace: "default", UID: "u1", Generation: 1, Finalizers: []string{finalizer}, DeletionTimestamp: &now},
		Spec:     migrationSpec{},
	}
	if err := controller.reconcileOne(context.Background(), &migration); err != nil {
		t.Fatalf("deletion must make progress even for an invalid spec: %v", err)
	}
	released := false
	for _, entry := range kubeRec.entries() {
		if strings.HasPrefix(entry, "PUT ") && !strings.HasSuffix(entry, "/status") {
			released = true
		}
	}
	if !released {
		t.Fatalf("requests = %v, want the finalizer removed", kubeRec.entries())
	}
}

func TestReconcileSurfacesCancelledReason(t *testing.T) {
	var status map[string]any
	controller := newTestController(t,
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/status") {
				var payload struct {
					Status map[string]any `json:"status"`
				}
				data, _ := io.ReadAll(request.Body)
				_ = json.Unmarshal(data, &payload)
				status = payload.Status
			}
			_, _ = writer.Write([]byte(`{}`))
		}),
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			_ = json.NewEncoder(writer).Encode([]shiftMigration{{ID: "op1", Status: "cancelled", Error: ""}})
		}))
	migration := ShiftMigration{
		Metadata: kubeMetadata{Name: "m1", Namespace: "default", UID: "u1", Generation: 1, Finalizers: []string{finalizer}},
		Spec:     migrationSpec{WorkloadID: "w1", SourceMachineID: "s1", DestinationMachineID: "d1"},
		Status:   map[string]any{"phase": "Running", "operationID": "op1"},
	}
	if err := controller.reconcileOne(context.Background(), &migration); err != nil {
		t.Fatal(err)
	}
	if status["phase"] != "Failed" {
		t.Fatalf("status = %#v", status)
	}
	reason, _ := status["reason"].(string)
	if reason == "" {
		t.Fatalf("status = %#v, want a non-empty reason for a cancelled migration", status)
	}
}

func TestNewConfigFromEnvRequiresHTTPS(t *testing.T) {
	base := map[string]string{
		"KUBERNETES_HOST":       "https://kube.example:6443",
		"KUBERNETES_TOKEN":      "k",
		"POD_NAMESPACE":         "default",
		"SHIFT_CONTROL_URL":     "https://control.example",
		"SHIFT_CONTROL_TOKEN":   "s",
		"SHIFT_ORGANIZATION_ID": "org",
		"KUBERNETES_CA_FILE":    "",
		"SHIFT_CA_FILE":         "",
		"SHIFT_K8S_LEADER_NAME": "",
	}
	applyEnv := func(overrides map[string]string) {
		for key, value := range base {
			t.Setenv(key, value)
		}
		for key, value := range overrides {
			t.Setenv(key, value)
		}
	}

	applyEnv(map[string]string{"KUBERNETES_HOST": "http://kube.example:8080"})
	if _, err := NewConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("err = %v, want an https requirement for KUBERNETES_HOST", err)
	}

	applyEnv(map[string]string{"SHIFT_CONTROL_URL": "http://control.example"})
	if _, err := NewConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("err = %v, want an https requirement for SHIFT_CONTROL_URL", err)
	}

	applyEnv(nil)
	if _, err := NewConfigFromEnv(); err != nil {
		t.Fatalf("https config must be accepted: %v", err)
	}
}

func TestNewShiftClientRejectsCleartext(t *testing.T) {
	if _, err := NewShiftClient(Config{ControlURL: "http://control.example"}); err == nil {
		t.Fatal("cleartext control-plane URL must be rejected")
	}
}

func TestNewKubeClientRejectsCleartext(t *testing.T) {
	if _, err := NewKubeClient(Config{KubernetesURL: "http://kube.example"}); err == nil {
		t.Fatal("cleartext API-server URL must be rejected")
	}
}

func TestMigrationJSONRoundTripKeepsTerminalDetection(t *testing.T) {
	migration := ShiftMigration{
		Metadata: kubeMetadata{Name: "m1", Generation: 4},
		Status:   map[string]any{"phase": "Failed", "observedGeneration": int64(4)},
	}
	decoded := migrationJSON(t, migration)
	status, _ := decoded["status"].(map[string]any)
	if observedGeneration(status) != 4 {
		t.Fatalf("observedGeneration = %d", observedGeneration(status))
	}
	if !migrationTerminal("cancelled") || migrationTerminal("running") {
		t.Fatal("terminal detection is wrong")
	}
}
