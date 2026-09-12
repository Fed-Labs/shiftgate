package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/migration"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/network"
	"shift.dev/shift/internal/persistence"
)

const localBodyLimit = int64(4 << 20)

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

type migrateRequest struct {
	WorkloadID     string              `json:"workload_id"`
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
	if err := authorizeNewSpec(&spec, caller); err != nil {
		writeAPIError(writer, http.StatusForbidden, "WORKLOAD_FORBIDDEN", err.Error())
		return
	}
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
	// Deleting a workload withdraws its network presence too: forwarders
	// drain and the host-port reservations go back, so a later workload can
	// claim the ports. Its changed-file index is discarded with it.
	s.network.Deactivate(workload.Spec.ID, network.DefaultDrainGrace)
	s.checkpoints.DiscardIndex(workload.Spec.ID)
	if err := s.runtime.Delete(workload.Spec.ID); err != nil {
		writeAPIError(writer, http.StatusConflict, "WORKLOAD_DELETE_FAILED", err.Error())
		return
	}
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
	timeout := time.Duration(input.TimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > time.Minute {
		timeout = 10 * time.Second
	}
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
	if tail > 5000 {
		tail = 5000
	}
	reader, err := s.runtime.LogReader(workload.Spec.ID, tail)
	if err != nil {
		writeAPIError(writer, http.StatusNotFound, "LOGS_NOT_FOUND", err.Error())
		return
	}
	defer reader.Close()
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(writer, reader)
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
	workload, ok := s.authorizedWorkload(writer, request, input.WorkloadID)
	if !ok {
		return
	}
	leaveRunning := true
	if input.LeaveRunning != nil {
		leaveRunning = *input.LeaveRunning
	}
	manifest, err := s.checkpoints.Create(request.Context(), workload.Spec.ID, checkpoint.CreateOptions{
		Kind: input.Kind, ParentID: input.ParentID, LeaveRunning: leaveRunning,
		TCPState: input.TCPState, Timeout: time.Duration(input.TimeoutSeconds) * time.Second,
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
	record, err := s.restorer.Prepare(request.Context(), manifest.ID, checkpoint.PrepareOptions{
		Timeout: time.Duration(input.TimeoutSeconds) * time.Second,
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
		writeAPIError(writer, http.StatusNotFound, "RESTORE_NOT_FOUND", err.Error())
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
	record, err := s.forker.Fork(request.Context(), sourceWorkloadID, checkpoint.ForkOptions{
		Name: input.Name, RootPath: input.RootPath, CheckpointID: input.CheckpointID,
		Activate: input.Activate, Timeout: time.Duration(input.TimeoutSeconds) * time.Second,
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
	record, err := s.cloner.Clone(request.Context(), manifest.ID, checkpoint.CloneOptions{
		Count: input.Count, NamePrefix: input.NamePrefix, Parallel: input.Parallel,
		Timeout: time.Duration(input.TimeoutSeconds) * time.Second,
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

func (s *Service) handleMigrationList(writer http.ResponseWriter, request *http.Request) {
	caller, _ := s.caller(request)
	values := s.migrations.List()
	filtered := values[:0]
	for _, value := range values {
		if caller.UID == 0 || s.workloadOwnedBy(value.WorkloadID, caller.UID) {
			filtered = append(filtered, value)
		}
	}
	writeAPIJSON(writer, http.StatusOK, filtered)
}

func (s *Service) handleMigrationCreate(writer http.ResponseWriter, request *http.Request) {
	var input migrateRequest
	if !decodeAPIJSON(writer, request, &input) {
		return
	}
	workload, ok := s.authorizedWorkload(writer, request, input.WorkloadID)
	if !ok {
		return
	}
	result, err := s.migrations.Create(request.Context(), migration.CreateRequest{
		WorkloadID: workload.Spec.ID, Destination: input.Destination, Mode: input.Mode,
		PreCopyPasses: input.PreCopyPasses,
		Timeout:       time.Duration(input.TimeoutSeconds) * time.Second,
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
	workload, ok := s.authorizedWorkload(writer, request, input.WorkloadID)
	if !ok {
		return
	}
	timeout := time.Duration(input.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	result, err := s.migrations.Preflight(ctx, migration.CreateRequest{
		WorkloadID: workload.Spec.ID, Destination: input.Destination, Mode: input.Mode,
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
		writeAPIError(writer, http.StatusNotFound, "MIGRATION_NOT_FOUND", err.Error())
		return
	}
	if _, ok := s.authorizedWorkload(writer, request, result.WorkloadID); !ok {
		return
	}
	writeAPIJSON(writer, http.StatusOK, result)
}

func (s *Service) handleMigrationCancel(writer http.ResponseWriter, request *http.Request) {
	result, err := s.migrations.Get(request.PathValue("id"))
	if err != nil {
		writeAPIError(writer, http.StatusNotFound, "MIGRATION_NOT_FOUND", err.Error())
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
			filtered = append(filtered, duty)
		}
	}
	writeAPIJSON(writer, http.StatusOK, standbyListResponse{Duties: filtered, AutomaticFailover: s.standby.automaticFailover()})
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
		status := http.StatusInternalServerError
		if errors.Is(err, persistence.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeAPIError(writer, status, "STANDBY_DUTY_NOT_FOUND", err.Error())
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
	response.Duty = updated
	writeAPIJSON(writer, http.StatusOK, response)
}

func (s *Service) authorizedWorkload(writer http.ResponseWriter, request *http.Request, id string) (model.Workload, bool) {
	workload, err := s.runtime.Get(id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, persistence.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeAPIError(writer, status, "WORKLOAD_NOT_FOUND", err.Error())
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

func authorizeNewSpec(spec *model.WorkloadSpec, caller peerCredentials) error {
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
	info, err := os.Stat(spec.RootPath)
	if err != nil {
		return err
	}
	if caller.UID != 0 {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != caller.UID {
			return errors.New("workload root must be owned by the calling user")
		}
	}
	return nil
}

func decodeAPIJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, localBodyLimit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_JSON", "request contains trailing data")
		return false
	}
	return true
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
