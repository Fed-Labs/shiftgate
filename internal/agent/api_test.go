package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/objectstore"
)

func TestLocalAPIWorkloadOwnership(t *testing.T) {
	configuration := config.DefaultAgent()
	configuration.StateDir = t.TempDir()
	configuration.Listen = "unix://" + configuration.StateDir + "/agent.sock"
	configuration.InsecureDevelopment = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := Open(configuration, logger)
	if err != nil {
		t.Fatal(err)
	}
	workloadRoot := t.TempDir()
	spec := model.WorkloadSpec{
		Name: "owned", Command: []string{"/bin/true"}, RootPath: workloadRoot,
		WorkingDir: workloadRoot, UID: os.Geteuid(), GID: os.Getegid(),
	}
	body, _ := json.Marshal(spec)
	ownerContext := context.WithValue(context.Background(), credentialsContextKey{}, peerCredentials{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())})
	request := httptest.NewRequestWithContext(ownerContext, http.MethodPost, "/v1/workloads", bytes.NewReader(body))
	response := httptest.NewRecorder()
	service.localHandler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", response.Code, response.Body.String())
	}
	var created model.Workload
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	otherContext := context.WithValue(context.Background(), credentialsContextKey{}, peerCredentials{UID: uint32(os.Geteuid() + 1), GID: uint32(os.Getegid() + 1)})
	unauthorized := httptest.NewRequestWithContext(otherContext, http.MethodGet, "/v1/workloads/"+created.Spec.ID, nil)
	unauthorizedResponse := httptest.NewRecorder()
	service.localHandler().ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-user access status %d: %s", unauthorizedResponse.Code, unauthorizedResponse.Body.String())
	}
}

func TestHealthRequiresLocalIdentity(t *testing.T) {
	configuration := config.DefaultAgent()
	configuration.StateDir = t.TempDir()
	configuration.Listen = "unix://" + configuration.StateDir + "/agent.sock"
	configuration.InsecureDevelopment = false
	service, err := Open(configuration, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/health", nil)
	response := httptest.NewRecorder()
	service.localHandler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous health status %d", response.Code)
	}
}

func TestLocalAPIRetriesCheckpointMirror(t *testing.T) {
	configuration := config.DefaultAgent()
	configuration.StateDir = t.TempDir()
	configuration.Listen = "unix://" + filepath.Join(configuration.StateDir, "agent.sock")
	configuration.InsecureDevelopment = true
	service, err := Open(configuration, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	manifest := model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		ID: "checkpoint-api", Kind: model.CheckpointFull, CreatedAt: time.Now().UTC(),
		Workload: model.WorkloadSpec{
			ID: "workload-api", Name: "api-mirror", RootPath: t.TempDir(),
			UID: os.Geteuid(), GID: os.Getegid(),
		},
		SourceIdentity: service.identity.Machine,
	}
	if err := checkpoint.SignManifest(&manifest, service.identity); err != nil {
		t.Fatal(err)
	}
	if err := service.repository.Save(manifest); err != nil {
		t.Fatal(err)
	}
	remote, err := objectstore.OpenLocal(filepath.Join(configuration.StateDir, "remote"))
	if err != nil {
		t.Fatal(err)
	}
	service.checkpoints.SetMirror(checkpoint.NewMirror(remote, service.chunks, service.repository))

	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/checkpoints/"+manifest.ID+"/mirror", nil)
	request = request.WithContext(context.WithValue(request.Context(), credentialsContextKey{}, peerCredentials{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}))
	response := httptest.NewRecorder()
	service.localHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("mirror status %d: %s", response.Code, response.Body.String())
	}
	var result checkpoint.MirrorResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.CheckpointID != manifest.ID || result.Objects != 1 {
		t.Fatalf("unexpected mirror result: %+v", result)
	}
	if _, err := remote.Head(context.Background(), "checkpoints/"+manifest.ID+"/manifest.enc.json"); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorCRIUMessage(t *testing.T) {
	healthy := model.CRIUCapabilities{Installed: true, Healthy: true, Version: "Version: 4.2"}
	if got := doctorCRIUMessage(healthy); got != "Version: 4.2" {
		t.Fatalf("healthy check must show the version, got %q", got)
	}
	failed := model.CRIUCapabilities{
		Installed: true,
		Version:   "Version: 4.2",
		Errors:    []string{"Error (criu.c:123): kernel doesn't support xxx\nsecond line"},
	}
	got := doctorCRIUMessage(failed)
	if strings.Contains(got, "\n") {
		t.Fatalf("failed check must collapse to one line, got %q", got)
	}
	if !strings.Contains(got, "kernel doesn't support xxx") || !strings.Contains(got, "second line") {
		t.Fatalf("failed check must name the kernel gap, got %q", got)
	}
	if !strings.HasPrefix(got, "Version: 4.2 — ") {
		t.Fatalf("failed check must lead with the version, got %q", got)
	}
	noVersion := model.CRIUCapabilities{Installed: true, Errors: []string{"exec: not found"}}
	if got := doctorCRIUMessage(noVersion); got != "exec: not found" {
		t.Fatalf("missing version must show only the error, got %q", got)
	}
}

// localCall drives one request against the local API as the given caller
// uid, the way the socket peer credentials arrive in production.
func localCall(t *testing.T, service *Service, method, requestPath string, input any, uid uint32) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request := httptest.NewRequestWithContext(context.Background(), method, requestPath, body)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request = request.WithContext(context.WithValue(request.Context(), credentialsContextKey{}, peerCredentials{UID: uid, GID: uid}))
	response := httptest.NewRecorder()
	service.localHandler().ServeHTTP(response, request)
	return response
}

// localAPIFixture opens a local-API service and creates one workload owned
// by uid 4242 — a uid no real caller of this test process has, so caller
// filtering is exercised with context credentials alone.
func localAPIFixture(t *testing.T) (*Service, model.Workload) {
	t.Helper()
	configuration := config.DefaultAgent()
	configuration.StateDir = t.TempDir()
	configuration.Listen = "unix://" + configuration.StateDir + "/agent.sock"
	configuration.InsecureDevelopment = true
	service, err := Open(configuration, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	spec := model.WorkloadSpec{
		Name: "protected", Command: []string{"/bin/true"}, RootPath: root, WorkingDir: root,
		UID: 4242, GID: 4242,
	}
	response := localCall(t, service, http.MethodPost, "/v1/workloads", spec, 0)
	if response.Code != http.StatusCreated {
		t.Fatalf("create workload status %d: %s", response.Code, response.Body.String())
	}
	var created model.Workload
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return service, created
}

// TestLocalAPIFailoverPolicy pins the policy pair at the API boundary: a
// failover policy is refused until a checkpoint schedule exists, then
// installs and clears through the workload's failover route, and a trigger
// against a duty that does not exist is a plain 404.
func TestLocalAPIFailoverPolicy(t *testing.T) {
	service, workload := localAPIFixture(t)
	failoverPath := "/v1/workloads/" + workload.Spec.ID + "/failover"

	// No checkpoint schedule yet: installing a standby is refused.
	response := localCall(t, service, http.MethodPost, failoverPath,
		failoverRequest{AgentURL: "https://standby.example:9443"}, 0)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "checkpoint policy") {
		t.Fatalf("install without a schedule must be refused with the reason, got %d: %s", response.Code, response.Body.String())
	}
	// An install needs a URL to install.
	response = localCall(t, service, http.MethodPost, failoverPath, failoverRequest{}, 0)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "agent_url is required") {
		t.Fatalf("an empty install must be refused, got %d: %s", response.Code, response.Body.String())
	}
	// The schedule, then the standby.
	response = localCall(t, service, http.MethodPost, "/v1/workloads/"+workload.Spec.ID+"/policy",
		policyRequest{IntervalSeconds: 60}, 0)
	if response.Code != http.StatusOK {
		t.Fatalf("install schedule status %d: %s", response.Code, response.Body.String())
	}
	response = localCall(t, service, http.MethodPost, failoverPath,
		failoverRequest{AgentURL: "https://standby.example:9443", MachineID: "machine-standby", KeepLast: 2}, 0)
	if response.Code != http.StatusOK {
		t.Fatalf("install failover policy status %d: %s", response.Code, response.Body.String())
	}
	var updated model.Workload
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.FailoverPolicy == nil || updated.Spec.FailoverPolicy.AgentURL != "https://standby.example:9443" ||
		updated.Spec.FailoverPolicy.MachineID != "machine-standby" || updated.Spec.FailoverPolicy.KeepLast != 2 {
		t.Fatalf("failover policy not carried on the workload: %+v", updated.Spec.FailoverPolicy)
	}
	if updated.Spec.CheckpointPolicy == nil || updated.Spec.CheckpointPolicy.IntervalSeconds != 60 {
		t.Fatalf("the checkpoint schedule must survive, got %+v", updated.Spec.CheckpointPolicy)
	}
	// Clearing it leaves the schedule in place.
	response = localCall(t, service, http.MethodPost, failoverPath, failoverRequest{Off: true}, 0)
	if response.Code != http.StatusOK {
		t.Fatalf("clear failover policy status %d: %s", response.Code, response.Body.String())
	}
	var cleared model.Workload
	if err := json.Unmarshal(response.Body.Bytes(), &cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.Spec.FailoverPolicy != nil || cleared.Spec.CheckpointPolicy == nil {
		t.Fatalf("clearing the standby must keep the schedule: %+v", cleared.Spec)
	}
	// A trigger against a duty that does not exist is a 404, not a restore.
	response = localCall(t, service, http.MethodPost, "/v1/standby/workload-missing/trigger", struct{}{}, 0)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "STANDBY_DUTY_NOT_FOUND") {
		t.Fatalf("trigger on an unknown duty must 404, got %d: %s", response.Code, response.Body.String())
	}
}

// TestLocalAPIStandbyVisibility pins who may see and trigger standby duties:
// root sees every duty, a caller sees the duties for their own workloads
// only, a trigger from anyone else is refused, and the owner's trigger runs
// — failing honestly here, because the duty holds no checkpoint.
func TestLocalAPIStandbyVisibility(t *testing.T) {
	service, workload := localAPIFixture(t)
	now := time.Now().UTC()
	owned := model.StandbyDuty{
		WorkloadID: workload.Spec.ID, WorkloadUID: 4242, State: model.StandbyArmed,
		SourceMachineID: "machine-source", HeldAt: now, LastCheckpointAt: now, UpdatedAt: now,
	}
	foreign := owned
	foreign.WorkloadID = "workload-foreign"
	foreign.WorkloadUID = 1000
	for _, record := range []model.StandbyDuty{owned, foreign} {
		if err := service.standby.duties.Put(record.WorkloadID, record); err != nil {
			t.Fatal(err)
		}
	}
	listDuties := func(uid uint32) standbyListResponse {
		response := localCall(t, service, http.MethodGet, "/v1/standby", nil, uid)
		if response.Code != http.StatusOK {
			t.Fatalf("standby list as %d status %d: %s", uid, response.Code, response.Body.String())
		}
		var status standbyListResponse
		if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	if status := listDuties(0); len(status.Duties) != 2 {
		t.Fatalf("root must see every duty, got %d", len(status.Duties))
	}
	if status := listDuties(4242); len(status.Duties) != 1 || status.Duties[0].WorkloadID != workload.Spec.ID {
		t.Fatalf("a caller must see only their own duties, got %+v", status.Duties)
	}
	if status := listDuties(0); status.AutomaticFailover {
		t.Fatal("an agent with no control plane must report automatic failover as impossible")
	}
	// Someone else's trigger is refused before anything runs.
	response := localCall(t, service, http.MethodPost, "/v1/standby/"+workload.Spec.ID+"/trigger", struct{}{}, 1000)
	if response.Code != http.StatusForbidden {
		t.Fatalf("a foreign trigger must be refused, got %d: %s", response.Code, response.Body.String())
	}
	// The owner's trigger runs and fails honestly: this duty holds no
	// checkpoint to restore.
	response = localCall(t, service, http.MethodPost, "/v1/standby/"+workload.Spec.ID+"/trigger", struct{}{}, 4242)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "no checkpoint") {
		t.Fatalf("the owner's trigger must fail with the honest reason, got %d: %s", response.Code, response.Body.String())
	}
}

// TestLocalAPIFailoverLedger pins the replication ledger's visibility: root
// sees every entry, a caller sees the entries for workloads they own, and an
// entry whose workload no longer exists is root's to see.
func TestLocalAPIFailoverLedger(t *testing.T) {
	service, workload := localAPIFixture(t)
	now := time.Now().UTC()
	entries := []model.ReplicationEntry{
		{WorkloadID: workload.Spec.ID, StandbyURL: "https://standby.example:9443", UpdatedAt: now},
		{WorkloadID: "workload-gone", StandbyURL: "https://old.example:9443", UpdatedAt: now},
	}
	for _, entry := range entries {
		if err := service.replicator.ledger.Put(entry.WorkloadID, entry); err != nil {
			t.Fatal(err)
		}
	}
	listEntries := func(uid uint32) []model.ReplicationEntry {
		response := localCall(t, service, http.MethodGet, "/v1/failover", nil, uid)
		if response.Code != http.StatusOK {
			t.Fatalf("ledger as %d status %d: %s", uid, response.Code, response.Body.String())
		}
		var listed []model.ReplicationEntry
		if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
			t.Fatal(err)
		}
		return listed
	}
	if listed := listEntries(0); len(listed) != 2 {
		t.Fatalf("root must see every ledger entry, got %d", len(listed))
	}
	if listed := listEntries(4242); len(listed) != 1 || listed[0].WorkloadID != workload.Spec.ID {
		t.Fatalf("a caller must see only their own entries, got %+v", listed)
	}
	if listed := listEntries(1000); len(listed) != 0 {
		t.Fatalf("a caller with no failover workloads sees nothing, got %+v", listed)
	}
}
