package agent

// standby.go is the receiving half of warm-standby failover. The peer server
// hands this file's supervisor every replication session it parks in the
// HELD state; the supervisor turns those into duty records, enforces the
// source's retention, and watches each armed duty for the source's death.
// Failover is deliberately hard to trigger: the standby restores only when
// the source's peer listener is unreachable AND the control plane's record
// for it is stale or offline — or when an operator explicitly commands it.
// There is no fencing: a source that is partitioned rather than dead can
// come back to a second live copy, and the docs say so.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/controlplane"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/persistence"
	shiftruntime "shift.dev/shift/internal/runtime"
	"shift.dev/shift/internal/securestore"
	"shift.dev/shift/internal/transfer"
)

// Supervisor cadence and thresholds. The tick is short so death detection is
// bounded by the control plane's staleness, not by this loop; the staleness
// bound itself is three default reporter intervals, because a machine that
// stopped heartbeating for that long is the strongest absence signal the
// control plane can give without a fencing mechanism.
const (
	standbyTick         = 10 * time.Second
	presenceStaleAfter  = 90 * time.Second
	standbyProbeTimeout = 10 * time.Second
	standbyFetchTimeout = 10 * time.Second
	failoverRestoreWait = 10 * time.Minute
	failoverRetryDelay  = time.Minute
)

// standbySupervisor owns the duty records. It implements the peer server's
// ReplicationWatcher, so its callbacks run on the server's handler
// goroutine and do only the one durable write a hold needs; everything slow
// — retention, presence queries, probes, the failover itself — belongs to
// the loop.
type standbySupervisor struct {
	duties      *securestore.EncryptedCollection[model.StandbyDuty]
	runtime     *shiftruntime.Manager
	checkpoints *checkpoint.Service
	restorer    *checkpoint.Restorer
	// controlURL/organizationID/apiKey drive the control-plane presence
	// query. An empty controlURL means this agent cannot confirm a source's
	// death, so the supervisor never fails over on its own — only an
	// operator trigger can act. The key is the machines-scope API key the
	// reporter already carries; it is sent as a bearer header and never
	// logged.
	controlURL     string
	organizationID string
	apiKey         string
	presenceClient *http.Client
	// peerClient builds a client for probing a source's peer listener. A
	// construction failure is this standby's problem and proves nothing
	// about the source.
	peerClient func(endpoint string) (*transfer.Client, error)
	logger     *slog.Logger
	mu         sync.Mutex
}

// openStandbySupervisor loads (or creates) the encrypted duty collection.
func openStandbySupervisor(duties *securestore.EncryptedCollection[model.StandbyDuty], runtimeManager *shiftruntime.Manager, checkpoints *checkpoint.Service, restorer *checkpoint.Restorer, controlURL, organizationID, apiKey string, peerClient func(string) (*transfer.Client, error), logger *slog.Logger) *standbySupervisor {
	presenceClient := &http.Client{Timeout: standbyFetchTimeout}
	return &standbySupervisor{
		duties: duties, runtime: runtimeManager, checkpoints: checkpoints, restorer: restorer,
		controlURL:     strings.TrimRight(strings.TrimSpace(controlURL), "/"),
		organizationID: strings.TrimSpace(organizationID),
		apiKey:         strings.TrimSpace(apiKey),
		presenceClient: presenceClient, peerClient: peerClient,
		logger: logger,
	}
}

// Duties lists the standby duties, oldest hold first.
func (s *standbySupervisor) Duties() []model.StandbyDuty {
	duties := s.duties.List()
	sort.Slice(duties, func(i, j int) bool { return duties[i].HeldAt.Before(duties[j].HeldAt) })
	return duties
}

// Duty reads one standby duty by workload id.
func (s *standbySupervisor) Duty(workloadID string) (model.StandbyDuty, error) {
	return s.duties.Get(workloadID)
}

// automaticFailover reports whether this agent can confirm a source's death
// on its own. It is false exactly when no control plane is configured, and
// the duty list says so: armed duties then need an explicit trigger.
func (s *standbySupervisor) automaticFailover() bool {
	return s.controlURL != ""
}

// ReplicationHeld records a session the peer server parked in the HELD
// state: the checkpoint is imported, verified, and restorable from here on.
// The duty it upserts is the fact the rest of the file reasons from.
func (s *standbySupervisor) ReplicationHeld(session transfer.Session) {
	if session.Purpose != transfer.PurposeReplication || session.State != transfer.SessionHeld || session.CheckpointID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	duty, err := s.duties.Get(session.WorkloadID)
	switch {
	case err == nil:
	case errors.Is(err, persistence.ErrNotFound):
		duty = model.StandbyDuty{WorkloadID: session.WorkloadID, HeldAt: now, State: model.StandbyArmed}
	default:
		s.logger.Error("standby duty lookup failed", "workload_id", session.WorkloadID, "error", err)
		return
	}
	if duty.State == model.StandbyFailedOver {
		// The workload already runs here, yet its source just pushed fresh
		// state — the source is alive again after a failover its own death
		// announcement caused. Nothing automatic resolves two live copies;
		// say it loudly and keep the record.
		s.logger.Warn("replication arrived for a workload already failed over; the source is alive again and two live copies may now exist — resolve manually",
			"workload_id", session.WorkloadID, "source", session.SourceMachineID, "checkpoint_id", session.CheckpointID)
		return
	}
	duty.SourceMachineID = session.SourceMachineID
	duty.SourceAgentURL = session.SourceAgentURL
	duty.KeepLast = session.KeepLast
	duty.LastCheckpointID = session.CheckpointID
	if manifest, loadErr := s.checkpoints.Load(session.CheckpointID); loadErr == nil {
		duty.WorkloadName = manifest.Workload.Name
		duty.WorkloadUID = manifest.Workload.UID
		duty.LastCheckpointAt = manifest.CreatedAt
	} else {
		// The session is already HELD, so the manifest was imported and
		// verified; a load failure now is a local read problem, and the
		// hold still stands with the facts the session itself carries.
		duty.LastCheckpointAt = session.UpdatedAt
		s.logger.Warn("held checkpoint manifest could not be re-read", "checkpoint_id", session.CheckpointID, "error", loadErr)
	}
	duty.UpdatedAt = now
	if err := s.duties.Put(duty.WorkloadID, duty); err != nil {
		s.logger.Error("standby duty persist failed", "workload_id", duty.WorkloadID, "error", err)
		return
	}
	s.logger.Info("standby duty updated",
		"workload_id", duty.WorkloadID, "checkpoint_id", duty.LastCheckpointID,
		"source", duty.SourceMachineID, "keep_last", duty.KeepLast)
}

// ReplicationWithdrawn drops the duty the authenticated source withdrew.
// The replicated checkpoints stay: deleting state on another machine's
// say-so is not what a withdrawal is for, and the standby's operator can
// prune what a withdrawn duty leaves behind.
func (s *standbySupervisor) ReplicationWithdrawn(sourceMachineID, workloadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	duty, err := s.duties.Get(workloadID)
	if err != nil {
		if !errors.Is(err, persistence.ErrNotFound) {
			s.logger.Error("standby duty lookup failed", "workload_id", workloadID, "error", err)
		}
		return
	}
	if duty.SourceMachineID != sourceMachineID {
		s.logger.Warn("standby withdrawal from a machine that is not the recorded source",
			"workload_id", workloadID, "recorded_source", duty.SourceMachineID, "caller", sourceMachineID)
		return
	}
	if duty.State == model.StandbyFailedOver {
		s.logger.Warn("withdrawal for a workload already failed over; the duty stays as the failover's history",
			"workload_id", workloadID)
		return
	}
	if err := s.duties.Delete(workloadID); err != nil {
		s.logger.Error("standby duty withdraw failed", "workload_id", workloadID, "error", err)
		return
	}
	s.logger.Info("standby duty withdrawn", "workload_id", workloadID, "source", sourceMachineID)
}

// run is the supervisor loop: retention, then death watch, then failover.
func (s *standbySupervisor) run(ctx context.Context) {
	ticker := time.NewTicker(standbyTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick is one supervision pass. The control plane is asked first — one
// cheap org-wide query per pass — and a source's peer listener is probed
// only when the control plane's record for it already looks stale, so a
// healthy source pays no probe traffic at all.
func (s *standbySupervisor) tick(ctx context.Context) {
	duties := s.Duties()
	for _, duty := range duties {
		if duty.State != model.StandbyArmed {
			continue
		}
		if duty.KeepLast > 0 {
			if _, err := s.checkpoints.PruneWorkload(duty.WorkloadID, duty.KeepLast); err != nil {
				s.logger.Warn("standby retention could not complete", "workload_id", duty.WorkloadID, "error", err)
			}
		}
	}
	if s.controlURL == "" {
		return
	}
	presence, err := s.fetchPresence(ctx)
	if err != nil {
		s.logger.Warn("control-plane presence query failed; no automatic failover this pass", "error", err)
		return
	}
	for _, duty := range duties {
		if duty.State != model.StandbyArmed {
			continue
		}
		if !time.Now().UTC().Before(duty.NextAttemptAt) {
			record, found := presence[duty.SourceMachineID]
			s.considerFailover(ctx, duty, record, found)
		}
	}
}

// considerFailover applies the two-signal rule to one armed duty: the
// control plane's record must be stale or offline, and then the source's
// own peer listener must fail to answer. One signal alone never fails a
// workload over — the control plane can be partitioned from a live source,
// and a probe failure can be this standby's own misconfiguration.
func (s *standbySupervisor) considerFailover(ctx context.Context, duty model.StandbyDuty, record controlplane.Machine, found bool) {
	if !found {
		s.logger.Warn("control plane has no record of a standby duty's source; automatic failover cannot confirm its death — register the source machine or trigger the failover explicitly",
			"workload_id", duty.WorkloadID, "source", duty.SourceMachineID)
		return
	}
	if !presenceOffline(record) {
		return
	}
	if duty.SourceAgentURL == "" {
		s.logger.Warn("standby duty has no source agent URL to probe; automatic failover cannot confirm death — trigger the failover explicitly",
			"workload_id", duty.WorkloadID, "source", duty.SourceMachineID)
		return
	}
	alive, err := s.probeSource(ctx, duty.SourceAgentURL)
	if err != nil {
		// The probe itself could not run — a client this standby cannot
		// build says nothing about the source. Never fail over on our own
		// misconfiguration.
		s.logger.Warn("standby source probe could not run", "workload_id", duty.WorkloadID, "error", err)
		return
	}
	if alive {
		// The control plane's staleness was a false alarm; the source
		// answered its peer listener. Healthy, and cheaper than a fence.
		return
	}
	reason := fmt.Sprintf("source peer listener unreachable and the control plane's record is %s", presenceDescription(record))
	// An automatic failover always restores eagerly: nobody opted into the
	// lazy trade-off, and an unattended recovery must be the predictable one.
	if _, err := s.failover(ctx, duty.WorkloadID, "", false, reason); err != nil {
		s.logger.Error("automatic failover failed", "workload_id", duty.WorkloadID, "error", err)
		s.recordAttemptFailure(duty, err)
	}
}

// presenceOffline reports whether a control-plane machine record counts as
// offline: a machine that announced offline, or whose last heartbeat is
// older than the staleness bound. A clock skewed between this standby and
// the control plane shifts the judgment by the skew — the bound is minutes,
// not seconds, for exactly that reason.
func presenceOffline(record controlplane.Machine) bool {
	if record.Status == "offline" {
		return true
	}
	return record.LastSeenAt != nil && time.Since(*record.LastSeenAt) > presenceStaleAfter
}

// presenceDescription renders what the control plane actually observed, for
// the duty record and the log — never a vague "unreachable".
func presenceDescription(record controlplane.Machine) string {
	if record.Status == "offline" {
		return "offline"
	}
	if record.LastSeenAt != nil {
		return fmt.Sprintf("online but last seen %s ago", time.Since(*record.LastSeenAt).Round(time.Second))
	}
	return "online with no last-seen timestamp"
}

// probeSource dials the source's peer listener. A client construction
// failure is returned as an error — inconclusive; only a listener that
// fails to answer counts as unreachable.
func (s *standbySupervisor) probeSource(ctx context.Context, agentURL string) (bool, error) {
	peer, err := s.peerClient(agentURL)
	if err != nil {
		return false, fmt.Errorf("probe client: %w", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, standbyProbeTimeout)
	defer cancel()
	if _, err := peer.Machine(probeCtx); err != nil {
		return false, nil
	}
	return true, nil
}

// fetchPresence reads the organization's machine records from the control
// plane — the same machines-scope key the reporter heartbeats with.
func (s *standbySupervisor) fetchPresence(ctx context.Context) (map[string]controlplane.Machine, error) {
	requestCtx, cancel := context.WithTimeout(ctx, standbyFetchTimeout)
	defer cancel()
	endpoint := fmt.Sprintf("%s/v1/organizations/%s/machines", s.controlURL, s.organizationID)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+s.apiKey)
	response, err := s.presenceClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return nil, fmt.Errorf("control plane answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var machines []controlplane.Machine
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&machines); err != nil {
		return nil, err
	}
	byID := make(map[string]controlplane.Machine, len(machines))
	for _, machine := range machines {
		byID[machine.MachineID] = machine
	}
	return byID, nil
}

// failover restores the duty's checkpoint here and marks the duty done. It
// is the one code path both the automatic watch and the operator trigger
// reach, so it re-reads the duty under the lock: whichever gets there first
// wins, and the second caller finds a workload that already runs here.
// checkpointID overrides the duty's newest (empty means newest); the operator
// trigger uses it to restore a specific point in time. Lazy starts the
// failed-over process before its memory is fully resident — an operator's
// explicit trade of predictability for recovery speed, never the automatic
// path's choice.
func (s *standbySupervisor) failover(ctx context.Context, workloadID, checkpointID string, lazy bool, reason string) (model.StandbyDuty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	duty, err := s.duties.Get(workloadID)
	if err != nil {
		return duty, err
	}
	if duty.State == model.StandbyFailedOver {
		return duty, nil
	}
	if checkpointID == "" {
		checkpointID = duty.LastCheckpointID
	}
	if checkpointID == "" {
		return duty, errors.New("standby duty holds no checkpoint to restore")
	}
	if workload, getErr := s.runtime.Get(workloadID); getErr == nil && workload.Status == model.WorkloadRunning {
		// The workload already runs here — a prior failover or a migration
		// brought it. Restoring again would freeze a running process for
		// nothing; record what actually happened instead.
		return s.markFailedOver(duty, "", "workload already running locally")
	}
	record, err := s.restorer.Prepare(ctx, checkpointID, checkpoint.PrepareOptions{Timeout: failoverRestoreWait, Lazy: lazy})
	if err != nil {
		return duty, fmt.Errorf("restore prepare: %w", err)
	}
	committed, err := s.restorer.Commit(record.ID)
	if err != nil {
		_, _ = s.restorer.Rollback(context.WithoutCancel(ctx), record.ID, "standby failover commit failed")
		return duty, fmt.Errorf("restore commit: %w", err)
	}
	// The restored spec carries the failover policy it had on the source —
	// pointing at a dead machine, or at this one. This machine is the
	// workload's home now; a standby replicating to itself is a loop, so the
	// policy is dropped. The checkpoint policy survives, so the workload
	// keeps its schedule — and its protection — here.
	if _, err := s.runtime.SetFailoverPolicy(workloadID, nil); err != nil {
		s.logger.Warn("restored workload's failover policy could not be dropped", "workload_id", workloadID, "error", err)
	}
	duty, err = s.markFailedOver(duty, committed.ID, reason)
	if err != nil {
		return duty, err
	}
	s.logger.Info("workload failed over to this standby",
		"workload_id", workloadID, "checkpoint_id", checkpointID,
		"restore_id", committed.ID, "reason", reason)
	return duty, nil
}

// markFailedOver persists the terminal duty state. The restore's own record
// carries the detailed outcome; the duty carries what an operator reading
// `standby list` needs — when, from what checkpoint, and why.
func (s *standbySupervisor) markFailedOver(duty model.StandbyDuty, restoreID, reason string) (model.StandbyDuty, error) {
	now := time.Now().UTC()
	duty.State = model.StandbyFailedOver
	duty.FailoverAt = now
	duty.FailoverRestoreID = restoreID
	duty.FailoverReason = reason
	duty.LastFailoverError = ""
	duty.NextAttemptAt = time.Time{}
	duty.UpdatedAt = now
	return duty, s.duties.Put(duty.WorkloadID, duty)
}

// recordAttemptFailure backs off a failing automatic failover and keeps the
// failure on the duty, where `standby list` shows it. A restore that keeps
// failing (missing chunks, no room, incompatible state) must not hammer the
// machine every tick, and must not be silenced either.
func (s *standbySupervisor) recordAttemptFailure(duty model.StandbyDuty, cause error) {
	duty.LastFailoverError = cause.Error()
	duty.NextAttemptAt = time.Now().UTC().Add(failoverRetryDelay)
	duty.UpdatedAt = time.Now().UTC()
	if err := s.duties.Put(duty.WorkloadID, duty); err != nil {
		s.logger.Error("standby duty persist failed", "workload_id", duty.WorkloadID, "error", err)
	}
}

// Trigger is the operator's explicit failover: no death confirmation, no
// probes — the command is the confirmation. The caller owns communicating
// what that means (a live source means two running copies); this method
// only refuses to act on a duty that does not exist. Lazy is the operator's
// choice to start the workload before its memory is resident.
func (s *standbySupervisor) Trigger(ctx context.Context, workloadID, checkpointID string, lazy bool, reason string) (model.StandbyDuty, error) {
	if _, err := s.duties.Get(workloadID); err != nil {
		return model.StandbyDuty{}, err
	}
	return s.failover(ctx, workloadID, checkpointID, lazy, reason)
}
