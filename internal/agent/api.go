package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/gc"
	"shift.dev/shift/internal/migration"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/network"
	"shift.dev/shift/internal/persistence"
)

const localBodyLimit = int64(4 << 20)

// maxRequestSeconds bounds every caller-supplied seconds-to-duration field
// (timeouts, grace periods): converting an absurd value would wrap the
// multiplication and silently become a different deadline than the caller
// asked for. Zero keeps its per-route meaning of "the default".
const maxRequestSeconds = 86400

const (
	// maxLogTailLines and maxLogTailBytes together bound what one logs
	// request may buffer: a tail of 5000 lines of multi-megabyte log lines
	// must not turn a small HTTP request into gigabytes of agent-side
	// allocation. Lines are dropped from the front as either cap is reached —
	// the end of the log is what a tail is for.
	maxLogTailLines = 5000
	maxLogTailBytes = 8 << 20
	maxLogLineBytes = 4 << 20
)

// Admission failures distinguishable from the plain authorization refusal so
// the create handler can answer with the contract's status and code for each.
var (
	errWorkloadIDInvalid    = errors.New("workload id is not valid")
	errWorkloadIDConflict   = errors.New("workload id already exists")
	errWorkloadNameConflict = errors.New("workload name already exists")
	errWorkloadRootInvalid  = errors.New("workload root is not valid")
)

type healthResponse struct {
	Status    string    `json:"status"`
	Version   string    `json:"version"`
	MachineID string    `json:"machine_id"`
	StartedAt time.Time `json:"started_at"`
	Uptime    string    `json:"uptime"`
}

type doctorCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

type doctorResponse struct {
	Healthy bool          `json:"healthy"`
	Checks  []doctorCheck `json:"checks"`
}

type stopRequest struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

type policyRequest struct {
	// IntervalSeconds of zero or less disables periodic checkpointing; any
	// positive value installs (or replaces) the schedule.
	IntervalSeconds int `json:"interval_seconds"`
	KeepLast        int `json:"keep_last,omitempty"`
}

type failoverRequest struct {
	// Off clears the policy: the replicator withdraws the duty from the
	// standby on its next pass and stops pushing new checkpoints. Anything
	// else installs (or replaces) a policy naming that standby.
	Off       bool   `json:"off,omitempty"`
	AgentURL  string `json:"agent_url,omitempty"`
	MachineID string `json:"machine_id,omitempty"`
	KeepLast  int    `json:"keep_last,omitempty"`
}

type standbyTriggerRequest struct {
	// CheckpointID optionally restores a specific replicated checkpoint; the
	// default is the duty's newest.
	CheckpointID string `json:"checkpoint_id,omitempty"`
	// Lazy starts the failed-over process before its memory is fully loaded,
	// serving pages on demand — the fastest possible recovery, opt-in because
	// a half-served workload dies if its lazy-pages daemon does. The
	// automatic path never asks for this; only an operator can.
	Lazy bool `json:"lazy,omitempty"`
}

type standbyTriggerResponse struct {
	Duty model.StandbyDuty `json:"duty"`
	// SourceReachable is what the pre-trigger probe observed: "yes", "no", or
	// "unknown" (no URL on the duty, or the probe itself could not run).
	SourceReachable string `json:"source_reachable"`
	Warning         string `json:"warning,omitempty"`
}

type standbyListResponse struct {
	Duties []model.StandbyDuty `json:"duties"`
	// AutomaticFailover reports whether this agent can confirm a source's
	// death on its own — a control plane is configured. When false, armed
	// duties never fail over automatically: only an explicit trigger acts.
	AutomaticFailover bool `json:"automatic_failover"`
}

type checkpointRequest struct {
	WorkloadID     string               `json:"workload_id"`
	Kind           model.CheckpointKind `json:"kind,omitempty"`
	ParentID       string               `json:"parent_id,omitempty"`
	LeaveRunning   *bool                `json:"leave_running,omitempty"`
	TCPState       bool                 `json:"tcp_state,omitempty"`
	TimeoutSeconds int                  `json:"timeout_seconds,omitempty"`
}

type restoreRequest struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Lazy starts the restored process before its memory is fully loaded,
	// serving pages on demand through userfaultfd. Opt-in: an eager restore
	// is the predictable default.
	Lazy bool `json:"lazy,omitempty"`
}

type forkRequest struct {
	Name           string `json:"name,omitempty"`
	RootPath       string `json:"root_path,omitempty"`
	CheckpointID   string `json:"checkpoint_id,omitempty"`
	Activate       bool   `json:"activate,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type cloneRequest struct {
	Count          int    `json:"count,omitempty"`
	NamePrefix     string `json:"name_prefix,omitempty"`
	Parallel       int    `json:"parallel,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type gcRequest struct {
	GraceSeconds int  `json:"grace_seconds,omitempty"`
	DryRun       bool `json:"dry_run"`
}

type gcResponse struct {
	Plan    gc.Plan `json:"plan"`
	Deleted int     `json:"deleted"`
	Skipped int     `json:"skipped"`
}

type migrateRequest struct {
	WorkloadID     string              `json:"workload_id"`
	IdempotencyKey string              `json:"idempotency_key,omitempty"`
	Destination    model.Destination   `json:"destination"`
	Mode           model.MigrationMode `json:"mode,omitempty"`
	PreCopyPasses  int                 `json:"pre_copy_passes,omitempty"`
	TimeoutSeconds int                 `json:"timeout_seconds,omitempty"`
}

func (s *Service) localHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/machine", s.handleMachine)
	mux.HandleFunc("GET /v1/identity", s.handleIdentity)
	mux.HandleFunc("GET /v1/doctor", s.handleDoctor)
	mux.HandleFunc("POST /v1/gc", s.handleGC)
	mux.Handle("GET /metrics", s.metrics.Handler())
	mux.HandleFunc("GET /v1/workloads", s.handleWorkloadList)
	mux.HandleFunc("POST /v1/workloads", s.handleWorkloadCreate)
	mux.HandleFunc("GET /v1/workloads/{id}", s.handleWorkloadGet)
	mux.HandleFunc("DELETE /v1/workloads/{id}", s.handleWorkloadDelete)
	mux.HandleFunc("POST /v1/workloads/{id}/start", s.handleWorkloadStart)
	mux.HandleFunc("POST /v1/workloads/{id}/pause", s.handleWorkloadPause)
	mux.HandleFunc("POST /v1/workloads/{id}/resume", s.handleWorkloadResume)
	mux.HandleFunc("POST /v1/workloads/{id}/stop", s.handleWorkloadStop)
	mux.HandleFunc("POST /v1/workloads/{id}/policy", s.handleWorkloadPolicy)
	mux.HandleFunc("POST /v1/workloads/{id}/failover", s.handleWorkloadFailover)
	mux.HandleFunc("GET /v1/workloads/{id}/logs", s.handleWorkloadLogs)
	mux.HandleFunc("GET /v1/checkpoints", s.handleCheckpointList)
	mux.HandleFunc("POST /v1/checkpoints", s.handleCheckpointCreate)
	mux.HandleFunc("GET /v1/checkpoints/{id}", s.handleCheckpointGet)
	mux.HandleFunc("POST /v1/checkpoints/{id}/mirror", s.handleCheckpointMirror)
	mux.HandleFunc("POST /v1/checkpoints/{id}/restore", s.handleCheckpointRestore)
	mux.HandleFunc("GET /v1/restores", s.handleRestoreList)
	mux.HandleFunc("GET /v1/restores/{id}", s.handleRestoreGet)
	mux.HandleFunc("POST /v1/workloads/{id}/fork", s.handleWorkloadFork)
	mux.HandleFunc("POST /v1/checkpoints/{id}/fork", s.handleCheckpointFork)
	mux.HandleFunc("GET /v1/forks", s.handleForkList)
	mux.HandleFunc("GET /v1/forks/{id}", s.handleForkGet)
	mux.HandleFunc("POST /v1/checkpoints/{id}/clone", s.handleCheckpointClone)
	mux.HandleFunc("GET /v1/clones", s.handleCloneList)
	mux.HandleFunc("GET /v1/clones/{id}", s.handleCloneGet)
	mux.HandleFunc("POST /v1/clones/{id}/rollback", s.handleCloneRollback)
	mux.HandleFunc("GET /v1/migrations", s.handleMigrationList)
	mux.HandleFunc("POST /v1/migrations", s.handleMigrationCreate)
	mux.HandleFunc("POST /v1/migrations/preflight", s.handleMigrationPreflight)
	mux.HandleFunc("GET /v1/migrations/{id}", s.handleMigrationGet)
	mux.HandleFunc("POST /v1/migrations/{id}/cancel", s.handleMigrationCancel)
	mux.HandleFunc("GET /v1/standby", s.handleStandbyList)
	mux.HandleFunc("POST /v1/standby/{workload}/trigger", s.handleStandbyTrigger)
	mux.HandleFunc("GET /v1/failover", s.handleFailoverList)
	mux.HandleFunc("GET /v1/updates", s.handleUpdateStatus)
	mux.HandleFunc("POST /v1/updates/check", s.handleUpdateCheck)
	mux.HandleFunc("POST /v1/updates/apply", s.handleUpdateApply)
	mux.HandleFunc("POST /v1/updates/rollback", s.handleUpdateRollback)
	mux.HandleFunc("POST /v1/updates/block", s.handleUpdateBlock)
	mux.HandleFunc("POST /v1/updates/unblock", s.handleUpdateUnblock)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, ok := s.caller(request); !ok {
			writeAPIError(writer, http.StatusUnauthorized, "LOCAL_AUTHENTICATION_FAILED", "local peer credentials are required")
			return
		}
		mux.ServeHTTP(writer, request)
	})
}

func (s *Service) caller(request *http.Request) (peerCredentials, bool) {
	if credentials, ok := requestCredentials(request.Context()); ok {
		return credentials, true
	}
	if s.config.InsecureDevelopment {
		return peerCredentials{PID: int32(os.Getpid()), UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}, true
	}
	return peerCredentials{}, false
}

func (s *Service) handleHealth(writer http.ResponseWriter, _ *http.Request) {
	writeAPIJSON(writer, http.StatusOK, healthResponse{
		Status: "ok", Version: config.Version, MachineID: s.identity.Machine.ID,
		StartedAt: s.startedAt, Uptime: time.Since(s.startedAt).Round(time.Second).String(),
	})
}

func (s *Service) handleMachine(writer http.ResponseWriter, request *http.Request) {
	capabilities, err := s.inventory.Inspect(request.Context())
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "INVENTORY_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusOK, capabilities)
}

func (s *Service) handleIdentity(writer http.ResponseWriter, _ *http.Request) {
	writeAPIJSON(writer, http.StatusOK, s.identity.Machine)
}

// doctorCRIUMessage renders the CRIU check's message. A healthy check shows
// the version; a failed one must show why — criu check's own output, which
// names the missing kernel capability — or the failure is undiagnosable from
// the doctor table alone.
func doctorCRIUMessage(criu model.CRIUCapabilities) string {
	if criu.Healthy || len(criu.Errors) == 0 {
		return criu.Version
	}
	detail := strings.Join(strings.Fields(strings.Join(criu.Errors, "; ")), " ")
	if criu.Version == "" {
		return detail
	}
	return criu.Version + " — " + detail
}

func (s *Service) handleDoctor(writer http.ResponseWriter, request *http.Request) {
	capabilities, err := s.inventory.Inspect(request.Context())
	checks := []doctorCheck{
		{Name: "platform", OK: model.CurrentPlatformSupported(), Message: capabilities.OS + "/" + capabilities.Architecture},
		{Name: "privileges", OK: os.Geteuid() == 0, Message: "agent effective uid " + strconv.Itoa(os.Geteuid())},
		{Name: "criu", OK: capabilities.CRIU.Installed && capabilities.CRIU.Healthy, Message: doctorCRIUMessage(capabilities.CRIU)},
		{Name: "cgroup_v2", OK: capabilities.Features["cgroup_v2"], Message: "cgroup v2 workload isolation"},
		{Name: "archive", OK: capabilities.Features["gnu_tar"], Message: "GNU tar with metadata support"},
	}
	if err != nil {
		checks = append(checks, doctorCheck{Name: "inventory", OK: false, Message: err.Error()})
	}
	if s.config.RemoteListen != "" {
		checks = append(checks, doctorCheck{Name: "peer_listener_tls", OK: s.config.TLS.CertificateFile != "" && s.config.TLS.PrivateKeyFile != "", Message: s.config.RemoteListen})
	}
	healthy := true
	for _, check := range checks {
		healthy = healthy && check.OK
	}
	writeAPIJSON(writer, http.StatusOK, doctorResponse{Healthy: healthy, Checks: checks})
}

func (s *Service) handleWorkloadList(writer http.ResponseWriter, request *http.Request) {
	caller, _ := s.caller(request)
	values := s.runtime.List()
	filtered := values[:0]
	for _, workload := range values {
		if caller.UID == 0 || workload.Spec.UID == int(caller.UID) {
			filtered = append(filtered, workload)
		}
	}
	writeAPIJSON(writer, http.StatusOK, filtered)
}

func (s *Service) handleWorkloadCreate(writer http.ResponseWriter, request *http.Request) {
	var spec model.WorkloadSpec
	if !decodeAPIJSON(writer, request, &spec) {
		return
	}
	caller, _ := s.caller(request)
	if err := s.authorizeNewSpec(&spec, caller); err != nil {
		status, code := http.StatusForbidden, "WORKLOAD_FORBIDDEN"
		switch {
		case errors.Is(err, errWorkloadIDConflict):
			status, code = http.StatusConflict, "WORKLOAD_ID_CONFLICT"
		case errors.Is(err, errWorkloadNameConflict):
			status, code = http.StatusConflict, "WORKLOAD_NAME_CONFLICT"
		case errors.Is(err, errWorkloadIDInvalid):
			status, code = http.StatusUnprocessableEntity, "WORKLOAD_ID_INVALID"
		case errors.Is(err, errWorkloadRootInvalid):
			status, code = http.StatusUnprocessableEntity, "WORKLOAD_ROOT_INVALID"
		}
		writeAPIError(writer, status, code, err.Error())
		return
	}
	// The record's timestamp is the agent's, never the caller's: the policy
	// scheduler measures intervals from it, so a caller-set created_at could
	// force an immediate periodic checkpoint or suppress scheduling.
	spec.CreatedAt = time.Now().UTC()
	workload, err := s.runtime.Create(spec)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "WORKLOAD_CREATE_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusCreated, workload)
}

func (s *Service) handleWorkloadGet(writer http.ResponseWriter, request *http.Request) {
	workload, ok := s.authorizedWorkload(writer, request, request.PathValue("id"))
	if !ok {
		return
	}
	writeAPIJSON(writer, http.StatusOK, workload)
}

func (s *Service) handleWorkloadDelete(writer http.ResponseWriter, request *http.Request) {
	workload, ok := s.authorizedWorkload(writer, request, request.PathValue("id"))
	if !ok {
		return
	}
	// The guarded delete runs first: it refuses while the workload's process
	// is alive, and a refused delete must leave everything in service. Only
	// once the record is gone does deleting a workload withdraw its network
	// presence — forwarders drain and the host-port reservations go back, so
	// a later workload can claim the ports — and discard its changed-file
	// index. Doing either before the delete would silently take a running
	// workload's published ports out of service on a request the API itself
	// answers with a refusal.
	if err := s.runtime.Delete(workload.Spec.ID); err != nil {
		writeAPIError(writer, http.StatusConflict, "WORKLOAD_DELETE_FAILED", err.Error())
		return
	}
	s.network.Deactivate(workload.Spec.ID, network.DefaultDrainGrace)
	s.checkpoints.DiscardIndex(workload.Spec.ID)
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleWorkloadStart(writer http.ResponseWriter, request *http.Request) {
	s.workloadAction(writer, request, func(id string) (model.Workload, error) {
		workload, err := s.runtime.Start(id)
		if err != nil {
			return workload, err
		}
		// A started workload with published ports gets its forwarders
		// back: the process binds its own listeners, and SHIFT carries
		// the host-port traffic to them.
		s.publishWorkloadPorts(request.Context(), workload)
		return workload, nil
	})
}

// publishWorkloadPorts reserves and forwards the ports a workload declares.
// Failures are logged, not fatal: the workload itself is running, and the
// operator can see the un-forwarded ports in the workload's status.
func (s *Service) publishWorkloadPorts(ctx context.Context, workload model.Workload) {
	if s.network == nil || len(workload.Spec.Ports) == 0 {
		return
	}
	mappings, err := network.MappingsFor(workload.Spec)
	if err != nil {
		s.logger.Warn("workload ports could not be resolved", "workload", workload.Spec.ID, "error", err)
		return
	}
	if err := s.network.Prepare(workload.Spec.ID, mappings); err != nil {
		s.logger.Warn("workload ports could not be reserved", "workload", workload.Spec.ID, "error", err)
		return
	}
	if err := s.network.Activate(ctx, workload.Spec.ID, mappings); err != nil {
		s.logger.Warn("workload ports could not be forwarded", "workload", workload.Spec.ID, "error", err)
	}
}

func (s *Service) handleWorkloadPause(writer http.ResponseWriter, request *http.Request) {
	s.workloadAction(writer, request, func(id string) (model.Workload, error) { return s.runtime.Pause(id) })
}

func (s *Service) handleWorkloadResume(writer http.ResponseWriter, request *http.Request) {
	s.workloadAction(writer, request, func(id string) (model.Workload, error) { return s.runtime.Resume(id) })
}

// handleWorkloadPolicy installs, replaces, or clears a workload's periodic
// checkpoint policy. The change takes effect on the scheduler's next pass
// and persists across agent restarts.
func (s *Service) handleWorkloadPolicy(writer http.ResponseWriter, request *http.Request) {
	workload, ok := s.authorizedWorkload(writer, request, request.PathValue("id"))
	if !ok {
		return
	}
	var input policyRequest
	if !decodeAPIJSON(writer, request, &input) {
		return
	}
	var policy *model.CheckpointPolicySpec
	if input.IntervalSeconds > 0 {
		policy = &model.CheckpointPolicySpec{IntervalSeconds: input.IntervalSeconds, KeepLast: input.KeepLast}
	}
	updated, err := s.runtime.SetCheckpointPolicy(workload.Spec.ID, policy)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, "POLICY_INVALID", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusOK, updated)
}

// handleWorkloadFailover installs or clears a workload's warm-standby policy.
// Installing one requires a checkpoint policy to exist first — the runtime
// setter enforces the pair, because replication carries the checkpoints a
// schedule produces and without one nothing would ever reach the standby.
func (s *Service) handleWorkloadFailover(writer http.ResponseWriter, request *http.Request) {
	workload, ok := s.authorizedWorkload(writer, request, request.PathValue("id"))
	if !ok {
		return
	}
	var input failoverRequest
	if !decodeAPIJSON(writer, request, &input) {
		return
	}
	var policy *model.FailoverPolicySpec
	if !input.Off {
		if input.AgentURL == "" {
			writeAPIError(writer, http.StatusBadRequest, "FAILOVER_POLICY_INVALID", "agent_url is required unless off is true")
			return
		}
		policy = &model.FailoverPolicySpec{
			AgentURL: input.AgentURL, MachineID: input.MachineID, KeepLast: input.KeepLast,
		}
	}
	updated, err := s.runtime.SetFailoverPolicy(workload.Spec.ID, policy)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, "FAILOVER_POLICY_INVALID", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusOK, updated)
}

func (s *Service) handleWorkloadStop(writer http.ResponseWriter, request *http.Request) {
	workload, ok := s.authorizedWorkload(writer, request, request.PathValue("id"))
	if !ok {
		return
	}
	var input stopRequest
	if request.ContentLength != 0 && !decodeAPIJSON(writer, request, &input) {
		return
	}
	// The stop grace is the caller's, clamped to the documented 60s limit:
	// a request above the limit gets the limit (not the default — silently
	// replacing an operator's 120s with 10s was the bug), and the effective
	// value is surfaced with the response. Absurd values are refused before
	// the conversion can wrap into a different timeout entirely.
	requested, ok := requestDuration(writer, input.TimeoutSeconds, "timeout_seconds")
	if !ok {
		return
	}
	timeout := requested
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if timeout > time.Minute {
		timeout = time.Minute
	}
	writer.Header().Set("X-Shift-Stop-Timeout-Seconds", strconv.Itoa(int(timeout.Seconds())))
	result, err := s.runtime.Stop(workload.Spec.ID, timeout)
	if err != nil {
		writeAPIError(writer, http.StatusConflict, "WORKLOAD_STOP_FAILED", err.Error())
		return
	}
	// A stopped workload has no listeners behind its forwarders; withdraw
	// them so in-flight connections finish and the ports are not held by a
	// process that can no longer answer.
	s.network.Deactivate(workload.Spec.ID, network.DefaultDrainGrace)
	writeAPIJSON(writer, http.StatusOK, result)
}

func (s *Service) handleWorkloadLogs(writer http.ResponseWriter, request *http.Request) {
	workload, ok := s.authorizedWorkload(writer, request, request.PathValue("id"))
	if !ok {
		return
	}
	tail, _ := strconv.Atoi(request.URL.Query().Get("tail"))
	if tail > maxLogTailLines {
		tail = maxLogTailLines
	}
	reader, err := s.runtime.LogReader(workload.Spec.ID, 0)
	if err != nil {
		writeAPIError(writer, http.StatusNotFound, "LOGS_NOT_FOUND", err.Error())
		return
	}
	defer func() { _ = reader.Close() }()
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if tail <= 0 {
		_, _ = io.Copy(writer, reader)
		return
	}
	_, _ = io.WriteString(writer, logTail(reader, tail, maxLogTailBytes))
}

// logTail returns the trailing lines of a log, bounded both in lines and in
// bytes. The workload's log content is controlled by whichever local user
// owns the workload, so an unbounded tail — 5000 lines of 4 MiB each — is a
// one-request memory denial of service against the agent. As either cap is
// reached the oldest lines are dropped; a line the scanner cannot hold in one
// piece ends the scan and the collected tail is returned as-is.
func logTail(reader io.Reader, maxLines, maxBytes int) string {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxLogLineBytes)
	lines := make([][]byte, 0, 16)
	total := 0
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		lines = append(lines, line)
		total += len(line) + 1
		for len(lines) > 0 && (len(lines) > maxLines || (total > maxBytes && len(lines) > 1)) {
			total -= len(lines[0]) + 1
			lines = lines[1:]
		}
	}
	var out strings.Builder
	for _, line := range lines {
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.String()
}

func (s *Service) workloadAction(writer http.ResponseWriter, request *http.Request, action func(string) (model.Workload, error)) {
	workload, ok := s.authorizedWorkload(writer, request, request.PathValue("id"))
	if !ok {
		return
	}
	result, err := action(workload.Spec.ID)
	if err != nil {
		writeAPIError(writer, http.StatusConflict, "WORKLOAD_ACTION_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusOK, result)
}

func (s *Service) handleCheckpointList(writer http.ResponseWriter, request *http.Request) {
	caller, _ := s.caller(request)
	workloadID := request.URL.Query().Get("workload_id")
	values := s.checkpoints.List(workloadID)
	filtered := values[:0]
	for _, summary := range values {
		if caller.UID == 0 || s.workloadOwnedBy(summary.WorkloadID, caller.UID) {
			filtered = append(filtered, summary)
		}
	}
	writeAPIJSON(writer, http.StatusOK, filtered)
}

func (s *Service) handleCheckpointCreate(writer http.ResponseWriter, request *http.Request) {
	var input checkpointRequest
	if !decodeAPIJSON(writer, request, &input) {
		return
	}
	if strings.TrimSpace(input.WorkloadID) == "" {
		writeAPIError(writer, http.StatusBadRequest, "CHECKPOINT_REQUEST_INVALID", "workload_id is required")
		return
	}
	workload, ok := s.authorizedWorkload(writer, request, input.WorkloadID)
	if !ok {
		return
	}
	leaveRunning := true
	if input.LeaveRunning != nil {
		leaveRunning = *input.LeaveRunning
	}
	timeout, ok := requestDuration(writer, input.TimeoutSeconds, "timeout_seconds")
	if !ok {
		return
	}
	manifest, err := s.checkpoints.Create(request.Context(), workload.Spec.ID, checkpoint.CreateOptions{
		Kind: input.Kind, ParentID: input.ParentID, LeaveRunning: leaveRunning,
		TCPState: input.TCPState, Timeout: timeout,
	})
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, "CHECKPOINT_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusCreated, manifest)
}

func (s *Service) handleCheckpointGet(writer http.ResponseWriter, request *http.Request) {
	manifest, err := s.checkpoints.Load(request.PathValue("id"))
	if err != nil {
		writeAPIError(writer, http.StatusNotFound, "CHECKPOINT_NOT_FOUND", err.Error())
		return
	}
	if !s.authorizeUID(writer, request, manifest.Workload.UID) {
		return
	}
	writeAPIJSON(writer, http.StatusOK, manifest)
}

func (s *Service) handleCheckpointMirror(writer http.ResponseWriter, request *http.Request) {
	manifest, err := s.checkpoints.Load(request.PathValue("id"))
	if err != nil {
		writeAPIError(writer, http.StatusNotFound, "CHECKPOINT_NOT_FOUND", err.Error())
		return
	}
	if !s.authorizeUID(writer, request, manifest.Workload.UID) {
		return
	}
	result, err := s.checkpoints.MirrorCheckpoint(request.Context(), manifest.ID)
	if err != nil {
		if errors.Is(err, checkpoint.ErrMirrorDisabled) {
			writeAPIError(writer, http.StatusConflict, "CHECKPOINT_MIRROR_DISABLED", err.Error())
			return
		}
		writeAPIError(writer, http.StatusBadGateway, "CHECKPOINT_MIRROR_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusOK, result)
}

func (s *Service) handleCheckpointRestore(writer http.ResponseWriter, request *http.Request) {
	manifest, err := s.checkpoints.Load(request.PathValue("id"))
	if err != nil {
		writeAPIError(writer, http.StatusNotFound, "CHECKPOINT_NOT_FOUND", err.Error())
		return
	}
	if !s.authorizeUID(writer, request, manifest.Workload.UID) {
		return
	}
	var input restoreRequest
	if request.ContentLength != 0 && !decodeAPIJSON(writer, request, &input) {
		return
	}
	timeout, ok := requestDuration(writer, input.TimeoutSeconds, "timeout_seconds")
	if !ok {
		return
	}
	record, err := s.restorer.Prepare(request.Context(), manifest.ID, checkpoint.PrepareOptions{
		Timeout: timeout,
		Lazy:    input.Lazy,
	})
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, "RESTORE_FAILED", err.Error())
		return
	}
	committed, err := s.restorer.Commit(record.ID)
	if err != nil {
		_, _ = s.restorer.Rollback(contextWithoutCancellation(request), record.ID, "standalone restore commit failed")
		writeAPIError(writer, http.StatusInternalServerError, "RESTORE_COMMIT_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusOK, committed)
}

func (s *Service) handleRestoreList(writer http.ResponseWriter, request *http.Request) {
	caller, _ := s.caller(request)
	values := s.restorer.List()
	filtered := values[:0]
	for _, value := range values {
		if caller.UID == 0 || s.workloadOwnedBy(value.WorkloadID, caller.UID) {
			filtered = append(filtered, value)
		}
	}
	writeAPIJSON(writer, http.StatusOK, filtered)
}

func (s *Service) handleRestoreGet(writer http.ResponseWriter, request *http.Request) {
	record, err := s.restorer.Get(request.PathValue("id"))
	if err != nil {
		status, code := http.StatusInternalServerError, "RESTORE_LOOKUP_FAILED"
		if errors.Is(err, persistence.ErrNotFound) {
			status, code = http.StatusNotFound, "RESTORE_NOT_FOUND"
		}
		writeAPIError(writer, status, code, err.Error())
		return
	}
	workload, ok := s.authorizedWorkload(writer, request, record.WorkloadID)
	if !ok {
		return
	}
	_ = workload
	writeAPIJSON(writer, http.StatusOK, record)
}

// handleWorkloadFork derives an independent workload from the source workload's
// current or latest state. The caller must be authorized for the source; the
// fork inherits the source's UID, so no privilege is gained by forking.
func (s *Service) handleWorkloadFork(writer http.ResponseWriter, request *http.Request) {
	workload, ok := s.authorizedWorkload(writer, request, request.PathValue("id"))
	if !ok {
		return
	}
	var input forkRequest
	if request.ContentLength != 0 && !decodeAPIJSON(writer, request, &input) {
		return
	}
	s.fork(writer, request, workload.Spec.ID, input)
}

// handleCheckpointFork forks from one explicit checkpoint. It is the same
// operation as forking a workload with checkpoint_id supplied, exposed on the
// checkpoint so a caller holding a checkpoint id does not need the workload id.
func (s *Service) handleCheckpointFork(writer http.ResponseWriter, request *http.Request) {
	manifest, err := s.checkpoints.Load(request.PathValue("id"))
	if err != nil {
		writeAPIError(writer, http.StatusNotFound, "CHECKPOINT_NOT_FOUND", err.Error())
		return
	}
	if !s.authorizeUID(writer, request, manifest.Workload.UID) {
		return
	}
	var input forkRequest
	if request.ContentLength != 0 && !decodeAPIJSON(writer, request, &input) {
		return
	}
	input.CheckpointID = manifest.ID
	s.fork(writer, request, manifest.Workload.ID, input)
}

func (s *Service) fork(writer http.ResponseWriter, request *http.Request, sourceWorkloadID string, input forkRequest) {
	timeout, ok := requestDuration(writer, input.TimeoutSeconds, "timeout_seconds")
	if !ok {
		return
	}
	record, err := s.forker.Fork(request.Context(), sourceWorkloadID, checkpoint.ForkOptions{
		Name: input.Name, RootPath: input.RootPath, CheckpointID: input.CheckpointID,
		Activate: input.Activate, Timeout: timeout,
	})
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, "FORK_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusCreated, record)
}

func (s *Service) handleForkList(writer http.ResponseWriter, request *http.Request) {
	caller, _ := s.caller(request)
	values := s.forker.List()
	filtered := values[:0]
	for _, value := range values {
		if caller.UID == 0 || s.workloadOwnedBy(value.SourceWorkloadID, caller.UID) {
			filtered = append(filtered, value)
		}
	}
	writeAPIJSON(writer, http.StatusOK, filtered)
}

func (s *Service) handleForkGet(writer http.ResponseWriter, request *http.Request) {
	record, err := s.forker.Get(request.PathValue("id"))
	if err != nil {
		writeAPIError(writer, http.StatusNotFound, "FORK_NOT_FOUND", err.Error())
		return
	}
	if _, ok := s.authorizedWorkload(writer, request, record.SourceWorkloadID); !ok {
		return
	}
	writeAPIJSON(writer, http.StatusOK, record)
}

// handleCheckpointClone derives a set of independent running workloads from
// one checkpoint, all on this machine. The caller must be authorized for the
// checkpointed workload; every clone inherits its UID, so no privilege is
// gained by cloning.
func (s *Service) handleCheckpointClone(writer http.ResponseWriter, request *http.Request) {
	manifest, err := s.checkpoints.Load(request.PathValue("id"))
	if err != nil {
		writeAPIError(writer, http.StatusNotFound, "CHECKPOINT_NOT_FOUND", err.Error())
		return
	}
	if !s.authorizeUID(writer, request, manifest.Workload.UID) {
		return
	}
	var input cloneRequest
	if request.ContentLength != 0 && !decodeAPIJSON(writer, request, &input) {
		return
	}
	timeout, ok := requestDuration(writer, input.TimeoutSeconds, "timeout_seconds")
	if !ok {
		return
	}
	record, err := s.cloner.Clone(request.Context(), manifest.ID, checkpoint.CloneOptions{
		Count: input.Count, NamePrefix: input.NamePrefix, Parallel: input.Parallel,
		Timeout: timeout,
	})
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, "CLONE_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusCreated, record)
}

func (s *Service) handleCloneList(writer http.ResponseWriter, request *http.Request) {
	caller, _ := s.caller(request)
	values := s.cloner.List()
	filtered := values[:0]
	for _, value := range values {
		if caller.UID == 0 || s.workloadOwnedBy(value.SourceWorkloadID, caller.UID) {
			filtered = append(filtered, value)
		}
	}
	writeAPIJSON(writer, http.StatusOK, filtered)
}

func (s *Service) handleCloneGet(writer http.ResponseWriter, request *http.Request) {
	record, err := s.cloner.Get(request.PathValue("id"))
	if err != nil {
		writeAPIError(writer, http.StatusNotFound, "CLONE_NOT_FOUND", err.Error())
		return
	}
	if _, ok := s.authorizedWorkload(writer, request, record.SourceWorkloadID); !ok {
		return
	}
	writeAPIJSON(writer, http.StatusOK, record)
}

// handleCloneRollback reverses an uncommitted clone set. Committed sets are
// fleets of ordinary workloads and are refused here — teardown is an explicit
// workload delete, never a side effect.
func (s *Service) handleCloneRollback(writer http.ResponseWriter, request *http.Request) {
	record, err := s.cloner.Get(request.PathValue("id"))
	if err != nil {
		writeAPIError(writer, http.StatusNotFound, "CLONE_NOT_FOUND", err.Error())
		return
	}
	if _, ok := s.authorizedWorkload(writer, request, record.SourceWorkloadID); !ok {
		return
	}
	reversed, err := s.cloner.Rollback(request.Context(), record.ID, "operator requested rollback")
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, "CLONE_ROLLBACK_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusOK, reversed)
}

func (s *Service) handleGC(writer http.ResponseWriter, request *http.Request) {
	caller, _ := s.caller(request)
	if caller.UID != 0 {
		writeAPIError(writer, http.StatusForbidden, "GC_ROOT_REQUIRED",
			"garbage collection requires root; rerun as root (sudo shiftgate gc)")
		return
	}
	var input gcRequest
	if !decodeAPIJSON(writer, request, &input) {
		return
	}
	// Online collection: the agent keeps serving while the collector reclaims
	// unreferenced chunks, guarded by the agent's in-process write lock so no
	// chunk can be deleted between a writer resolving it and installing its
	// reference. The two-phase grace period is unchanged — a chunk must be
	// observed unreferenced across two passes — and a dry run plans without
	// deleting. Concurrent GC runs are serialized: two collectors over one
	// state directory would race the orphan ledger. The offline stopped-agent
	// procedure remains in the reclaim runbook (deployments/monitoring/README.md,
	// "Chunk-store reclaim").
	s.gcRunMu.Lock()
	defer s.gcRunMu.Unlock()
	collector, err := gc.OpenOnline(s.config.StateDir, s.repository, s.chunks, &s.gcGuard)
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "GC_OPEN_FAILED", err.Error())
		return
	}
	defer collector.Close()
	grace, ok := requestDuration(writer, input.GraceSeconds, "grace_seconds")
	if !ok {
		return
	}
	if grace <= 0 {
		grace = 24 * time.Hour
	}
	plan, err := collector.Plan(request.Context(), grace, time.Now().UTC())
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "GC_PLAN_FAILED", err.Error())
		return
	}
	swept, err := collector.Sweep(request.Context(), plan.ID, input.DryRun)
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "GC_SWEEP_FAILED", err.Error())
		return
	}
	deleted, skipped := 0, 0
	for _, candidate := range swept.Candidates {
		switch candidate.Status {
		case "deleted":
			deleted++
		case "skipped", "error":
			skipped++
		}
	}
	writeAPIJSON(writer, http.StatusOK, gcResponse{Plan: swept, Deleted: deleted, Skipped: skipped})
}

func (s *Service) handleMigrationList(writer http.ResponseWriter, request *http.Request) {
	caller, _ := s.caller(request)
	values := s.migrations.List()
	filtered := values[:0]
	for _, value := range values {
		if caller.UID == 0 || s.workloadOwnedBy(value.WorkloadID, caller.UID) {
			filtered = append(filtered, publicMigration(value))
		}
	}
	writeAPIJSON(writer, http.StatusOK, filtered)
}

// publicMigration projects a migration record for API reads. The relay
// ticket is a one-use bearer credential for the transfer session — it is
// served exactly once, to the caller that supplied it in the create request,
// and never re-served on list or get.
func publicMigration(record model.Migration) model.Migration {
	record.Destination.RelayTicket = ""
	return record
}

func (s *Service) handleMigrationCreate(writer http.ResponseWriter, request *http.Request) {
	var input migrateRequest
	if !decodeAPIJSON(writer, request, &input) {
		return
	}
	if strings.TrimSpace(input.WorkloadID) == "" {
		writeAPIError(writer, http.StatusBadRequest, "MIGRATION_REQUEST_INVALID", "workload_id is required")
		return
	}
	workload, ok := s.authorizedWorkload(writer, request, input.WorkloadID)
	if !ok {
		return
	}
	// Live mode is a paid entitlement and this local API is an admission
	// point the product controls: without an entitlement that allows it, the
	// create is refused before anything freezes. Cold mode and preflight are
	// never gated — compatibility must always be checkable.
	if input.Mode == model.MigrationLive && !s.liveMigrationAllowed() {
		writeAPIError(writer, http.StatusPaymentRequired,
			billing.LiveMigrationPlanRequiredCode, billing.LiveMigrationPlanRequiredMessage)
		return
	}
	timeout, ok := requestDuration(writer, input.TimeoutSeconds, "timeout_seconds")
	if !ok {
		return
	}
	result, err := s.migrations.Create(request.Context(), migration.CreateRequest{
		WorkloadID: workload.Spec.ID, IdempotencyKey: input.IdempotencyKey, Destination: input.Destination, Mode: input.Mode,
		PreCopyPasses: input.PreCopyPasses,
		Timeout:       timeout,
	})
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "MIGRATION_CREATE_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusAccepted, result)
}

// handleMigrationPreflight runs a migration's discover and validate stages
// without creating a migration: the destination is reached over the peer
// channel, the same compatibility check runs on the same inputs, and the
// report comes back for the operator to read. Nothing is frozen or moved.
func (s *Service) handleMigrationPreflight(writer http.ResponseWriter, request *http.Request) {
	var input migrateRequest
	if !decodeAPIJSON(writer, request, &input) {
		return
	}
	if strings.TrimSpace(input.WorkloadID) == "" {
		writeAPIError(writer, http.StatusBadRequest, "MIGRATION_REQUEST_INVALID", "workload_id is required")
		return
	}
	workload, ok := s.authorizedWorkload(writer, request, input.WorkloadID)
	if !ok {
		return
	}
	timeout, ok := requestDuration(writer, input.TimeoutSeconds, "timeout_seconds")
	if !ok {
		return
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	result, err := s.migrations.Preflight(ctx, migration.CreateRequest{
		WorkloadID: workload.Spec.ID, IdempotencyKey: input.IdempotencyKey, Destination: input.Destination, Mode: input.Mode,
		PreCopyPasses: input.PreCopyPasses, Timeout: timeout,
	})
	if err != nil {
		switch {
		case errors.Is(err, migration.ErrDestinationUnreachable):
			writeAPIError(writer, http.StatusBadGateway, "DESTINATION_UNREACHABLE", err.Error())
		case errors.Is(err, migration.ErrDestinationIsSource):
			writeAPIError(writer, http.StatusUnprocessableEntity, "DESTINATION_IS_SOURCE", err.Error())
		case errors.Is(err, migration.ErrDestinationIdentityMismatch):
			writeAPIError(writer, http.StatusUnprocessableEntity, "DESTINATION_IDENTITY_MISMATCH", err.Error())
		default:
			writeAPIError(writer, http.StatusUnprocessableEntity, "MIGRATION_PREFLIGHT_FAILED", err.Error())
		}
		return
	}
	writeAPIJSON(writer, http.StatusOK, result)
}

func (s *Service) handleMigrationGet(writer http.ResponseWriter, request *http.Request) {
	result, err := s.migrations.Get(request.PathValue("id"))
	if err != nil {
		status, code := http.StatusInternalServerError, "MIGRATION_LOOKUP_FAILED"
		if errors.Is(err, persistence.ErrNotFound) {
			status, code = http.StatusNotFound, "MIGRATION_NOT_FOUND"
		}
		writeAPIError(writer, status, code, err.Error())
		return
	}
	if _, ok := s.authorizedWorkload(writer, request, result.WorkloadID); !ok {
		return
	}
	writeAPIJSON(writer, http.StatusOK, publicMigration(result))
}

func (s *Service) handleMigrationCancel(writer http.ResponseWriter, request *http.Request) {
	result, err := s.migrations.Get(request.PathValue("id"))
	if err != nil {
		status, code := http.StatusInternalServerError, "MIGRATION_LOOKUP_FAILED"
		if errors.Is(err, persistence.ErrNotFound) {
			status, code = http.StatusNotFound, "MIGRATION_NOT_FOUND"
		}
		writeAPIError(writer, status, code, err.Error())
		return
	}
	if _, ok := s.authorizedWorkload(writer, request, result.WorkloadID); !ok {
		return
	}
	if err := s.migrations.Cancel(result.ID); err != nil {
		writeAPIError(writer, http.StatusConflict, "MIGRATION_CANCEL_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusAccepted, map[string]string{"status": "cancellation_requested"})
}

// handleStandbyList lists this agent's standby duties — the workloads whose
// state it holds and watches — and whether it can confirm a source's death on
// its own. Root sees every duty; other callers see only duties for their own
// workloads, the same ownership rule every other local resource follows.
func (s *Service) handleStandbyList(writer http.ResponseWriter, request *http.Request) {
	caller, _ := s.caller(request)
	duties := s.standby.Duties()
	filtered := duties[:0]
	for _, duty := range duties {
		if caller.UID == 0 || duty.WorkloadUID == int(caller.UID) {
			filtered = append(filtered, publicStandbyDuty(duty))
		}
	}
	writeAPIJSON(writer, http.StatusOK, standbyListResponse{Duties: filtered, AutomaticFailover: s.standby.automaticFailover()})
}

// publicStandbyDuty projects a duty for API responses. The ownership token
// is the control plane's fencing capability — issued exactly once and never
// re-disclosable through routine reads — and the fence result is the fencing
// command's own output; both stay inside the agent.
func publicStandbyDuty(duty model.StandbyDuty) model.StandbyDuty {
	duty.OwnershipToken = ""
	duty.FenceResult = ""
	return duty
}

// handleFailoverList lists the replication ledger: what this source pushed to
// which standby, and how the last attempt went — the source-side counterpart
// of the standby's duty list.
func (s *Service) handleFailoverList(writer http.ResponseWriter, request *http.Request) {
	caller, _ := s.caller(request)
	entries := s.replicator.Entries()
	filtered := entries[:0]
	for _, entry := range entries {
		if caller.UID == 0 || s.workloadOwnedBy(entry.WorkloadID, caller.UID) {
			filtered = append(filtered, entry)
		}
	}
	writeAPIJSON(writer, http.StatusOK, filtered)
}

// handleStandbyTrigger commands an explicit failover. No death confirmation
// runs first — the command is the confirmation — but the source is probed
// when its URL is known, and a source that still answers lands in the
// response as a warning: two live copies then exist, and only the operator
// can resolve that. The failed-over duty comes back so the caller sees what
// ran, from what checkpoint, under which restore id.
func (s *Service) handleStandbyTrigger(writer http.ResponseWriter, request *http.Request) {
	duty, err := s.standby.Duty(request.PathValue("workload"))
	if err != nil {
		status, code := http.StatusInternalServerError, "STANDBY_DUTY_LOOKUP_FAILED"
		if errors.Is(err, persistence.ErrNotFound) {
			status, code = http.StatusNotFound, "STANDBY_DUTY_NOT_FOUND"
		}
		writeAPIError(writer, status, code, err.Error())
		return
	}
	if !s.authorizeUID(writer, request, duty.WorkloadUID) {
		return
	}
	var input standbyTriggerRequest
	if request.ContentLength != 0 && !decodeAPIJSON(writer, request, &input) {
		return
	}
	response := standbyTriggerResponse{SourceReachable: "unknown"}
	if duty.SourceAgentURL != "" {
		if alive, probeErr := s.standby.probeSource(request.Context(), duty.SourceAgentURL); probeErr == nil {
			response.SourceReachable = "no"
			if alive {
				response.SourceReachable = "yes"
				response.Warning = "the source answered its peer listener; this workload now has two live copies — stop one manually"
			}
		}
	}
	updated, err := s.standby.Trigger(request.Context(), duty.WorkloadID, input.CheckpointID, input.Lazy, "operator commanded failover")
	if err != nil {
		status, code := http.StatusConflict, "FAILOVER_FAILED"
		if errors.Is(err, persistence.ErrNotFound) {
			status, code = http.StatusNotFound, "STANDBY_DUTY_NOT_FOUND"
		}
		writeAPIError(writer, status, code, err.Error())
		return
	}
	response.Duty = publicStandbyDuty(updated)
	writeAPIJSON(writer, http.StatusOK, response)
}

func (s *Service) authorizedWorkload(writer http.ResponseWriter, request *http.Request, id string) (model.Workload, bool) {
	workload, err := s.runtime.Get(id)
	if err != nil {
		status, code := http.StatusInternalServerError, "WORKLOAD_LOOKUP_FAILED"
		message := err.Error()
		if errors.Is(err, persistence.ErrNotFound) {
			status, code = http.StatusNotFound, "WORKLOAD_NOT_FOUND"
			message = fmt.Sprintf("no workload with id %q; run 'shiftgate workload list' to see the known ones", id)
		}
		writeAPIError(writer, status, code, message)
		return model.Workload{}, false
	}
	caller, _ := s.caller(request)
	if caller.UID != 0 && workload.Spec.UID != int(caller.UID) {
		writeAPIError(writer, http.StatusForbidden, "WORKLOAD_FORBIDDEN", "workload belongs to another local user")
		return model.Workload{}, false
	}
	return workload, true
}

func (s *Service) authorizeUID(writer http.ResponseWriter, request *http.Request, uid int) bool {
	caller, _ := s.caller(request)
	if caller.UID != 0 && uid != int(caller.UID) {
		writeAPIError(writer, http.StatusForbidden, "RESOURCE_FORBIDDEN", "resource belongs to another local user")
		return false
	}
	return true
}

func (s *Service) workloadOwnedBy(id string, uid uint32) bool {
	workload, err := s.runtime.Get(id)
	return err == nil && workload.Spec.UID == int(uid)
}

// authorizeNewSpec vets a workload spec before any record exists. A
// non-root caller may only run workloads as itself, under a root it owns;
// workload ids and names are admitted here too, before either can reach a
// record key, a URL path segment, or a log-file path.
func (s *Service) authorizeNewSpec(spec *model.WorkloadSpec, caller peerCredentials) error {
	if caller.UID != 0 {
		if spec.UID != 0 && spec.UID != int(caller.UID) {
			return errors.New("non-root callers cannot choose another workload uid")
		}
		if spec.GID != 0 && spec.GID != int(caller.GID) {
			return errors.New("non-root callers cannot choose another workload gid")
		}
		spec.UID = int(caller.UID)
		spec.GID = int(caller.GID)
	}
	// Workload ids are the agent's to mint. A client-supplied id is either a
	// masquerade attempt against an existing record (refused with 409 before
	// anything can overwrite it) or a name the agent will not store: ids are
	// format-checked so a crafted one can never steer a path join (the log
	// paths under .shift/logs are keyed by the id), then dropped so the
	// create always carries a freshly minted UUID.
	if spec.ID != "" {
		if !validWorkloadID(spec.ID) {
			return fmt.Errorf("%w: %q is not a well-formed workload id; omit the id field and read the assigned one from the create response", errWorkloadIDInvalid, spec.ID)
		}
		for _, existing := range s.runtime.List() {
			if existing.Spec.ID == spec.ID {
				return fmt.Errorf("%w: refusing to overwrite existing workload %s; ids are assigned by the agent", errWorkloadIDConflict, spec.ID)
			}
		}
		spec.ID = ""
	}
	if spec.Name != "" {
		for _, existing := range s.runtime.List() {
			if strings.EqualFold(existing.Spec.Name, spec.Name) {
				return fmt.Errorf("%w: %q is already in use; workload names are machine-global, pick another", errWorkloadNameConflict, spec.Name)
			}
		}
	}
	if strings.TrimSpace(spec.RootPath) == "" {
		return fmt.Errorf("%w: root path is required; SHIFT will not infer an unrestricted home-directory capture", errWorkloadRootInvalid)
	}
	info, err := os.Stat(spec.RootPath)
	if err != nil {
		return fmt.Errorf("%w: %v", errWorkloadRootInvalid, err)
	}
	if caller.UID != 0 {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != caller.UID {
			return errors.New("workload root must be owned by the calling user")
		}
	}
	return nil
}

// validWorkloadID reports whether an id is exactly the shape model.NewID
// mints: a UUIDv4 in lowercase hex with dashes at the fixed positions. It is
// the format gate in front of every path a workload id can reach — log files
// are named <id>.log under directories the root agent creates.
func validWorkloadID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		char := id[i]
		switch i {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
				return false
			}
		}
	}
	return true
}

// requestDuration converts a caller-supplied seconds field into a duration,
// refusing values whose conversion would wrap into a completely different
// deadline. Zero keeps its per-route meaning of "the downstream default".
func requestDuration(writer http.ResponseWriter, seconds int, field string) (time.Duration, bool) {
	if seconds < 0 || seconds > maxRequestSeconds {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT",
			fmt.Sprintf("%s must be between 0 and %d seconds", field, maxRequestSeconds))
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// decodeAPIJSON decodes a request body under the local API's size limit. An
// empty body is the zero request — the documented way to clear a policy or
// run a default GC — not malformed JSON, and an over-limit body is a 413,
// not a generic decode failure.
func decodeAPIJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, localBodyLimit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		writeDecodeError(writer, err)
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != nil {
		if !errors.Is(err, io.EOF) {
			writeDecodeError(writer, err)
			return false
		}
		return true
	}
	writeAPIError(writer, http.StatusBadRequest, "INVALID_JSON", "request contains trailing data")
	return false
}

func writeDecodeError(writer http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeAPIError(writer, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE",
			fmt.Sprintf("request body exceeds the %d byte local API limit", localBodyLimit))
		return
	}
	writeAPIError(writer, http.StatusBadRequest, "INVALID_JSON", err.Error())
}

func writeAPIJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeAPIError(writer http.ResponseWriter, status int, code, message string) {
	writeAPIJSON(writer, status, model.ErrorResponse{Code: code, Message: message})
}

func contextWithoutCancellation(request *http.Request) context.Context {
	return context.WithoutCancel(request.Context())
}
