package agent

// standby.go is the receiving half of warm-standby failover. The peer server
// hands this file's supervisor every replication session it parks in the
// HELD state; the supervisor turns those into duty records, enforces the
// source's retention, and watches each armed duty for the source's death.
// Failover is deliberately hard to trigger: the standby restores only when
// the source's peer listener is unreachable AND the control plane's record
// for it is stale or offline — both signals sustained across consecutive
// supervision passes — or when an operator explicitly commands it. Every
// automatic restore first takes control-plane ownership and positively
// fences the source through the configured fencing provider; a source that
// keeps answering is never fenced, and with no fencing provider the
// automatic path refuses to act at all. The operator trigger is the one
// genuinely unfenced path: the command is the confirmation, and a live
// source that returns finds a second running copy the operator must resolve.

import (
	"bytes"
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
	"shift.dev/shift/internal/fencing"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/persistence"
	shiftruntime "shift.dev/shift/internal/runtime"
	"shift.dev/shift/internal/transfer"
)

// Supervisor cadence and thresholds. The tick is short so death detection is
// bounded by the control plane's staleness, not by this loop; the staleness
// bound itself is three default reporter intervals, because a machine that
// stopped heartbeating for that long is the strongest absence signal the
// control plane can give.
type ownershipAcquireResponse struct {
	Epoch          uint64     `json:"epoch"`
	Token          string     `json:"token"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
}

type ownershipFencedRequest struct {
	WorkloadID string `json:"workload_id"`
	MachineID  string `json:"machine_id"`
	Epoch      uint64 `json:"epoch"`
	Token      string `json:"token"`
}

const (
	standbyTick         = 10 * time.Second
	presenceStaleAfter  = 90 * time.Second
	standbyProbeTimeout = 10 * time.Second
	standbyFetchTimeout = 10 * time.Second
	failoverRestoreWait = 10 * time.Minute
	failoverRetryDelay  = time.Minute
	// deathSampleTarget is how many consecutive supervision passes must see
	// both death signals — a stale or offline control-plane record AND a peer
	// listener that fails to answer — before an automatic failover is even
	// attempted. One failed probe is a transient blip, not a death: the
	// source may be under load or slow, and the fence that follows a wrong
	// call kills a healthy machine. Death must be a sustained observation.
	deathSampleTarget = 3
)

var (
	// errFailoverInProgress refuses a second concurrent failover of the same
	// workload: the automatic watch and an operator trigger can race, and the
	// second caller finds the first one already restoring.
	errFailoverInProgress = errors.New("a failover for this workload is already running")
	// errForeignCheckpoint refuses a checkpoint that holds another workload's
	// state. The restore runs under the manifest's own workload, so a
	// mismatched id would restore and start that other workload while this
	// duty claims success.
	errForeignCheckpoint = errors.New("checkpoint belongs to a different workload")
	// errDutyTerminal is an internal skip: the duty record reached its
	// terminal failed-over state and a stale writer must not clobber it.
	errDutyTerminal = errors.New("duty is already failed over")
)

// dutyStore is the duty record's persistence. The real store is the encrypted
// collection; tests substitute one that fails on demand, because "a hold
// whose duty cannot be persisted must not be answered as durable" is only
// observable when Put fails.
type dutyStore interface {
	Get(workloadID string) (model.StandbyDuty, error)
	Put(workloadID string, duty model.StandbyDuty) error
	Delete(workloadID string) error
	List() []model.StandbyDuty
}

// restoreFunc runs the slow half of a failover — prepare and commit — and
// returns the committed restore record. It is a field so tests can hold it
// open and prove the duty lock is not held across it.
type restoreFunc func(ctx context.Context, checkpointID string, lazy bool) (checkpoint.RestoreRecord, error)

// standbySupervisor owns the duty records. It implements the peer server's
// ReplicationWatcher, so its callbacks run on the server's handler
// goroutine and do only the one durable write a hold needs; everything slow
// — retention, presence queries, probes, the failover itself — belongs to
// the loop. Every duty mutation is a locked read-modify-write against the
// freshly read record: peer callbacks, failure bookkeeping, and the failover
// itself all touch the same record and must never overwrite each other's
// fields with a stale copy.
type standbySupervisor struct {
	duties      dutyStore
	runtime     *shiftruntime.Manager
	checkpoints *checkpoint.Service
	restorer    *checkpoint.Restorer
	restore     restoreFunc
	// sessions is the peer server's transfer store. The supervisor uses it
	// for exactly one thing: degrading a hold whose duty could not be
	// persisted, so the source learns its push did not land and retries.
	sessions *transfer.Sessions
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
	fencer     fencing.Provider
	machineID  string
	logger     *slog.Logger
	mu         sync.Mutex
	// deathSamples counts consecutive passes in which both death signals
	// held for a workload; failoverRunning names the workloads a restore is
	// currently running for. Both are guarded by mu.
	deathSamples    map[string]int
	failoverRunning map[string]bool
}

// openStandbySupervisor loads (or creates) the encrypted duty collection.
func openStandbySupervisor(duties dutyStore, sessions *transfer.Sessions, runtimeManager *shiftruntime.Manager, checkpoints *checkpoint.Service, restorer *checkpoint.Restorer, controlURL, organizationID, apiKey, machineID string, peerClient func(string) (*transfer.Client, error), fencer fencing.Provider, logger *slog.Logger) *standbySupervisor {
	presenceClient := &http.Client{Timeout: standbyFetchTimeout}
	supervisor := &standbySupervisor{
		duties: duties, sessions: sessions, runtime: runtimeManager, checkpoints: checkpoints, restorer: restorer,
		controlURL:     strings.TrimRight(strings.TrimSpace(controlURL), "/"),
		organizationID: strings.TrimSpace(organizationID),
		apiKey:         strings.TrimSpace(apiKey),
		presenceClient: presenceClient, peerClient: peerClient, fencer: fencer, machineID: strings.TrimSpace(machineID),
		logger:          logger,
		deathSamples:    map[string]int{},
		failoverRunning: map[string]bool{},
	}
	supervisor.restore = supervisor.restoreCheckpoint
	return supervisor
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

// updateDutyLocked is the one way a duty record is written: it re-reads the
// stored record, applies the mutation to that fresh copy, and persists it.
// A stale snapshot — the tick loop's list, a failure path's leftover copy —
// is never written back over a concurrent hold or failover. Callers hold mu.
func (s *standbySupervisor) updateDutyLocked(workloadID string, mutate func(*model.StandbyDuty) error) (model.StandbyDuty, error) {
	duty, err := s.duties.Get(workloadID)
	if err != nil {
		return duty, err
	}
	if err := mutate(&duty); err != nil {
		return duty, err
	}
	if err := s.duties.Put(workloadID, duty); err != nil {
		return duty, err
	}
	return duty, nil
}

// ReplicationHeld records a session the peer server parked in the HELD
// state: the checkpoint is imported, verified, and restorable from here on.
// The duty it upserts is the fact the rest of the file reasons from — and
// the hold itself is only durable once that record lands. A duty that cannot
// be persisted degrades the session instead of answering a silent success:
// the source's ledger then records the failure and retries, rather than
// believing a duty watch exists that never will.
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
	duty.FencingProvider = session.FencingProvider
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
		s.logger.Error("standby duty persist failed; degrading the hold so the source retries", "workload_id", duty.WorkloadID, "error", err)
		s.degradeHold(session)
		return
	}
	s.logger.Info("standby duty updated",
		"workload_id", duty.WorkloadID, "checkpoint_id", duty.LastCheckpointID,
		"source", duty.SourceMachineID, "keep_last", duty.KeepLast)
}

// degradeHold undoes the promise a hold makes. The duty record is the fact
// the death watch and retention reason from; a hold without it is partial
// state, and partial state answered as success is how a source comes to
// believe it is protected when nothing watches it. The session is marked
// failed so the source's post-hold confirmation sees the degradation and its
// ledger records the failure.
func (s *standbySupervisor) degradeHold(session transfer.Session) {
	if s.sessions == nil {
		return
	}
	if _, err := s.sessions.UpdateCleanup(session.ID, session.SourceMachineID, func(value *transfer.Session) error {
		value.State = transfer.SessionFailed
		value.FailureReason = "standby could not record the duty for this hold"
		return nil
	}); err != nil {
		s.logger.Error("could not degrade an undurable hold", "session_id", session.ID, "error", err)
	}
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
	delete(s.deathSamples, workloadID)
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
// and a probe failure can be this standby's own misconfiguration — and the
// two agreeing signals must persist across consecutive passes before the
// fence and the restore run: death is a sustained observation, never a
// single ten-second sample.
func (s *standbySupervisor) considerFailover(ctx context.Context, duty model.StandbyDuty, record controlplane.Machine, found bool) {
	if !found {
		s.logger.Warn("control plane has no record of a standby duty's source; automatic failover cannot confirm its death — register the source machine or trigger the failover explicitly",
			"workload_id", duty.WorkloadID, "source", duty.SourceMachineID)
		return
	}
	if !presenceOffline(record) {
		// The source is heartbeating: whatever the probes saw before, the
		// observation is contradicted and the death count starts over.
		s.clearDeathSample(duty.WorkloadID)
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
		s.clearDeathSample(duty.WorkloadID)
		return
	}
	if !s.recordDeathSample(duty.WorkloadID) {
		s.logger.Debug("source unreachable but death is not yet a sustained observation; waiting for more passes",
			"workload_id", duty.WorkloadID, "source", duty.SourceMachineID)
		return
	}
	if s.fencer == nil {
		s.logger.Warn("automatic failover blocked because no fencing provider is configured", "workload_id", duty.WorkloadID)
		s.recordAttemptFailure(duty.WorkloadID, fencing.ErrAutomaticFailoverBlocked)
		return
	}
	ownership, ownershipErr := s.acquireOwnership(ctx, duty.WorkloadID)
	if ownershipErr != nil {
		s.logger.Warn("automatic failover blocked because ownership could not be acquired", "workload_id", duty.WorkloadID, "error", ownershipErr)
		s.recordAttemptFailure(duty.WorkloadID, ownershipErr)
		return
	}
	fenceResult, fenceErr := s.fencer.Fence(ctx, fencing.Request{WorkloadID: duty.WorkloadID, SourceMachineID: duty.SourceMachineID, Epoch: ownership.Epoch})
	// The fencing evidence is duty history whichever way this attempt ends.
	// It is written now — not only on the failure paths — so a successful
	// failover keeps its STONITH record too instead of dropping it.
	s.recordFencingEvidence(duty.WorkloadID, ownership, string(fenceResult))
	if fenceErr != nil || !fencing.AllowsAutomatic(fenceResult) {
		if fenceErr == nil {
			fenceErr = fencing.ErrAutomaticFailoverBlocked
		}
		s.logger.Warn("automatic failover blocked because fencing was not positively confirmed", "workload_id", duty.WorkloadID, "provider", s.fencer.Name(), "result", fenceResult, "error", fenceErr)
		s.recordAttemptFailure(duty.WorkloadID, fenceErr)
		return
	}
	if err := s.markOwnershipFenced(ctx, duty.WorkloadID, ownership); err != nil {
		s.logger.Warn("automatic failover blocked because ownership fence state could not be persisted", "workload_id", duty.WorkloadID, "error", err)
		s.recordAttemptFailure(duty.WorkloadID, err)
		return
	}
	reason := fmt.Sprintf("source peer listener unreachable across %d passes, fencing=%s, and the control plane's record is %s", deathSampleTarget, fenceResult, presenceDescription(record))
	// An automatic failover always restores eagerly: nobody opted into the
	// lazy trade-off, and an unattended recovery must be the predictable one.
	if _, err := s.failover(ctx, duty.WorkloadID, "", false, reason); err != nil {
		s.logger.Error("automatic failover failed", "workload_id", duty.WorkloadID, "error", err)
		s.recordAttemptFailure(duty.WorkloadID, err)
	}
}

// recordDeathSample adds one sustained observation of both death signals and
// reports whether the target has been reached. The count saturates at the
// target and stays there until contradicted, so a backed-off retry does not
// have to prove the death again. Callers hold no lock; mu guards the map.
func (s *standbySupervisor) recordDeathSample(workloadID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deathSamples[workloadID] < deathSampleTarget {
		s.deathSamples[workloadID]++
	}
	return s.deathSamples[workloadID] >= deathSampleTarget
}

// clearDeathSample forgets a workload's death observations after contrary
// evidence: the source is heartbeating or answering its peer listener again.
func (s *standbySupervisor) clearDeathSample(workloadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deathSamples, workloadID)
}

// recordFencingEvidence persists the ownership epoch, token, and fence
// result on the duty record — the audit trail of a STONITH event — whether
// or not the attempt that produced them succeeds.
func (s *standbySupervisor) recordFencingEvidence(workloadID string, ownership ownershipAcquireResponse, fenceResult string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.updateDutyLocked(workloadID, func(duty *model.StandbyDuty) error {
		duty.OwnershipEpoch = ownership.Epoch
		duty.OwnershipToken = ownership.Token
		duty.FenceResult = fenceResult
		duty.UpdatedAt = time.Now().UTC()
		return nil
	}); err != nil {
		s.logger.Error("standby duty persist failed", "workload_id", workloadID, "error", err)
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

// acquireOwnership takes the control plane's workload ownership lease for
// this machine — the monotone epoch an automatic failover fences and restores
// under. Without the control-plane identity it refuses: an unattended
// failover must never run outside the ownership protocol.
func (s *standbySupervisor) acquireOwnership(ctx context.Context, workloadID string) (ownershipAcquireResponse, error) {
	if s.controlURL == "" || s.organizationID == "" || s.apiKey == "" || s.machineID == "" {
		return ownershipAcquireResponse{}, fencing.ErrAutomaticFailoverBlocked
	}
	body, err := json.Marshal(map[string]any{"workload_id": workloadID, "machine_id": s.machineID, "lease_seconds": 120})
	if err != nil {
		return ownershipAcquireResponse{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, standbyFetchTimeout)
	defer cancel()
	endpoint := fmt.Sprintf("%s/v1/organizations/%s/ownership/acquire", s.controlURL, s.organizationID)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ownershipAcquireResponse{}, err
	}
	request.Header.Set("Authorization", "Bearer "+s.apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.presenceClient.Do(request)
	if err != nil {
		return ownershipAcquireResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return ownershipAcquireResponse{}, fmt.Errorf("ownership acquire answered %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	var result ownershipAcquireResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return ownershipAcquireResponse{}, err
	}
	if result.Epoch == 0 || result.Token == "" {
		return ownershipAcquireResponse{}, errors.New("ownership acquire returned no epoch or token")
	}
	return result, nil
}

func (s *standbySupervisor) markOwnershipFenced(ctx context.Context, workloadID string, ownership ownershipAcquireResponse) error {
	if s.controlURL == "" || s.organizationID == "" || s.apiKey == "" || s.machineID == "" {
		return fencing.ErrAutomaticFailoverBlocked
	}
	body, err := json.Marshal(ownershipFencedRequest{WorkloadID: workloadID, MachineID: s.machineID, Epoch: ownership.Epoch, Token: ownership.Token})
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, standbyFetchTimeout)
	defer cancel()
	endpoint := fmt.Sprintf("%s/v1/organizations/%s/ownership/fence", s.controlURL, s.organizationID)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+s.apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.presenceClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("ownership fence answered %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	return nil
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
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+s.apiKey)
	response, err := s.presenceClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
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
// reach. Only the claim and the final state flip touch the duty lock: the
// restore itself runs with no lock held, so a live source's hold/withdraw
// callbacks and a concurrent operator request keep flowing while it works —
// the lock discipline the ReplicationWatcher contract promises. checkpointID
// overrides the duty's newest (empty means newest); the operator trigger
// uses it to restore a specific point in time. Lazy starts the failed-over
// process before its memory is fully resident — an operator's explicit
// trade of predictability for recovery speed, never the automatic path's
// choice.
func (s *standbySupervisor) failover(ctx context.Context, workloadID, checkpointID string, lazy bool, reason string) (model.StandbyDuty, error) {
	duty, claimed, err := s.beginFailover(workloadID, checkpointID)
	if err != nil {
		return duty, err
	}
	if !claimed {
		// The duty already reached its terminal failed-over state.
		return duty, nil
	}
	selected := checkpointID
	if selected == "" {
		selected = duty.LastCheckpointID
	}
	defer s.endFailover(workloadID)
	// The manifest is the authority on which workload a checkpoint holds,
	// and the restore runs under the manifest's workload. A checkpoint id
	// from a different workload would restore and start that other workload
	// while this duty claims success — refuse before anything is prepared.
	manifest, err := s.checkpoints.Load(selected)
	if err != nil {
		return duty, fmt.Errorf("restore prepare: %w", err)
	}
	if manifest.Workload.ID != duty.WorkloadID {
		return duty, fmt.Errorf("%w: checkpoint %s holds workload %s, not %s", errForeignCheckpoint, selected, manifest.Workload.ID, duty.WorkloadID)
	}
	if workload, getErr := s.runtime.Get(workloadID); getErr == nil && workload.Status == model.WorkloadRunning {
		// The workload already runs here — a prior failover or a migration
		// brought it. Restoring again would freeze a running process for
		// nothing; record what actually happened instead.
		return s.markFailedOver(workloadID, "", "workload already running locally")
	}
	committed, err := s.restore(ctx, selected, lazy)
	if err != nil {
		return duty, err
	}
	// The restored spec carries the failover policy it had on the source —
	// pointing at a dead machine, or at this one. This machine is the
	// workload's home now; a standby replicating to itself is a loop, so the
	// policy is dropped. The checkpoint policy survives, so the workload
	// keeps its schedule — and its protection — here.
	if _, err := s.runtime.SetFailoverPolicy(workloadID, nil); err != nil {
		s.logger.Warn("restored workload's failover policy could not be dropped", "workload_id", workloadID, "error", err)
	}
	duty, err = s.markFailedOver(workloadID, committed.ID, reason)
	if err != nil {
		return duty, err
	}
	s.logger.Info("workload failed over to this standby",
		"workload_id", workloadID, "checkpoint_id", selected,
		"restore_id", committed.ID, "reason", reason)
	return duty, nil
}

// beginFailover validates and claims the failover under the duty lock: one
// failover per workload at a time, and never one that races a completed
// failover's terminal record. The claim is what lets the slow restore run
// unlocked without a second caller starting a second restore. claimed is
// false exactly when the duty already failed over — nothing left to do.
func (s *standbySupervisor) beginFailover(workloadID, checkpointID string) (model.StandbyDuty, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	duty, err := s.duties.Get(workloadID)
	if err != nil {
		return duty, false, err
	}
	if duty.State == model.StandbyFailedOver {
		return duty, false, nil
	}
	if s.failoverRunning[workloadID] {
		return duty, false, errFailoverInProgress
	}
	if checkpointID == "" {
		checkpointID = duty.LastCheckpointID
	}
	if checkpointID == "" {
		return duty, false, errors.New("standby duty holds no checkpoint to restore")
	}
	s.failoverRunning[workloadID] = true
	return duty, true, nil
}

// endFailover releases the failover claim.
func (s *standbySupervisor) endFailover(workloadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failoverRunning, workloadID)
}

// restoreCheckpoint is the slow half of a failover: prepare the checkpoint
// and commit the restore. It runs without the duty lock held.
func (s *standbySupervisor) restoreCheckpoint(ctx context.Context, checkpointID string, lazy bool) (checkpoint.RestoreRecord, error) {
	record, err := s.restorer.Prepare(ctx, checkpointID, checkpoint.PrepareOptions{Timeout: failoverRestoreWait, Lazy: lazy})
	if err != nil {
		return record, fmt.Errorf("restore prepare: %w", err)
	}
	committed, err := s.restorer.Commit(record.ID)
	if err != nil {
		_, _ = s.restorer.Rollback(context.WithoutCancel(ctx), record.ID, "standby failover commit failed")
		return committed, fmt.Errorf("restore commit: %w", err)
	}
	return committed, nil
}

// markFailedOver persists the terminal duty state. The restore's own record
// carries the detailed outcome; the duty carries what an operator reading
// `standby list` needs — when, from what checkpoint, and why. The write is a
// read-modify-write against the stored record, so a concurrent hold's fresh
// facts survive — and a duty that is already failed over keeps its original
// history instead of being overwritten with a later, different reason.
func (s *standbySupervisor) markFailedOver(workloadID, restoreID, reason string) (model.StandbyDuty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	duty, err := s.updateDutyLocked(workloadID, func(duty *model.StandbyDuty) error {
		if duty.State == model.StandbyFailedOver {
			return errDutyTerminal
		}
		duty.State = model.StandbyFailedOver
		duty.FailoverAt = now
		duty.FailoverRestoreID = restoreID
		duty.FailoverReason = reason
		duty.LastFailoverError = ""
		duty.NextAttemptAt = time.Time{}
		duty.UpdatedAt = now
		return nil
	})
	if errors.Is(err, errDutyTerminal) {
		return duty, nil
	}
	return duty, err
}

// recordAttemptFailure backs off a failing automatic failover and keeps the
// failure on the duty, where `standby list` shows it. A restore that keeps
// failing (missing chunks, no room, incompatible state) must not hammer the
// machine every tick, and must not be silenced either. The write lands on
// the freshly read record: a concurrent hold's newer checkpoint or a
// completed failover's terminal state is never clobbered by this stale
// failure.
func (s *standbySupervisor) recordAttemptFailure(workloadID string, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.updateDutyLocked(workloadID, func(duty *model.StandbyDuty) error {
		if duty.State == model.StandbyFailedOver {
			return errDutyTerminal
		}
		duty.LastFailoverError = cause.Error()
		duty.NextAttemptAt = time.Now().UTC().Add(failoverRetryDelay)
		duty.UpdatedAt = time.Now().UTC()
		return nil
	}); err != nil && !errors.Is(err, errDutyTerminal) {
		s.logger.Error("standby duty persist failed", "workload_id", workloadID, "error", err)
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
