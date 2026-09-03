package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/transfer"
	"shift.dev/shift/internal/update"
)

// agentGate is the update gate over the agent's own in-flight work. A binary
// swap during a migration, restore, fork, or incoming transfer would leave the
// transferred state half-written by one version and half by another, so the
// installer consults this gate twice: before the download and again immediately
// before the swap.
type agentGate struct {
	service *Service
}

// Ready implements update.Gate.
func (gate agentGate) Ready(_ context.Context) error {
	return gate.service.inFlightWork().busy()
}

// inFlightWork is what the update gate consults: the long-running work this
// machine is doing right now. It exists as one small type so the readiness
// rules are stated once and can be tested directly; the agent always fills it
// from its real subsystems.
type inFlightWork struct {
	migrations func() []model.Migration
	restores   func() []checkpoint.RestoreRecord
	forks      func() []checkpoint.ForkRecord
	sessions   func() []transfer.Session
}

// inFlightWork collects the agent's live work for the gate.
func (s *Service) inFlightWork() inFlightWork {
	return inFlightWork{
		migrations: s.migrations.List,
		restores:   s.restorer.List,
		forks:      s.forker.List,
		sessions:   s.sessions.List,
	}
}

// busy reports the first piece of in-flight work an update must wait for, or nil
// when the machine is idle. Terminal records are history rather than work and
// do not block.
func (work inFlightWork) busy() error {
	for _, migration := range work.migrations() {
		switch migration.Stage {
		case model.MigrationCompleted, model.MigrationFailed, model.MigrationRolledBack, model.MigrationCancelled:
		default:
			return fmt.Errorf("migration %s of workload %s is %s", migration.ID, migration.WorkloadID, migration.Stage)
		}
	}
	for _, record := range work.restores() {
		switch record.State {
		case checkpoint.RestoreCommitted, checkpoint.RestoreRolledBack, checkpoint.RestoreFailed:
		default:
			return fmt.Errorf("restore %s of workload %s is %s", record.ID, record.WorkloadID, record.State)
		}
	}
	for _, record := range work.forks() {
		switch record.State {
		case checkpoint.ForkCommitted, checkpoint.ForkRolledBack, checkpoint.ForkFailed:
		default:
			return fmt.Errorf("fork %s of workload %s is %s", record.ID, record.SourceWorkloadID, record.State)
		}
	}
	for _, session := range work.sessions() {
		switch session.State {
		case transfer.SessionCommitted, transfer.SessionRolledBack, transfer.SessionFailed:
		default:
			return fmt.Errorf("incoming transfer %s for workload %s is %s", session.ID, session.WorkloadID, session.State)
		}
	}
	return nil
}

// openUpdates builds the update manager from configuration. Updates stay off
// until a machine is given both a release feed and the keys it trusts, so a
// default installation never consults a release server.
func (s *Service) openUpdates() error {
	updates := s.config.Updates
	if !updates.Enabled {
		return nil
	}
	keys, err := updates.LoadTrustedKeys()
	if err != nil {
		return fmt.Errorf("updates: %w", err)
	}
	ring, err := update.NewKeyRing(keys, updates.SignatureThreshold, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("updates: %w", err)
	}
	source, err := updates.FeedSource()
	if err != nil {
		return fmt.Errorf("updates: %w", err)
	}
	executable, err := updatesExecutable(updates.ExecutablePath)
	if err != nil {
		return fmt.Errorf("updates: %w", err)
	}
	installer, err := update.NewInstaller(update.InstallerConfig{
		Component:      update.ComponentAgent,
		ExecutablePath: executable,
		StagingRoot:    updates.StagingDir,
		BackupRoot:     updates.BackupDir,
		Fetcher:        updates.Fetcher(),
		Gate:           agentGate{service: s},
		KeepBackups:    updates.KeepBackups,
	})
	if err != nil {
		return fmt.Errorf("updates: %w", err)
	}
	var peerProtocol func() int
	if s.reporter != nil {
		peerProtocol = s.reporter.PeerProtocolVersion
	}
	manager, err := update.NewManager(update.ManagerConfig{
		Installation: update.Installation{
			Component:       update.ComponentAgent,
			Version:         config.Version,
			Channel:         updates.Channel,
			OS:              runtime.GOOS,
			Architecture:    runtime.GOARCH,
			MachineID:       s.identity.Machine.ID,
			ExecutablePath:  executable,
			ProtocolVersion: model.ProtocolVersion,
			ConfigVersion:   config.AgentConfigVersion,
			StateVersion:    model.StateSchemaVersion,
		},
		Source:              source,
		KeyRing:             ring,
		Installer:           installer,
		StatePath:           updates.StateFile,
		Policy:              updates.Policy,
		CheckInterval:       updates.CheckInterval,
		Logger:              s.logger.With("component", "updates"),
		PeerProtocolVersion: peerProtocol,
	})
	if err != nil {
		return fmt.Errorf("updates: %w", err)
	}
	s.updates = manager
	return nil
}

// updatesExecutable resolves the binary an update replaces: the configured path
// when one is given, otherwise the running executable. Symlinks are resolved
// because the installer swaps the real file with a rename in its directory.
func updatesExecutable(configured string) (string, error) {
	target := configured
	if target == "" {
		running, err := os.Executable()
		if err != nil {
			return "", err
		}
		target = running
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

type updateApplyRequest struct {
	Version string `json:"version,omitempty"`
}

type updateRollbackRequest struct {
	BackupID string `json:"backup_id,omitempty"`
}

type updateBlockRequest struct {
	Version string `json:"version"`
	Reason  string `json:"reason,omitempty"`
}

type updateUnblockRequest struct {
	Version string `json:"version"`
}

// requireUpdates reports the update manager, or explains that this machine has
// no update configuration rather than failing on a nil pointer.
func (s *Service) requireUpdates(writer http.ResponseWriter) (*update.Manager, bool) {
	if s.updates == nil {
		writeAPIError(writer, http.StatusNotFound, "UPDATES_DISABLED", "updates are not configured on this machine")
		return nil, false
	}
	return s.updates, true
}

// authorizeUpdateMutation requires the caller to be root or the user the agent
// itself runs as: replacing the agent binary is already equivalent to that
// power, and this keeps any other local user from wielding it.
func (s *Service) authorizeUpdateMutation(writer http.ResponseWriter, request *http.Request) bool {
	caller, _ := s.caller(request)
	if caller.UID == 0 || caller.UID == uint32(os.Geteuid()) {
		return true
	}
	writeAPIError(writer, http.StatusForbidden, "UPDATE_FORBIDDEN", "updates may only be managed by root or the agent's own user")
	return false
}

// handleUpdateStatus reports the full update picture. Any authenticated local
// caller may read it: knowing which release is pending is not a privilege.
func (s *Service) handleUpdateStatus(writer http.ResponseWriter, _ *http.Request) {
	manager, ok := s.requireUpdates(writer)
	if !ok {
		return
	}
	writeAPIJSON(writer, http.StatusOK, manager.Status())
}

// handleUpdateCheck consults the feed now rather than waiting for the interval.
// It writes update state, so it is held to the same caller as a mutation.
func (s *Service) handleUpdateCheck(writer http.ResponseWriter, request *http.Request) {
	manager, ok := s.requireUpdates(writer)
	if !ok {
		return
	}
	if !s.authorizeUpdateMutation(writer, request) {
		return
	}
	result, err := manager.Check(request.Context())
	if err != nil {
		writeAPIError(writer, http.StatusBadGateway, "UPDATE_CHECK_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusOK, result)
}

// handleUpdateApply installs a release: the current selection when no version is
// named, or one specific version an operator has chosen. The installed binary
// only takes effect when the service restarts, which the status reports.
func (s *Service) handleUpdateApply(writer http.ResponseWriter, request *http.Request) {
	manager, ok := s.requireUpdates(writer)
	if !ok {
		return
	}
	if !s.authorizeUpdateMutation(writer, request) {
		return
	}
	var input updateApplyRequest
	if request.ContentLength != 0 && !decodeAPIJSON(writer, request, &input) {
		return
	}
	installed, err := manager.Apply(request.Context(), input.Version)
	if err != nil {
		if errors.Is(err, update.ErrNotReady) {
			writeAPIError(writer, http.StatusConflict, "UPDATE_DEFERRED", err.Error())
			return
		}
		writeAPIError(writer, http.StatusUnprocessableEntity, "UPDATE_APPLY_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusOK, installed)
}

// handleUpdateRollback reinstalls a preserved binary. It works without network
// access, from the backups the installer keeps, and blocks the version it undid
// so the automatic loop cannot immediately reinstall it.
func (s *Service) handleUpdateRollback(writer http.ResponseWriter, request *http.Request) {
	manager, ok := s.requireUpdates(writer)
	if !ok {
		return
	}
	if !s.authorizeUpdateMutation(writer, request) {
		return
	}
	var input updateRollbackRequest
	if request.ContentLength != 0 && !decodeAPIJSON(writer, request, &input) {
		return
	}
	installed, err := manager.Rollback(request.Context(), input.BackupID)
	if err != nil {
		writeAPIError(writer, http.StatusConflict, "UPDATE_ROLLBACK_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusOK, installed)
}

// handleUpdateBlock refuses one version on this machine, which is how an
// operator answers a release that misbehaved here.
func (s *Service) handleUpdateBlock(writer http.ResponseWriter, request *http.Request) {
	manager, ok := s.requireUpdates(writer)
	if !ok {
		return
	}
	if !s.authorizeUpdateMutation(writer, request) {
		return
	}
	var input updateBlockRequest
	if !decodeAPIJSON(writer, request, &input) {
		return
	}
	if err := manager.Block(input.Version, input.Reason); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "UPDATE_BLOCK_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusOK, manager.Status())
}

// handleUpdateUnblock allows a blocked version again, which is how an operator
// re-arms a release after the reason it was blocked was addressed.
func (s *Service) handleUpdateUnblock(writer http.ResponseWriter, request *http.Request) {
	manager, ok := s.requireUpdates(writer)
	if !ok {
		return
	}
	if !s.authorizeUpdateMutation(writer, request) {
		return
	}
	var input updateUnblockRequest
	if !decodeAPIJSON(writer, request, &input) {
		return
	}
	if err := manager.Unblock(input.Version); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "UPDATE_UNBLOCK_FAILED", err.Error())
		return
	}
	writeAPIJSON(writer, http.StatusOK, manager.Status())
}
