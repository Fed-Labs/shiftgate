package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
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
}

type forkRequest struct {
	Name           string `json:"name,omitempty"`
	RootPath       string `json:"root_path,omitempty"`
	CheckpointID   string `json:"checkpoint_id,omitempty"`
	Activate       bool   `json:"activate,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type migrateRequest struct {
	WorkloadID     string              `json:"workload_id"`
	Destination    model.Destination   `json:"destination"`
	Mode           model.MigrationMode `json:"mode,omitempty"`
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
	mux.HandleFunc("GET /v1/migrations", s.handleMigrationList)
	mux.HandleFunc("POST /v1/migrations", s.handleMigrationCreate)
	mux.HandleFunc("GET /v1/migrations/{id}", s.handleMigrationGet)
	mux.HandleFunc("POST /v1/migrations/{id}/cancel", s.handleMigrationCancel)
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

func (s *Service) handleDoctor(writer http.ResponseWriter, request *http.Request) {
	capabilities, err := s.inventory.Inspect(request.Context())
	checks := []doctorCheck{
		{Name: "platform", OK: model.CurrentPlatformSupported(), Message: capabilities.OS + "/" + capabilities.Architecture},
		{Name: "privileges", OK: os.Geteuid() == 0, Message: "agent effective uid " + strconv.Itoa(os.Geteuid())},
		{Name: "criu", OK: capabilities.CRIU.Installed && capabilities.CRIU.Healthy, Message: capabilities.CRIU.Version},
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
	record, err := s.restorer.Prepare(request.Context(), manifest.ID, time.Duration(input.TimeoutSeconds)*time.Second)
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
		Timeout: time.Duration(input.TimeoutSeconds) * time.Second,
	})
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "MIGRATION_CREATE_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusAccepted, result)
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
