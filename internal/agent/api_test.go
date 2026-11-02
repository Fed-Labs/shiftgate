package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/network"
	"shift.dev/shift/internal/objectstore"
	linuxplatform "shift.dev/shift/internal/platform/linux"
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

// TestLocalAPICreateAdmitsNoClientIDs pins the create-side id contract: a
// client-supplied id that names an existing workload is refused outright —
// the record can never be overwritten — one that is not even well-formed is
// refused as a client error (it must never reach a log-path join), and a
// well-formed request is still stored under an agent-minted id.
func TestLocalAPICreateAdmitsNoClientIDs(t *testing.T) {
	service, victim := localAPIFixture(t)

	// Overwrite attempt: an id that names another user's record.
	attackerRoot := t.TempDir()
	attacker := model.WorkloadSpec{
		ID: victim.Spec.ID, Name: "attacker", Command: []string{"/bin/false"},
		RootPath: attackerRoot, WorkingDir: attackerRoot, UID: 4243, GID: 4243,
	}
	response := localCall(t, service, http.MethodPost, "/v1/workloads", attacker, 4243)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "WORKLOAD_ID_CONFLICT") {
		t.Fatalf("a create carrying an existing id must be refused with 409, got %d: %s", response.Code, response.Body.String())
	}

	// The victim's record is untouched.
	got := localCall(t, service, http.MethodGet, "/v1/workloads/"+victim.Spec.ID, nil, 4242)
	if got.Code != http.StatusOK {
		t.Fatalf("victim lookup status %d: %s", got.Code, got.Body.String())
	}
	var survivor model.Workload
	if err := json.Unmarshal(got.Body.Bytes(), &survivor); err != nil {
		t.Fatal(err)
	}
	if survivor.Spec.Name != "protected" || len(survivor.Spec.Command) == 0 || survivor.Spec.Command[0] != "/bin/true" {
		t.Fatalf("the victim record must survive a create-id collision: %+v", survivor.Spec)
	}

	// Path-traversal id: refused as a client error before any path join.
	traversalRoot := t.TempDir()
	traversal := model.WorkloadSpec{
		ID: "../../../../etc/shift-owned", Name: "traversal", Command: []string{"/bin/false"},
		RootPath: traversalRoot, WorkingDir: traversalRoot,
	}
	response = localCall(t, service, http.MethodPost, "/v1/workloads", traversal, 0)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "WORKLOAD_ID_INVALID") {
		t.Fatalf("a malformed id must be refused with 422, got %d: %s", response.Code, response.Body.String())
	}

	// A well-formed id is advisory at most: the stored id is minted here.
	requested, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	freshRoot := t.TempDir()
	fresh := model.WorkloadSpec{
		ID: requested, Name: "fresh-id", Command: []string{"/bin/false"},
		RootPath: freshRoot, WorkingDir: freshRoot,
	}
	response = localCall(t, service, http.MethodPost, "/v1/workloads", fresh, 0)
	if response.Code != http.StatusCreated {
		t.Fatalf("a well-formed requested id must not break the create, got %d: %s", response.Code, response.Body.String())
	}
	var created model.Workload
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Spec.ID == requested || !validWorkloadID(created.Spec.ID) {
		t.Fatalf("the agent must mint the stored id, got %q for request %q", created.Spec.ID, requested)
	}
}

// TestLocalAPIDeleteRefusedLeavesSideEffectsAlone pins the delete ordering:
// the guarded runtime delete runs first, and a refused delete must leave the
// published ports forwarding and the changed-file index intact — withdrawing
// either before the refusal silently degrades a running workload the API just
// refused to delete.
func TestLocalAPIDeleteRefusedLeavesSideEffectsAlone(t *testing.T) {
	configuration := config.DefaultAgent()
	configuration.StateDir = t.TempDir()
	configuration.Listen = "unix://" + configuration.StateDir + "/agent.sock"
	configuration.InsecureDevelopment = true
	service, err := Open(configuration, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	spec := model.WorkloadSpec{
		Name: "ported", Command: []string{"/bin/true"}, RootPath: root, WorkingDir: root,
		UID: os.Geteuid(), GID: os.Getegid(),
		Ports: []model.PortSpec{{Protocol: "tcp", ContainerPort: 8080, HostPort: hostPort}},
	}
	response := localCall(t, service, http.MethodPost, "/v1/workloads", spec, 0)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", response.Code, response.Body.String())
	}
	var created model.Workload
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created.Spec.ID

	mappings, err := network.MappingsFor(created.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.network.Prepare(id, mappings); err != nil {
		t.Fatal(err)
	}
	if err := service.network.Activate(context.Background(), id, mappings); err != nil {
		t.Fatal(err)
	}
	if ports := service.network.ForwardedPorts(id); len(ports) != 1 || ports[0] != hostPort {
		t.Fatalf("the published port must be forwarding before the delete, got %v", ports)
	}

	indexPath := filepath.Join(configuration.StateDir, "indices", id+".json")
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte(`{"baseline":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// A live child process makes the runtime delete refuse, the ordinary case.
	workload, err := service.runtime.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("/bin/sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	startTicks, err := linuxplatform.ProcessStartTicks(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	workload.Process = &model.ProcessState{PID: child.Process.Pid, PGID: child.Process.Pid, ProcStartTicks: startTicks, StartedAt: time.Now().UTC()}
	workload.Status = model.WorkloadRunning
	if err := service.runtime.RestoreMetadata(workload); err != nil {
		t.Fatal(err)
	}

	response = localCall(t, service, http.MethodDelete, "/v1/workloads/"+id, nil, 0)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "WORKLOAD_DELETE_FAILED") {
		t.Fatalf("delete of a running workload must refuse with 409, got %d: %s", response.Code, response.Body.String())
	}
	if ports := service.network.ForwardedPorts(id); len(ports) != 1 {
		t.Fatalf("a refused delete must leave the published ports in service, got %v", ports)
	}
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("a refused delete must keep the changed-file index: %v", err)
	}

	// With the process gone the delete succeeds — and only now the side
	// effects run.
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	response = localCall(t, service, http.MethodDelete, "/v1/workloads/"+id, nil, 0)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete status %d: %s", response.Code, response.Body.String())
	}
	if ports := service.network.ForwardedPorts(id); len(ports) != 0 {
		t.Fatalf("a completed delete must withdraw the published ports, got %v", ports)
	}
	if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
		t.Fatalf("a completed delete must discard the changed-file index, stat err %v", err)
	}
}

// TestLocalAPIStandbyDutyRedactsSecrets pins that duty reads never re-serve
// the control-plane ownership token or the fencing command's output.
func TestLocalAPIStandbyDutyRedactsSecrets(t *testing.T) {
	service, workload := localAPIFixture(t)
	now := time.Now().UTC()
	secret := "ownership-token-secret-123"
	duty := model.StandbyDuty{
		WorkloadID: workload.Spec.ID, WorkloadUID: 4242, State: model.StandbyArmed,
		SourceMachineID: "machine-source", OwnershipToken: secret,
		FenceResult: "fence-command-output-secret", FencingProvider: "self",
		HeldAt: now, LastCheckpointAt: now, UpdatedAt: now,
	}
	if err := service.standby.duties.Put(duty.WorkloadID, duty); err != nil {
		t.Fatal(err)
	}
	response := localCall(t, service, http.MethodGet, "/v1/standby", nil, 4242)
	if response.Code != http.StatusOK {
		t.Fatalf("standby list status %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Contains(body, secret) || strings.Contains(body, "fence-command-output-secret") {
		t.Fatalf("duty responses must not re-serve the ownership token or fence output: %s", body)
	}
	if !strings.Contains(body, workload.Spec.ID) {
		t.Fatalf("the duty itself must remain visible: %s", body)
	}
	projected := publicStandbyDuty(duty)
	if projected.OwnershipToken != "" || projected.FenceResult != "" {
		t.Fatalf("the duty projection must strip the secrets: %+v", projected)
	}
}

// TestLocalAPIMigrationReadsRedactRelayTicket pins that the one-use relay
// ticket is never re-served on migration reads — only the create request that
// carried it ever saw it.
func TestLocalAPIMigrationReadsRedactRelayTicket(t *testing.T) {
	service, workload := localAPIFixture(t)
	stamped, err := service.runtime.Get(workload.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	stamped.Status = model.WorkloadRunning
	if err := service.runtime.RestoreMetadata(stamped); err != nil {
		t.Fatal(err)
	}
	const ticket = "relay-ticket-secret-xyz"
	input := migrateRequest{
		WorkloadID: workload.Spec.ID,
		Destination: model.Destination{
			MachineID: "machine-destination", AgentURL: "https://dest.example:9443",
			Route: "relay", RelayEndpoint: "https://relay.example:443", RelayTicket: ticket,
		},
	}
	response := localCall(t, service, http.MethodPost, "/v1/migrations", input, 0)
	if response.Code != http.StatusAccepted {
		t.Fatalf("migration create status %d: %s", response.Code, response.Body.String())
	}
	var created model.Migration
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	// Let the (deliberately unconfigured relay) run settle so the record
	// reads below and the temp-dir cleanup are deterministic.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		record, getErr := service.migrations.Get(created.ID)
		if getErr == nil {
			switch record.Stage {
			case model.MigrationFailed, model.MigrationRolledBack, model.MigrationCancelled, model.MigrationCompleted: //nolint:misspell // persisted stage vocabulary
				deadline = time.Now()
			}
		}
		if time.Now().Before(deadline) {
			time.Sleep(25 * time.Millisecond)
		}
	}
	list := localCall(t, service, http.MethodGet, "/v1/migrations", nil, 0)
	if list.Code != http.StatusOK {
		t.Fatalf("migration list status %d: %s", list.Code, list.Body.String())
	}
	if strings.Contains(list.Body.String(), ticket) {
		t.Fatalf("migration list must not re-serve the relay ticket: %s", list.Body.String())
	}
	single := localCall(t, service, http.MethodGet, "/v1/migrations/"+created.ID, nil, 0)
	if single.Code != http.StatusOK {
		t.Fatalf("migration get status %d: %s", single.Code, single.Body.String())
	}
	if strings.Contains(single.Body.String(), ticket) {
		t.Fatalf("migration get must not re-serve the relay ticket: %s", single.Body.String())
	}
}

// TestLocalAPICreateNameConflictAndTimestamps pins the create-side name and
// time contract: names are refused with a conflict status, and created_at is
// stamped by the agent so a caller cannot skew the checkpoint schedule.
func TestLocalAPICreateNameConflictAndTimestamps(t *testing.T) {
	service, _ := localAPIFixture(t)
	root := t.TempDir()
	first := model.WorkloadSpec{
		Name: "dup-name", Command: []string{"/bin/true"}, RootPath: root, WorkingDir: root,
	}
	response := localCall(t, service, http.MethodPost, "/v1/workloads", first, 0)
	if response.Code != http.StatusCreated {
		t.Fatalf("first create status %d: %s", response.Code, response.Body.String())
	}
	other := t.TempDir()
	second := model.WorkloadSpec{
		Name: "DUP-NAME", Command: []string{"/bin/true"}, RootPath: other, WorkingDir: other,
	}
	response = localCall(t, service, http.MethodPost, "/v1/workloads", second, 0)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "WORKLOAD_NAME_CONFLICT") {
		t.Fatalf("a duplicate name must be refused with 409, got %d: %s", response.Code, response.Body.String())
	}
	past := t.TempDir()
	backdated := model.WorkloadSpec{
		Name: "backdated", Command: []string{"/bin/true"}, RootPath: past, WorkingDir: past,
		CreatedAt: time.Now().UTC().Add(-365 * 24 * time.Hour),
	}
	response = localCall(t, service, http.MethodPost, "/v1/workloads", backdated, 0)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", response.Code, response.Body.String())
	}
	var created model.Workload
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Spec.CreatedAt.Before(time.Now().UTC().Add(-time.Minute)) {
		t.Fatalf("created_at must be stamped server-side, got %s", created.Spec.CreatedAt)
	}
}

// TestLocalAPIInputContract pins the request-contract fixes: empty bodies are
// the documented zero request rather than an "EOF" decode failure, lookup
// misses carry their own codes, and absurd seconds values are refused before
// the duration conversion can wrap.
func TestLocalAPIInputContract(t *testing.T) {
	service, workload := localAPIFixture(t)

	// Named lookup failures.
	missing := localCall(t, service, http.MethodGet, "/v1/workloads/workload-missing", nil, 0)
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "WORKLOAD_NOT_FOUND") {
		t.Fatalf("missing workload must be 404 WORKLOAD_NOT_FOUND, got %d: %s", missing.Code, missing.Body.String())
	}
	missingRestore := localCall(t, service, http.MethodGet, "/v1/restores/restore-missing", nil, 0)
	if missingRestore.Code != http.StatusNotFound || !strings.Contains(missingRestore.Body.String(), "RESTORE_NOT_FOUND") {
		t.Fatalf("missing restore must be 404 RESTORE_NOT_FOUND, got %d: %s", missingRestore.Code, missingRestore.Body.String())
	}

	// An empty body is the zero request: the policy clears exactly as docs/api.md
	// describes, GC runs on its defaults, and a create names the missing field.
	install := localCall(t, service, http.MethodPost, "/v1/workloads/"+workload.Spec.ID+"/policy",
		policyRequest{IntervalSeconds: 60}, 0)
	if install.Code != http.StatusOK {
		t.Fatalf("policy install status %d: %s", install.Code, install.Body.String())
	}
	cleared := localCall(t, service, http.MethodPost, "/v1/workloads/"+workload.Spec.ID+"/policy", nil, 0)
	if cleared.Code != http.StatusOK {
		t.Fatalf("an empty body must clear the policy, got %d: %s", cleared.Code, cleared.Body.String())
	}
	var updated model.Workload
	if err := json.Unmarshal(cleared.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.CheckpointPolicy != nil {
		t.Fatalf("an empty body must remove the policy, got %+v", updated.Spec.CheckpointPolicy)
	}
	emptyCreate := localCall(t, service, http.MethodPost, "/v1/workloads", nil, 0)
	if emptyCreate.Code == http.StatusBadRequest && strings.Contains(emptyCreate.Body.String(), "EOF") {
		t.Fatalf("an empty body must not answer INVALID_JSON(EOF), got %d: %s", emptyCreate.Code, emptyCreate.Body.String())
	}
	if !strings.Contains(emptyCreate.Body.String(), "root path is required") {
		t.Fatalf("an empty create must name the missing field, got %d: %s", emptyCreate.Code, emptyCreate.Body.String())
	}
	emptyCheckpoint := localCall(t, service, http.MethodPost, "/v1/checkpoints", nil, 0)
	if emptyCheckpoint.Code != http.StatusBadRequest || !strings.Contains(emptyCheckpoint.Body.String(), "workload_id is required") {
		t.Fatalf("an empty checkpoint create must name the missing field, got %d: %s", emptyCheckpoint.Code, emptyCheckpoint.Body.String())
	}
	emptyMigration := localCall(t, service, http.MethodPost, "/v1/migrations", nil, 0)
	if emptyMigration.Code != http.StatusBadRequest || !strings.Contains(emptyMigration.Body.String(), "workload_id is required") {
		t.Fatalf("an empty migration create must name the missing field, got %d: %s", emptyMigration.Code, emptyMigration.Body.String())
	}
	gc := localCall(t, service, http.MethodPost, "/v1/gc", nil, 0)
	if gc.Code != http.StatusOK {
		t.Fatalf("an empty body must run GC on its defaults, got %d: %s", gc.Code, gc.Body.String())
	}

	// Absurd seconds values are refused, never wrapped into other deadlines.
	hugeStop := localCall(t, service, http.MethodPost, "/v1/workloads/"+workload.Spec.ID+"/stop",
		stopRequest{TimeoutSeconds: int(^uint(0) >> 1)}, 0)
	if hugeStop.Code != http.StatusBadRequest || !strings.Contains(hugeStop.Body.String(), "INVALID_ARGUMENT") {
		t.Fatalf("an absurd timeout must be refused, got %d: %s", hugeStop.Code, hugeStop.Body.String())
	}
	negativeCheckpoint := localCall(t, service, http.MethodPost, "/v1/checkpoints",
		checkpointRequest{WorkloadID: workload.Spec.ID, TimeoutSeconds: -1}, 0)
	if negativeCheckpoint.Code != http.StatusBadRequest || !strings.Contains(negativeCheckpoint.Body.String(), "INVALID_ARGUMENT") {
		t.Fatalf("a negative timeout must be refused, got %d: %s", negativeCheckpoint.Code, negativeCheckpoint.Body.String())
	}
	hugeGrace := localCall(t, service, http.MethodPost, "/v1/gc",
		gcRequest{GraceSeconds: int(^uint(0) >> 1), DryRun: true}, 0)
	if hugeGrace.Code != http.StatusBadRequest || !strings.Contains(hugeGrace.Body.String(), "INVALID_ARGUMENT") {
		t.Fatalf("an absurd grace must be refused, got %d: %s", hugeGrace.Code, hugeGrace.Body.String())
	}
}

// TestWorkloadLogsTailIsBounded pins the tail's byte cap: a caller asking for
// thousands of lines of megabyte-sized log lines gets a bounded response, and
// the tail keeps the newest content.
func TestWorkloadLogsTailIsBounded(t *testing.T) {
	service, workload := localAPIFixture(t)
	logPath := filepath.Join(workload.Spec.RootPath, ".shift", "logs", workload.Spec.ID+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	const lineCount = 12
	const lineSize = 1 << 20
	var log strings.Builder
	for i := 0; i < lineCount; i++ {
		log.WriteString("LINE-")
		log.WriteByte(byte('A' + i))
		log.WriteString("-")
		log.WriteString(strings.Repeat("x", lineSize))
		log.WriteByte('\n')
	}
	if err := os.WriteFile(logPath, []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	full := localCall(t, service, http.MethodGet, "/v1/workloads/"+workload.Spec.ID+"/logs", nil, 0)
	if full.Code != http.StatusOK || !strings.Contains(full.Body.String(), "LINE-A-") {
		t.Fatalf("the untailed read must stream the whole log, got %d", full.Code)
	}

	response := localCall(t, service, http.MethodGet, "/v1/workloads/"+workload.Spec.ID+"/logs?tail=5000", nil, 0)
	if response.Code != http.StatusOK {
		t.Fatalf("tail status %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if len(body) > maxLogTailBytes+64<<10 {
		t.Fatalf("the tail must be byte-bounded, got %d bytes", len(body))
	}
	if !strings.Contains(body, "LINE-L-") {
		t.Fatalf("the tail must keep the newest lines, got %d bytes", len(body))
	}
	if strings.Contains(body, "LINE-A-") {
		t.Fatalf("over-budget oldest lines must be dropped, got %d bytes", len(body))
	}

	lastTwo := localCall(t, service, http.MethodGet, "/v1/workloads/"+workload.Spec.ID+"/logs?tail=2", nil, 0)
	if lastTwo.Code != http.StatusOK {
		t.Fatalf("tail status %d: %s", lastTwo.Code, lastTwo.Body.String())
	}
	two := lastTwo.Body.String()
	if !strings.Contains(two, "LINE-K-") || !strings.Contains(two, "LINE-L-") || strings.Contains(two, "LINE-J-") {
		t.Fatalf("tail=2 must return exactly the last two lines, got %d bytes", len(two))
	}
}
