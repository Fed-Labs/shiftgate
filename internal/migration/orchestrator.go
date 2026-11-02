package migration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/compatibility"
	"shift.dev/shift/internal/endpoint"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/network"
	"shift.dev/shift/internal/observability"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	shiftruntime "shift.dev/shift/internal/runtime"
	"shift.dev/shift/internal/securestore"
	"shift.dev/shift/internal/transfer"
)

type PeerClient interface {
	Machine(context.Context) (model.MachineCapabilities, error)
	Reserve(context.Context, transfer.ReserveRequest) (transfer.Session, error)
	ImportKey(context.Context, string, string, uint32, []byte) error
	ImportManifest(context.Context, string, model.CheckpointManifest) (transfer.MissingResponse, error)
	UploadChunk(context.Context, string, model.ChunkRef, *chunkstore.Store) error
	Verify(context.Context, string) (transfer.Session, error)
	Restore(context.Context, string, time.Duration) (checkpoint.RestoreRecord, error)
	Commit(context.Context, string) (transfer.Session, error)
	Rollback(context.Context, string) (transfer.Session, error)
	Get(context.Context, string) (transfer.Session, error)
	// GetForCleanup reads sessions the way rollback does: expiry does not
	// hide them, so a recovery probe cannot mistake an expired reservation
	// for "no destination state" while a restored workload is still live.
	GetForCleanup(context.Context, string) (transfer.Session, error)
}

type ClientFactory func(model.Destination) (PeerClient, error)

// WriteGuard is the in-process checkpoint-write exclusion the agent's online
// GC collector takes exclusively around reference scans and deletes (its
// gcGuard). The orchestrator holds it shared across each capture window —
// chunk write through manifest install — so a sweep cannot delete chunks the
// capture is mid-way through referencing (report 42). It is optional: without
// it the collector's grace floors are the only protection for long captures.
type WriteGuard interface {
	RLock()
	RUnlock()
}

type CreateRequest struct {
	WorkloadID string `json:"workload_id"`
	// IdempotencyKey is scoped to the source agent's migration records. A
	// coordinator can safely retry after a lost response without duplicating
	// the transfer.
	IdempotencyKey string              `json:"idempotency_key,omitempty"`
	Destination    model.Destination   `json:"destination"`
	Mode           model.MigrationMode `json:"mode"`
	// PreCopyPasses caps the pre-dump iterations of a live migration's
	// pre-copy loop (0 = the default single pass). Ignored for cold
	// migrations, which freeze the source immediately.
	PreCopyPasses int           `json:"pre_copy_passes,omitempty"`
	Timeout       time.Duration `json:"timeout"`
}

type Orchestrator struct {
	mu             sync.Mutex
	records        *securestore.EncryptedCollection[model.Migration]
	freezes        *securestore.EncryptedCollection[migrationFreeze]
	runtime        *shiftruntime.Manager
	checkpoints    *checkpoint.Service
	checkpointRepo *checkpoint.Repository
	chunks         *chunkstore.Store
	keys           *securestore.Manager
	identity       *identity.Identity
	inventory      *linuxplatform.Inventory
	clients        ClientFactory
	sourceNetwork  SourceNetwork
	writeGuard     WriteGuard
	diagnostics    *observability.Diagnostics
	tracer         *observability.Tracer
	logger         *slog.Logger
	semaphore      chan struct{}
	cancel         map[string]context.CancelFunc
}

func Open(stateDir string, keys *securestore.Manager, runtimeManager *shiftruntime.Manager, checkpointService *checkpoint.Service, checkpointRepo *checkpoint.Repository, chunks *chunkstore.Store, machineIdentity *identity.Identity, inventory *linuxplatform.Inventory, clients ClientFactory, maxConcurrent int, logger *slog.Logger) (*Orchestrator, error) {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	if logger == nil {
		logger = slog.Default()
	}
	records, err := securestore.OpenEncryptedCollection[model.Migration](stateDir+"/metadata/migrations.enc.json", "migration-orchestration-v1", keys)
	if err != nil {
		return nil, err
	}
	freezes, err := securestore.OpenEncryptedCollection[migrationFreeze](stateDir+"/metadata/migration-freezes.enc.json", "migration-freeze-v1", keys)
	if err != nil {
		return nil, err
	}
	return &Orchestrator{
		records: records, freezes: freezes, runtime: runtimeManager, checkpoints: checkpointService,
		checkpointRepo: checkpointRepo, chunks: chunks, keys: keys, identity: machineIdentity,
		inventory: inventory, clients: clients, logger: logger,
		semaphore: make(chan struct{}, maxConcurrent), cancel: make(map[string]context.CancelFunc),
	}, nil
}

// SetDiagnostics attaches the operational recorder that turns persisted
// migration outcomes into metrics series.
func (o *Orchestrator) SetDiagnostics(diagnostics *observability.Diagnostics) {
	o.diagnostics = diagnostics
}

// SetTracer attaches the tracer spanning a migration's whole run. The trace
// begins at the API request that created the migration and is carried into the
// background run goroutine and every peer call.
func (o *Orchestrator) SetTracer(tracer *observability.Tracer) {
	o.tracer = tracer
}

// SetWriteGuard attaches the agent's in-process GC write guard. It must be
// called before any migration starts.
func (o *Orchestrator) SetWriteGuard(guard WriteGuard) {
	o.writeGuard = guard
}

// lockWrites acquires the GC write guard for one capture window; the returned
// function releases it.
func (o *Orchestrator) lockWrites() func() {
	if o.writeGuard == nil {
		return func() {}
	}
	o.writeGuard.RLock()
	return o.writeGuard.RUnlock
}

func (o *Orchestrator) Create(parent context.Context, request CreateRequest) (model.Migration, error) {
	if request.Mode == "" {
		request.Mode = model.MigrationCold
	}
	if request.WorkloadID == "" {
		return model.Migration{}, errors.New("workload id is required")
	}
	normalizedEndpoint, err := endpoint.Normalize(request.Destination.AgentURL)
	if err != nil {
		return model.Migration{}, err
	}
	request.Destination.AgentURL = normalizedEndpoint
	serverName, err := endpoint.ServerName(normalizedEndpoint, request.Destination.ServerName)
	if err != nil {
		return model.Migration{}, err
	}
	request.Destination.ServerName = serverName
	if request.Mode != model.MigrationCold && request.Mode != model.MigrationLive {
		return model.Migration{}, fmt.Errorf("unsupported migration mode %q", request.Mode)
	}
	workload, err := o.runtime.Get(request.WorkloadID)
	if err != nil {
		return model.Migration{}, err
	}
	if workload.Status != model.WorkloadRunning && workload.Status != model.WorkloadPaused {
		return model.Migration{}, fmt.Errorf("workload must be running or paused, got %s", workload.Status)
	}
	// Admission is serialized: the idempotency scan and the record insert run
	// under one lock, so concurrent Creates with the same key cannot both
	// start, and a second migration can never be admitted for a workload that
	// already has one in flight. Two live migrations of one workload would
	// fight over its freeze: the loser's failure path must never SIGCONT the
	// process the winner is mid-dump on.
	o.mu.Lock()
	for _, existing := range o.records.List() {
		if existing.WorkloadID != request.WorkloadID && (request.IdempotencyKey == "" || existing.IdempotencyKey != request.IdempotencyKey) {
			continue
		}
		if request.IdempotencyKey != "" && existing.IdempotencyKey == request.IdempotencyKey {
			if existing.WorkloadID != request.WorkloadID || !destinationMatches(request.Destination, existing.Destination) || existing.Mode != request.Mode || existing.PreCopyPasses != request.PreCopyPasses {
				o.mu.Unlock()
				return model.Migration{}, ErrIdempotencyConflict
			}
			o.mu.Unlock()
			return existing, nil
		}
		if !terminal(existing.Stage) {
			o.mu.Unlock()
			return model.Migration{}, ErrMigrationAlreadyActive
		}
	}
	id, err := model.NewID()
	if err != nil {
		o.mu.Unlock()
		return model.Migration{}, err
	}
	now := time.Now().UTC()
	migration := model.Migration{
		ID: id, IdempotencyKey: request.IdempotencyKey, WorkloadID: workload.Spec.ID, SourceMachineID: o.identity.Machine.ID,
		Destination: request.Destination, Mode: request.Mode, Stage: model.MigrationCreated,
		PreCopyPasses: request.PreCopyPasses,
		CreatedAt:     now, UpdatedAt: now, SourcePreserved: true, Revision: 1,
		Events: []model.MigrationEvent{{Sequence: 1, Stage: model.MigrationCreated, Message: "migration created", Timestamp: now, Progress: 0}},
	}
	if err := o.records.Put(id, migration); err != nil {
		o.mu.Unlock()
		return model.Migration{}, err
	}
	o.mu.Unlock()
	// The migration outlives the API request that created it, so the run
	// goroutine gets a fresh context — carrying the request's trace, so the
	// whole migration stays in one trace view across agent and collector.
	runContext := context.Background()
	if trace, ok := observability.TraceFromContext(parent); ok {
		runContext = observability.ContextWithTrace(runContext, trace)
	}
	ctx, cancel := context.WithCancel(runContext)
	o.mu.Lock()
	o.cancel[id] = cancel
	o.mu.Unlock()
	go o.run(ctx, id, request.Timeout)
	return migration, nil
}

// Preflight failures that carry the same codes a real migration would fail
// with, so a dry run never invents its own vocabulary.
var (
	ErrDestinationUnreachable      = errors.New("destination unreachable")
	ErrDestinationIsSource         = errors.New("source and destination resolve to the same machine identity")
	ErrDestinationIdentityMismatch = errors.New("destination certificate does not match the requested machine")
	ErrIdempotencyConflict         = errors.New("migration idempotency key was reused with different parameters")
	// ErrMigrationAlreadyActive rejects a second migration for a workload that
	// already has one in flight: two live migrations of one workload would
	// both try to freeze and move it.
	ErrMigrationAlreadyActive = errors.New("workload already has an active migration")
)

const (
	// rollbackAttempts / rollbackRetryBackoff give a transient rollback
	// failure — a one-second network blip to the destination — seconds of
	// retry budget before it escalates, mirroring stopSourceWithRetry.
	rollbackAttempts       = 4
	rollbackRetryBackoff   = 500 * time.Millisecond
	rollbackAttemptTimeout = 2 * time.Minute
	rollbackProbeTimeout   = 20 * time.Second
	// transferRounds / transferRetryBackoff bound the frozen-window
	// transfer's re-negotiation: each round re-imports the manifest and
	// uploads only what is still missing before the attempt is declared dead.
	transferRounds       = 3
	transferRetryBackoff = 2 * time.Second
	uploadAttempts       = 5
	uploadRetryBackoff   = 500 * time.Millisecond
	// statePersistAttempts / statePersistBackoff retry the stage writes whose
	// loss would otherwise strand a record outside the stage machine.
	statePersistAttempts = 3
	statePersistBackoff  = 100 * time.Millisecond
	// maxReservationWindow matches the destination's reservation ceiling:
	// requesting more would be silently clamped there to a fraction of it,
	// and a run must never outlive its own reservation.
	maxReservationWindow = 24 * time.Hour
)

// PreflightResult is everything a dry run can state about a would-be
// migration: the two machines as the compatibility checker saw them, the
// report, and the network plan the migration would apply. Nothing is moved,
// frozen, or recorded — no migration record is created.
type PreflightResult struct {
	Workload      model.WorkloadSpec        `json:"workload"`
	SourceMachine model.MachineCapabilities `json:"source_machine"`
	Destination   model.MachineCapabilities `json:"destination"`
	Mode          model.MigrationMode       `json:"mode"`
	Report        model.CompatibilityReport `json:"report"`
	Network       model.NetworkPlan         `json:"network"`
}

// Preflight answers whether a migration would be admitted, without moving
// anything: it resolves the destination over the peer channel, gathers the
// source inventory, and runs the same compatibility check the migration's
// validate stage runs on the same inputs, plus the network plan computed
// before anything moves. An unreachable destination, a destination that is
// the source, and an identity mismatch come back as the sentinel errors —
// the same codes a real migration would fail with; everything the checker
// finds is in the report, where an error-severity issue means the migration
// would be rejected.
func (o *Orchestrator) Preflight(ctx context.Context, request CreateRequest) (PreflightResult, error) {
	if request.Mode == "" {
		request.Mode = model.MigrationCold
	}
	normalizedEndpoint, err := endpoint.Normalize(request.Destination.AgentURL)
	if err != nil {
		return PreflightResult{}, err
	}
	request.Destination.AgentURL = normalizedEndpoint
	serverName, err := endpoint.ServerName(normalizedEndpoint, request.Destination.ServerName)
	if err != nil {
		return PreflightResult{}, err
	}
	request.Destination.ServerName = serverName
	if request.Mode != model.MigrationCold && request.Mode != model.MigrationLive {
		return PreflightResult{}, fmt.Errorf("unsupported migration mode %q", request.Mode)
	}
	workload, err := o.runtime.Get(request.WorkloadID)
	if err != nil {
		return PreflightResult{}, err
	}
	if workload.Status != model.WorkloadRunning && workload.Status != model.WorkloadPaused {
		return PreflightResult{}, fmt.Errorf("workload must be running or paused, got %s", workload.Status)
	}
	// A dry run must refuse what Create refuses: the same admission rules and
	// the same idempotency verdicts, so a green preflight cannot promise a
	// migration the real request would reject. An identical same-key replay
	// is not a refusal — Create answers it with the existing record.
	o.mu.Lock()
	for _, existing := range o.records.List() {
		if request.IdempotencyKey != "" && existing.IdempotencyKey == request.IdempotencyKey {
			if existing.WorkloadID != request.WorkloadID || !destinationMatches(request.Destination, existing.Destination) || existing.Mode != request.Mode || existing.PreCopyPasses != request.PreCopyPasses {
				o.mu.Unlock()
				return PreflightResult{}, ErrIdempotencyConflict
			}
			continue
		}
		if existing.WorkloadID == request.WorkloadID && !terminal(existing.Stage) {
			o.mu.Unlock()
			return PreflightResult{}, ErrMigrationAlreadyActive
		}
	}
	o.mu.Unlock()
	peer, err := o.clients(request.Destination)
	if err != nil {
		return PreflightResult{}, fmt.Errorf("%w: %w", ErrDestinationUnreachable, err)
	}
	destination, err := peer.Machine(ctx)
	if err != nil {
		return PreflightResult{}, fmt.Errorf("%w: %w", ErrDestinationUnreachable, err)
	}
	if destination.MachineID == o.identity.Machine.ID {
		return PreflightResult{}, ErrDestinationIsSource
	}
	if request.Destination.MachineID != "" && request.Destination.MachineID != destination.MachineID {
		return PreflightResult{}, ErrDestinationIdentityMismatch
	}
	source, err := o.inventory.Inspect(ctx)
	if err != nil {
		return PreflightResult{}, fmt.Errorf("source inventory: %w", err)
	}
	manifest := model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		Workload: workload.Spec, SourceMachine: source, RequiredBytes: int64(workload.Spec.Resources.StorageBytes),
		DeviceNeeds: workload.Spec.Resources.GPUs,
	}
	report := (compatibility.Checker{}).Check(manifest, destination)
	plan, err := network.PlanFor(workload.Spec)
	if err != nil {
		return PreflightResult{}, fmt.Errorf("network plan: %w", err)
	}
	return PreflightResult{
		Workload: workload.Spec, SourceMachine: source, Destination: destination,
		Mode: request.Mode, Report: report, Network: plan,
	}, nil
}

func (o *Orchestrator) Get(id string) (model.Migration, error) {
	return o.records.Get(id)
}

func (o *Orchestrator) List() []model.Migration {
	values := o.records.List()
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.After(values[j].CreatedAt) })
	return values
}

func (o *Orchestrator) Cancel(id string) error {
	o.mu.Lock()
	cancel, ok := o.cancel[id]
	o.mu.Unlock()
	if !ok {
		migration, err := o.records.Get(id)
		if err != nil {
			return err
		}
		if terminal(migration.Stage) {
			return fmt.Errorf("migration is already %s", migration.Stage)
		}
		return errors.New("migration is not active in this agent process")
	}
	cancel()
	return nil
}

func (o *Orchestrator) Recover(ctx context.Context) {
	for _, migration := range o.records.List() {
		if terminal(migration.Stage) {
			continue
		}
		go o.recoverOne(ctx, migration.ID)
	}
}

// frozenStageRank orders the stages that imply this migration stopped the
// workload: a cold migration freezes during its snapshot; a live one freezes
// only at its final delta, after the pre-copy passes, so its ownership begins
// at TRANSFER. It is the fallback for records written before freeze ownership
// was persisted; current runs consult the persisted fact, which is anchored
// on the freeze itself — a live migration freezes inside its final
// checkpoint while the record is still in SNAPSHOT or PREPARE.
var frozenStageRank = map[model.MigrationStage]int{
	model.MigrationSnapshot:     1,
	model.MigrationPrepare:      2,
	model.MigrationTransfer:     3,
	model.MigrationVerify:       4,
	model.MigrationRestore:      5,
	model.MigrationPostValidate: 6,
	model.MigrationSwitch:       7,
	model.MigrationCommit:       8,
	model.MigrationCleanup:      9,
	model.MigrationRollingBack:  10,
}

// recoveryOwnsFreeze reports whether a crashed migration had already stopped
// its workload — the only case in which recovery may resume it. The persisted
// fact is authoritative: it is written before the freeze window opens and
// records both whether this migration owns the freeze (a workload the
// operator had paused is never owned) and that ownership began before the
// stage machine could reach TRANSFER. The stage-rank rule remains only for
// records created before the fact existed.
func recoveryOwnsFreeze(migration model.Migration, fact *migrationFreeze) bool {
	if fact != nil {
		return fact.OwnsFreeze
	}
	rank, known := frozenStageRank[migration.Stage]
	if !known {
		return false
	}
	if migration.Mode == model.MigrationLive {
		return rank >= frozenStageRank[model.MigrationTransfer]
	}
	return true
}

func (o *Orchestrator) run(parent context.Context, id string, timeout time.Duration) {
	o.semaphore <- struct{}{}
	defer func() {
		<-o.semaphore
		o.mu.Lock()
		if cancel := o.cancel[id]; cancel != nil {
			cancel()
		}
		delete(o.cancel, id)
		o.mu.Unlock()
	}()
	if timeout <= 0 {
		timeout = 2 * time.Hour
	}
	// The destination clamps any transfer reservation beyond 24h to a
	// fraction of it, so a run that outlives its own reservation would fail
	// mid-transfer on an expiry error while everything was succeeding. One
	// clamp governs the run budget and every reservation it opens.
	if timeout > maxReservationWindow {
		timeout = maxReservationWindow
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	// One span covers the whole migration; every peer call made with ctx
	// carries its trace id, so source and destination land in one trace view.
	_, span := o.tracer.Start(ctx, "migration", map[string]string{"migration_id": id})
	defer span.End()
	var peer PeerClient
	reserved := false
	committed := false
	// downtimeStart is the instant the workload actually stopped executing
	// (the final checkpoint's freeze); workloadResumedAt is the instant it was
	// provably running on the destination (the restore call resumes it and
	// validates health before returning). Both stay zero until they happen.
	downtimeStart := time.Time{}
	workloadResumedAt := time.Time{}
	// sourceFrozen records that THIS migration stopped the workload — only
	// then may its failure path resume it. A migration that loses an
	// admission race never froze anything and must not SIGCONT the process
	// the winner is mid-dump on.
	sourceFrozen := false
	fail := func(code string, cause error) {
		// Downtime is the freeze window: zero when the failure happened
		// before the workload was ever stopped. Once the destination has
		// resumed the workload the window ended there — later bookkeeping
		// failures happen while it is already running.
		downtime := time.Duration(0)
		if !downtimeStart.IsZero() {
			end := time.Now()
			if !workloadResumedAt.IsZero() {
				end = workloadResumedAt
			}
			downtime = end.Sub(downtimeStart)
		}
		// A cancel kills the CRIU child mid-checkpoint, whose error does not
		// wrap context.Canceled — the run context's own state is what says
		// this was a cancellation, not a checkpoint failure.
		if errors.Is(cause, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			o.handleCancellation(context.Background(), id, peer, reserved, committed, sourceFrozen, cause, downtime)
			return
		}
		o.handleFailure(context.Background(), id, peer, reserved, committed, sourceFrozen, code, cause, downtime)
	}

	if err := o.transition(id, model.MigrationDiscover, "discovering source and destination capabilities", 0.03); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	migration, _ := o.records.Get(id)
	workload, err := o.runtime.Get(migration.WorkloadID)
	if err != nil {
		fail("WORKLOAD_NOT_FOUND", err)
		return
	}
	// Freeze ownership is persisted before the freeze window can open: a
	// crash between the workload's freeze and the first post-freeze stage
	// must still tell restart recovery that this migration may resume it —
	// and that a workload the operator had paused is never resumed by it.
	o.noteFreezeOwnership(id, workload.Spec.ID, workload.Status == model.WorkloadRunning)
	peer, err = o.clients(migration.Destination)
	if err != nil {
		fail("DESTINATION_CLIENT_FAILED", err)
		return
	}
	destination, err := peer.Machine(ctx)
	if err != nil {
		fail("DESTINATION_UNREACHABLE", err)
		return
	}
	if destination.MachineID == o.identity.Machine.ID {
		fail("DESTINATION_IS_SOURCE", ErrDestinationIsSource)
		return
	}
	if migration.Destination.MachineID != "" && migration.Destination.MachineID != destination.MachineID {
		fail("DESTINATION_IDENTITY_MISMATCH", ErrDestinationIdentityMismatch)
		return
	}
	_ = o.update(id, func(value *model.Migration) error {
		value.Destination.MachineID = destination.MachineID
		return nil
	})
	if err := o.transition(id, model.MigrationValidate, "validating destination compatibility", 0.08); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	source, err := o.inventory.Inspect(ctx)
	if err != nil {
		fail("SOURCE_INVENTORY_FAILED", err)
		return
	}
	preflightManifest := model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		Workload: workload.Spec, SourceMachine: source, RequiredBytes: int64(workload.Spec.Resources.StorageBytes),
		DeviceNeeds: workload.Spec.Resources.GPUs,
	}
	report := (compatibility.Checker{}).Check(preflightManifest, destination)
	if err := o.update(id, func(value *model.Migration) error {
		value.Compatibility = report
		return nil
	}); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	if !report.Compatible {
		fail("DESTINATION_INCOMPATIBLE", compatibilityFailure(report))
		return
	}
	// The network plan is computed before anything moves: it records what
	// will happen to the workload's listeners and connections, and every
	// later stage — the checkpoint's TCP-state flag, the destination's port
	// establishment, and the status document the application reads —
	// follows it. An unportable port declaration fails here, not
	// mid-restore.
	networkPlan, err := network.PlanFor(workload.Spec)
	if err != nil {
		fail("NETWORK_PLAN_INVALID", err)
		return
	}
	if err := o.update(id, func(value *model.Migration) error {
		value.Network = networkPlan
		appendEvent(value, "network plan: "+networkPlan.Summary, 0.1, 0, 0)
		return nil
	}); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	snapshotMessage := "freezing source and creating checkpoint"
	if migration.Mode == model.MigrationLive {
		snapshotMessage = "running pre-copy passes while the workload keeps running"
		if workload.Status != model.WorkloadRunning {
			snapshotMessage = "workload is already paused; creating the final checkpoint directly"
		}
	}
	// A drain-policy workload stops being reachable before it is frozen:
	// its published listeners stop accepting and in-flight connections get
	// the grace period to finish, so the checkpoint captures a process
	// whose connections ended cleanly rather than mid-stream. Draining counts
	// toward migration downtime: the workload is unreachable from the moment
	// its forwarders stop.
	drainedAt := o.drainSourceForwarders(id, workload.Spec.ID, networkPlan)
	if !drainedAt.IsZero() {
		downtimeStart = drainedAt
		o.noteDowntimeStart(id, downtimeStart)
	}
	if err := o.transition(id, model.MigrationSnapshot, snapshotMessage, 0.12); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	var manifest model.CheckpointManifest
	var preCopyTransferred int64
	var live *checkpoint.LiveSession
	if migration.Mode == model.MigrationLive {
		// The pass cap travels with the migration record so it survives the
		// request that started it.
		if live, err = o.checkpoints.BeginLive(ctx, workload.Spec.ID, checkpoint.LiveOptions{
			Passes:   migration.PreCopyPasses,
			TCPState: workload.Spec.NetworkPolicy == model.NetworkPreserve,
			Timeout:  timeout / 2,
		}); err != nil {
			fail("CHECKPOINT_FAILED", err)
			return
		}
		// Abort is deferred rather than placed on each failure path: it is a
		// no-op once Finalize or Abort closed the session, and any failure
		// before that must release the staging tree and the single-checkpoint
		// guard the session holds.
		defer live.Abort()
	}
	// Downtime is the window the migration itself makes the workload
	// unreachable. A drain-policy workload went unreachable when its
	// forwarders stopped — before any freeze — and that earlier bound stands
	// for the whole run. Otherwise a cold migration freezes inside its
	// checkpoint, and a live migration of an already-paused workload cannot
	// know when that pause happened, so for both the checkpoint's start
	// remains the earliest defensible bound. A live migration of a running
	// workload freezes only inside Finalize, so until that instant there is
	// no downtime window at all — a failure before the freeze leaves the
	// workload having run the whole time.
	if downtimeStart.IsZero() && (live == nil || !live.CanPass()) {
		downtimeStart = time.Now()
		o.noteDowntimeStart(id, downtimeStart)
	}
	if live != nil && live.CanPass() {
		// Spec §10: each pass's images transfer the moment the pass completes
		// — while the workload keeps running — so the freeze at the end
		// carries only the final delta. The destination session opens here
		// rather than in a separate stage: the reservation is measured from
		// pass 1's actual packaged size instead of guessed, and the key the
		// passes are encrypted under is imported before the first chunk
		// leaves — the chunks land in KEY_READY, before any manifest exists.
		unlockWrites := o.lockWrites()
		firstPass, passErr := live.Pass(ctx)
		unlockWrites()
		if passErr != nil {
			fail("CHECKPOINT_FAILED", passErr)
			return
		}
		estimated, estimateErr := checkpoint.EstimateLiveTransferBytes(workload.Spec, firstPass.Asset.StoredSize, live.PassLimit())
		if estimateErr != nil {
			// The walk came back partial: reserve the measured part and warn.
			// The capture itself fails on the same unreadable tree, so the
			// reservation never outlives the transfer it was measured for.
			o.logger.Warn("live transfer reservation used a partial root walk",
				"migration_id", id, "workload_id", workload.Spec.ID, "error", estimateErr)
		}
		session, reserveErr := peer.Reserve(ctx, transfer.ReserveRequest{
			ID: id, SourceMachineID: o.identity.Machine.ID, WorkloadID: workload.Spec.ID,
			EstimatedBytes: estimated, ExpiresAt: reservationExpiry(timeout),
		})
		if reserveErr != nil {
			fail("DESTINATION_RESERVATION_FAILED", reserveErr)
			return
		}
		reserved = true
		dataKey, keyErr := o.keys.ExportWorkloadKey(workload.Spec.ID, live.KeyVersion())
		if keyErr != nil {
			fail("KEY_EXPORT_FAILED", keyErr)
			return
		}
		if err := peer.ImportKey(ctx, id, workload.Spec.ID, live.KeyVersion(), dataKey); err != nil {
			fail("KEY_TRANSFER_FAILED", err)
			return
		}
		o.noteReservation(id, "destination session reserved; pre-copy transfers begin while the workload keeps running", session.ExpiresAt)
		current := firstPass
		for {
			uploadStarted := time.Now()
			passBytes, uploadErr := o.uploadChunks(ctx, id, peer, current.Asset.Chunks,
				0.12+0.16*float64(current.Index-1)/float64(live.PassLimit()), 0.16/float64(live.PassLimit()),
				fmt.Sprintf("transferring pre-copy pass %d while the workload keeps running", current.Index))
			if uploadErr != nil {
				fail("CHUNK_TRANSFER_FAILED", uploadErr)
				return
			}
			if o.diagnostics != nil {
				o.diagnostics.Transferred("upload", passBytes, time.Since(uploadStarted))
			}
			preCopyTransferred += passBytes
			// Recorded per pass, not once at the end: a migration that fails
			// mid-loop still reports the bytes its passes actually moved.
			_ = o.update(id, func(value *model.Migration) error {
				value.Metrics.PreCopyTransferredBytes = preCopyTransferred
				value.Metrics.TransferredBytes = preCopyTransferred
				return nil
			})
			if !current.More {
				break
			}
			unlockWrites = o.lockWrites()
			current, passErr = live.Pass(ctx)
			unlockWrites()
			if passErr != nil {
				fail("CHECKPOINT_FAILED", passErr)
				return
			}
		}
		_ = o.progress(id, "freezing the workload for the final delta checkpoint", 0.28, 0, 0)
	}
	if live != nil && downtimeStart.IsZero() {
		// The freeze window opens with the final checkpoint: the workload is
		// about to stop, nothing beyond this point is live time, and a crash
		// inside Finalize must still bound the frozen outage from here.
		downtimeStart = time.Now()
		o.noteDowntimeStart(id, downtimeStart)
	}
	if live != nil {
		unlockWrites := o.lockWrites()
		manifest, err = live.Finalize(ctx)
		unlockWrites()
		if err != nil {
			// A freeze that happened inside the failing Finalize still counts:
			// the workload was stopped from that instant until the session
			// thawed it, and only that window is the migration's downtime —
			// never the passes, which ran while it was live. A drain-policy
			// workload went unreachable even earlier and keeps that bound.
			if frozen := live.FreezeStartedAt(); !frozen.IsZero() {
				if drainedAt.IsZero() {
					downtimeStart = frozen
					o.noteDowntimeStart(id, downtimeStart)
				}
				sourceFrozen = workload.Status == model.WorkloadRunning
			}
			fail("CHECKPOINT_FAILED", err)
			return
		}
	} else {
		// The cold dump stops the workload: from here this migration owns its
		// freeze and is the only one allowed to resume it on rollback.
		sourceFrozen = workload.Status == model.WorkloadRunning
		unlockWrites := o.lockWrites()
		manifest, err = o.checkpoints.Create(ctx, workload.Spec.ID, checkpoint.CreateOptions{
			Kind: model.CheckpointFull, LeaveRunning: false,
			TCPState: workload.Spec.NetworkPolicy == model.NetworkPreserve,
			Timeout:  timeout / 2,
		})
		unlockWrites()
		if err != nil {
			fail("CHECKPOINT_FAILED", err)
			return
		}
	}
	// Downtime begins at the instant the checkpoint actually froze the
	// workload, not at the checkpoint's start: live-mode pre-copy passes run
	// while the workload is still live and are not downtime. A workload that
	// was already paused when the migration began was frozen before this
	// migration existed, so its freeze instant is unknown and the
	// checkpoint's start remains the earliest defensible bound. A drain's
	// earlier unreachability outranks both.
	if !manifest.Metrics.FreezeStartedAt.IsZero() {
		if drainedAt.IsZero() {
			downtimeStart = manifest.Metrics.FreezeStartedAt
			o.noteDowntimeStart(id, downtimeStart)
		}
		sourceFrozen = workload.Status == model.WorkloadRunning
	}
	if err := o.update(id, func(value *model.Migration) error {
		value.CheckpointID = manifest.ID
		value.Metrics.TotalStateBytes = manifest.Metrics.PlainBytes
		value.Metrics.CheckpointDuration = manifest.Metrics.Duration
		return nil
	}); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	if !reserved {
		// A cold migration — or a live one of a workload that was already
		// paused, with nothing to pre-copy — opens the destination session
		// against the finished checkpoint, so the reservation is its exact
		// stored size.
		if err := o.transition(id, model.MigrationPrepare, "reserving destination and establishing workload key", 0.28); err != nil {
			fail("STATE_PERSIST_FAILED", err)
			return
		}
		session, reserveErr := peer.Reserve(ctx, transfer.ReserveRequest{
			ID: id, SourceMachineID: o.identity.Machine.ID, WorkloadID: workload.Spec.ID,
			EstimatedBytes: manifest.Metrics.StoredBytes, ExpiresAt: reservationExpiry(timeout),
		})
		if reserveErr != nil {
			fail("DESTINATION_RESERVATION_FAILED", reserveErr)
			return
		}
		reserved = true
		o.noteReservation(id, "destination session reserved against the finished checkpoint", session.ExpiresAt)
		dataKey, err := o.keys.ExportWorkloadKey(workload.Spec.ID, manifest.Security.KeyVersion)
		if err != nil {
			fail("KEY_EXPORT_FAILED", err)
			return
		}
		if err := peer.ImportKey(ctx, id, workload.Spec.ID, manifest.Security.KeyVersion, dataKey); err != nil {
			fail("KEY_TRANSFER_FAILED", err)
			return
		}
	}
	if err := o.transition(id, model.MigrationTransfer, "negotiating missing chunks", 0.32); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	// The frozen-window transfer re-negotiates instead of dying on one failed
	// chunk: the manifest is content-addressed, so every re-import reports
	// exactly the chunks still missing and nothing already landed is re-sent.
	// A transient interruption costs a re-negotiation round, not the whole
	// migration — and disk-full or memory pressure on the destination shows
	// up here as repeated put failures, ending in one clean rollback instead
	// of a half-guessed retry storm.
	transferStarted := time.Now()
	var transferred int64
	var missing transfer.MissingResponse
	var transferErr error
	manifestFailed := false
	for round := 0; round < transferRounds; round++ {
		if round > 0 {
			o.logger.Warn("re-negotiating missing chunks after transfer interruption",
				"migration_id", id, "round", round+1, "error", transferErr)
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(round) * transferRetryBackoff):
			}
			if ctx.Err() != nil {
				transferErr = ctx.Err()
				break
			}
		}
		missingResponse, manifestErr := peer.ImportManifest(ctx, id, manifest)
		if manifestErr != nil {
			transferErr = manifestErr
			manifestFailed = true
			continue
		}
		manifestFailed = false
		missing = missingResponse
		roundBytes, roundErr := o.uploadChunks(ctx, id, peer, missing.Missing, 0.32, 0.38, "transferring encrypted checkpoint chunks")
		transferred += roundBytes
		// Partial accounting is persisted per round: a migration that dies
		// mid-transfer still reports the bytes it actually moved.
		_ = o.update(id, func(value *model.Migration) error {
			value.Metrics.TransferredBytes = preCopyTransferred + transferred
			value.Metrics.PreCopyTransferredBytes = preCopyTransferred
			return nil
		})
		transferErr = roundErr
		if roundErr == nil {
			break
		}
	}
	transferDuration := time.Since(transferStarted)
	if transferErr != nil {
		if manifestFailed {
			fail("MANIFEST_TRANSFER_FAILED", transferErr)
		} else {
			fail("CHUNK_TRANSFER_FAILED", transferErr)
		}
		return
	}
	if o.diagnostics != nil {
		o.diagnostics.Transferred("upload", transferred, transferDuration)
	}
	if err := o.update(id, func(value *model.Migration) error {
		// TransferredBytes counts everything the migration moved — the
		// pre-copy passes plus the frozen window — while the rate describes
		// only the frozen window, the part that was downtime.
		value.Metrics.TransferredBytes = preCopyTransferred + transferred
		value.Metrics.PreCopyTransferredBytes = preCopyTransferred
		value.Metrics.DeduplicatedBytes = missing.DeduplicatedBytes
		value.Metrics.TransferDuration = transferDuration
		if transferDuration > 0 {
			value.Metrics.TransferBytesPerSec = float64(transferred) / transferDuration.Seconds()
		}
		return nil
	}); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	if err := o.transition(id, model.MigrationVerify, "verifying destination checkpoint integrity", 0.72); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	if _, err := peer.Verify(ctx, id); err != nil {
		fail("DESTINATION_VERIFY_FAILED", err)
		return
	}
	if err := o.transition(id, model.MigrationRestore, "restoring workload on destination", 0.80); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	restoreStarted := time.Now()
	if _, err := peer.Restore(ctx, id, timeout/3); err != nil {
		fail("DESTINATION_RESTORE_FAILED", err)
		return
	}
	restoreDuration := time.Since(restoreStarted)
	// The restore call returns with the process already resumed and health
	// validated on the destination, so the freeze window ends here. The
	// validation round-trip, commit, and source cleanup that follow happen
	// while the workload is running and are not downtime.
	workloadResumedAt = time.Now()
	_ = o.update(id, func(value *model.Migration) error {
		value.Metrics.RestoreDuration = restoreDuration
		value.Metrics.Downtime = workloadResumedAt.Sub(downtimeStart)
		return nil
	})
	if err := o.transition(id, model.MigrationPostValidate, "destination process passed health validation", 0.90); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	session, err := peer.Get(ctx, id)
	if err != nil || session.State != transfer.SessionRestored {
		if err == nil {
			err = fmt.Errorf("destination reported %s instead of RESTORED", session.State)
		}
		fail("DESTINATION_VALIDATION_FAILED", err)
		return
	}
	if err := o.transition(id, model.MigrationSwitch, "destination is authoritative; source remains preserved and stopped", 0.94); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	if err := o.transition(id, model.MigrationCommit, "committing destination restore", 0.97); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	if _, err := peer.Commit(ctx, id); err != nil {
		fail("DESTINATION_COMMIT_FAILED", err)
		return
	}
	committed = true
	if err := o.transition(id, model.MigrationCleanup, "removing the preserved source process", 0.99); err != nil {
		o.logger.Error("persist cleanup stage", "migration_id", id, "error", err)
	}
	// The source root still exists here. If cleanup fails and the source
	// keeps running, the application left behind is told the migration
	// completed elsewhere and it is no longer the live copy.
	o.recordSourceOutcome(id, workload.Spec.ID, networkPlan, network.StatusCompleted, "workload migrated to "+destination.MachineID)
	cleanupErr := o.stopSourceWithRetry(workload.Spec.ID)
	if err := o.update(id, func(value *model.Migration) error {
		if cleanupErr == nil {
			value.SourcePreserved = false
		} else {
			value.SourcePreserved = true
			value.FailureCode = "SOURCE_CLEANUP_FAILED"
			value.FailureReason = cleanupErr.Error()
			appendEvent(value, "destination committed; source cleanup requires operator attention", 1, 0, 0)
		}
		return nil
	}); err != nil {
		// Post-commit bookkeeping must never leave the record non-terminal
		// while the destination already runs the workload: settle it through
		// the same completion path restart recovery uses.
		o.logger.Error("persist migration metrics", "migration_id", id, "error", err)
		if settleErr := o.forceCompleteRecovered(id, "destination committed; final bookkeeping settled best-effort", cleanupErr); settleErr != nil {
			o.logger.Error("force-complete committed migration", "migration_id", id, "error", settleErr)
		}
		return
	}
	message := "migration completed"
	if cleanupErr != nil {
		message = "destination committed; source remains preserved because cleanup failed"
	}
	if err := o.transition(id, model.MigrationCompleted, message, 1); err != nil {
		o.logger.Error("persist migration completion", "migration_id", id, "error", err)
		if settleErr := o.forceCompleteRecovered(id, "destination committed; completion transition settled best-effort", cleanupErr); settleErr != nil {
			o.logger.Error("force-complete committed migration", "migration_id", id, "error", settleErr)
		}
		return
	}
	o.logger.Info("migration completed", "migration_id", id, "workload_id", workload.Spec.ID, "destination", destination.MachineID, "source_preserved", cleanupErr != nil)
	// The same freeze-to-resumed window the record reports, not the whole
	// migration's wall clock.
	o.recordOutcome(id, observability.OutcomeSuccess, workloadResumedAt.Sub(downtimeStart))
}

func (o *Orchestrator) uploadChunks(ctx context.Context, migrationID string, peer PeerClient, refs []model.ChunkRef, progressBase, progressSpan float64, message string) (int64, error) {
	if len(refs) == 0 {
		return 0, nil
	}
	workerCount := 4
	if len(refs) < workerCount {
		workerCount = len(refs)
	}
	type result struct {
		ref model.ChunkRef
		err error
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan model.ChunkRef)
	results := make(chan result, workerCount)
	var workers sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for ref := range jobs {
				var err error
				for attempt := 0; attempt < uploadAttempts; attempt++ {
					err = peer.UploadChunk(ctx, migrationID, ref, o.chunks)
					if err == nil || ctx.Err() != nil {
						break
					}
					select {
					case <-ctx.Done():
					case <-time.After(time.Duration(1<<attempt) * uploadRetryBackoff):
					}
				}
				select {
				case results <- result{ref: ref, err: err}:
				case <-ctx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, ref := range refs {
			select {
			case jobs <- ref:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()
	var transferred int64
	var total int64
	for _, ref := range refs {
		total += ref.StoredSize
	}
	completed := 0
	for result := range results {
		if result.err != nil {
			cancel()
			return transferred, result.err
		}
		transferred += result.ref.StoredSize
		completed++
		if completed == len(refs) || completed%8 == 0 {
			_ = o.progress(migrationID, message, progressBase+progressSpan*float64(transferred)/float64(maxInt64(total, 1)), transferred, total)
		}
	}
	if err := ctx.Err(); err != nil {
		return transferred, err
	}
	if completed != len(refs) {
		return transferred, errors.New("chunk transfer stopped before all chunks completed")
	}
	return transferred, nil
}

func (o *Orchestrator) handleFailure(ctx context.Context, id string, peer PeerClient, reserved, committed, sourceFrozen bool, code string, cause error, downtime time.Duration) {
	if cause == nil {
		cause = errors.New("migration failed")
	}
	o.logger.Error("migration failed", "migration_id", id, "code", code, "error", cause)
	migration, err := o.records.Get(id)
	if err != nil {
		return
	}
	if terminal(migration.Stage) {
		return
	}
	_ = o.update(id, func(value *model.Migration) error {
		// A settled record keeps its root cause: recovery passes and restarts
		// append their own account but never rewrite the failure code and
		// reason the operator needs.
		if value.FailureCode == "" {
			value.FailureCode = code
			value.FailureReason = cause.Error()
		} else {
			value.FailureReason += "; " + code + ": " + cause.Error()
		}
		// The freeze window the attempt actually caused — zero when the
		// workload was never stopped, which a live migration that fails
		// before its freeze must be able to state plainly.
		value.Metrics.Downtime = downtime
		return nil
	})
	if committed {
		// The destination is authoritative; the migration completed its data
		// movement even though a later step failed, so it counts as a success
		// with the source cleanup outcome recorded on the migration.
		o.settleCommitted(id, migration.WorkloadID, "destination committed; settled after interruption", downtime)
		return
	}
	// Roll the migration back rather than parking it in FAILED: observers
	// treat FAILED as terminal, so a migration whose source is still being
	// preserved must never be observable in FAILED. The stage machine lets
	// every pre-commit stage enter ROLLING_BACK directly; the terminal
	// state is decided after the rollback attempt — ROLLED_BACK when the
	// source was preserved and safe, and never FAILED while a rollback is
	// still outstanding: a record left in ROLLING_BACK is re-driven by
	// Recover instead of stranding a frozen source behind a terminal wall.
	// A migration already ROLLING_BACK (crash recovery resuming a rollback)
	// skips the pre-transition entirely: the stage has no self-edge, and
	// taking the →FAILED fallback here would make a clean re-rollback after
	// restart unreportable.
	preFailed := false
	if migration.Stage == model.MigrationRollingBack {
		_ = o.update(id, func(value *model.Migration) error {
			appendEvent(value, "resuming rollback after interruption", migrationProgress(value.Stage), 0, 0)
			return nil
		})
	} else if model.CanTransition(migration.Stage, model.MigrationRollingBack) {
		// A failed ROLLING_BACK persist (disk full, OOM pressure) must not
		// swallow the rollback or park the record where no terminal
		// transition is legal: retry the write, and settle through the
		// direct path below if it never lands.
		var transitionErr error
		for attempt := 0; attempt < statePersistAttempts; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt) * statePersistBackoff)
			}
			if transitionErr = o.transition(id, model.MigrationRollingBack, "rolling back destination and preserving source", migrationProgress(migration.Stage)); transitionErr == nil {
				break
			}
		}
		if transitionErr != nil {
			o.logger.Error("persist rollback stage", "migration_id", id, "error", transitionErr)
		}
	} else if model.CanTransition(migration.Stage, model.MigrationFailed) {
		// This stage cannot roll back at all; nothing was reserved from it,
		// so the failure itself is the terminal truth.
		preFailed = true
		_ = o.transition(id, model.MigrationFailed, cause.Error(), migrationProgress(migration.Stage))
	}
	migration, _ = o.records.Get(id)
	rollbackConfirmed, rollbackErrors := o.rollbackDestination(ctx, id, peer, reserved)
	// The source is thawed only when this migration owns the freeze and the
	// destination cannot be running a copy: a confirmed rollback, or an
	// unconfirmed one where the probe proves the destination holds at most
	// bytes. Resuming on anything less would resurrect a second live copy,
	// and resuming a freeze this migration does not own would SIGCONT a
	// process an operator paused on purpose.
	safeToThaw := rollbackConfirmed
	var loudNotes []string
	if !rollbackConfirmed && sourceFrozen && !o.destinationHoldsLiveCopy(ctx, id, peer) {
		safeToThaw = true
		loudNotes = append(loudNotes, "destination rollback unconfirmed; source resumed because the destination holds no restored workload")
	}
	var resumeErrors []string
	sourceResumed := false
	if sourceFrozen && safeToThaw {
		if workload, getErr := o.runtime.Get(migration.WorkloadID); getErr == nil {
			if workload.Status == model.WorkloadRunning {
				sourceResumed = true
			} else if workload.Process != nil {
				if _, resumeErr := o.runtime.Resume(migration.WorkloadID); resumeErr != nil {
					resumeErrors = append(resumeErrors, "source resume: "+resumeErr.Error())
				} else {
					sourceResumed = true
				}
			}
		}
	}
	// Forwarders republish only toward a workload that is actually running
	// again: a drained-but-still-frozen source would accept connections that
	// hang on a process that cannot serve them.
	if sourceResumed || o.sourceWorkloadRunning(migration.WorkloadID) {
		o.restoreSourceNetwork(ctx, migration)
	} else {
		loudNotes = append(loudNotes, "source forwarding left drained while the workload is not running")
	}
	_ = o.update(id, func(value *model.Migration) error {
		value.SourcePreserved = true
		if len(rollbackErrors) > 0 {
			value.FailureReason += "; rollback incomplete: " + strings.Join(rollbackErrors, "; ")
		}
		if len(resumeErrors) > 0 {
			value.FailureReason += "; " + strings.Join(resumeErrors, "; ")
		}
		for _, note := range loudNotes {
			value.FailureReason += "; " + note
		}
		return nil
	})
	o.recordSourceOutcome(id, migration.WorkloadID, migration.Network, network.StatusFailed, cause.Error())
	migration, _ = o.records.Get(id)
	switch {
	case preFailed:
		o.recordOutcome(id, observability.OutcomeFailure, downtime)
	case (rollbackConfirmed || safeToThaw) && len(resumeErrors) == 0 && len(rollbackErrors) == 0:
		if model.CanTransition(migration.Stage, model.MigrationRolledBack) {
			_ = o.transition(id, model.MigrationRolledBack, "migration rolled back; source workload preserved", 1)
		} else {
			_ = o.forceSettle(id, model.MigrationRolledBack, "migration rolled back; source workload preserved")
		}
		o.recordOutcome(id, observability.OutcomeFailure, downtime)
	case (rollbackConfirmed || safeToThaw) && len(resumeErrors) == 0:
		// Rollback was unconfirmed but nothing live is left on the
		// destination and the source is back: settle, loudly, with the
		// unconfirmed cleanup named in the failure reason.
		_ = o.forceSettle(id, model.MigrationRolledBack, "source preserved; destination rollback was not confirmed")
		o.recordOutcome(id, observability.OutcomeFailure, downtime)
	default:
		// Rollback or resume is still outstanding. The record stays in
		// ROLLING_BACK — deliberately non-terminal — so Recover re-attempts
		// it on every restart and the frozen source can never be lost behind
		// a terminal FAILED no one reopens.
		_ = o.update(id, func(value *model.Migration) error {
			appendEvent(value, "rollback or source resume incomplete; record kept non-terminal for recovery to re-attempt", migrationProgress(value.Stage), 0, 0)
			return nil
		})
		o.logger.Error("migration rollback incomplete; kept for recovery",
			"migration_id", id, "rollback_errors", rollbackErrors, "resume_errors", resumeErrors)
	}
}

// rollbackDestination tears down whatever the destination holds for this
// migration, retrying with backoff: a one-second network blip must not
// escalate into permanent operator intervention. Confirmed is returned only
// when the destination provably holds nothing to undo — a missing session is
// as confirmed as a completed rollback, while a refused or unreachable one
// never is.
func (o *Orchestrator) rollbackDestination(ctx context.Context, id string, peer PeerClient, reserved bool) (bool, []string) {
	if !reserved {
		// Nothing was ever reserved on the destination: it is provably
		// stateless for this migration, which is as confirmed as a rollback.
		return true, nil
	}
	if peer == nil {
		// The destination client could not even be constructed; state may
		// still be over there and must be assumed present.
		return false, []string{"destination rollback could not be attempted: no destination client"}
	}
	var lastErr error
	for attempt := 0; attempt < rollbackAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(1<<uint(attempt-1)) * rollbackRetryBackoff):
			}
			if ctx.Err() != nil {
				break
			}
		}
		rollbackCtx, cancel := context.WithTimeout(ctx, rollbackAttemptTimeout)
		_, rollbackErr := peer.Rollback(rollbackCtx, id)
		cancel()
		if rollbackErr == nil {
			return true, nil
		}
		if errors.Is(rollbackErr, transfer.ErrTransferNotFound) {
			// The destination provably holds nothing for this migration.
			return true, nil
		}
		lastErr = rollbackErr
	}
	if lastErr == nil {
		lastErr = errors.New("rollback aborted by cancellation")
	}
	return false, []string{fmt.Sprintf("destination rollback failed after %d attempts: %s", rollbackAttempts, lastErr.Error())}
}

// destinationHoldsLiveCopy probes the destination after an unconfirmed
// rollback. Only a restored or committed session runs a copy of the workload,
// and only a session at or past verify can start one without this source
// driving it: anything below that holds at most bytes. Unknown answers —
// including a client that cannot probe — count as "may hold a copy", because
// the source must never be thawed on a guess.
func (o *Orchestrator) destinationHoldsLiveCopy(ctx context.Context, id string, peer PeerClient) bool {
	if peer == nil {
		return true
	}
	probeCtx, cancel := context.WithTimeout(ctx, rollbackProbeTimeout)
	session, err := peer.GetForCleanup(probeCtx, id)
	cancel()
	if err != nil {
		return !errors.Is(err, transfer.ErrTransferNotFound)
	}
	switch session.State {
	case transfer.SessionReserved, transfer.SessionKeyReady, transfer.SessionManifestReady,
		transfer.SessionRolledBack, transfer.SessionFailed:
		return false
	default:
		return true
	}
}

// sourceWorkloadRunning reports whether the preserved source is actually
// serving again.
func (o *Orchestrator) sourceWorkloadRunning(workloadID string) bool {
	workload, err := o.runtime.Get(workloadID)
	return err == nil && workload.Status == model.WorkloadRunning
}

// settleCommitted records the post-commit outcome and settles the record
// COMPLETED through the recovery completion path: the destination is
// authoritative, and the only outstanding question is whether the preserved
// source was stopped. A failed stop is recorded the way run() records it —
// SourcePreserved stays true and SOURCE_CLEANUP_FAILED names the problem —
// instead of claiming a cleanup that never happened.
func (o *Orchestrator) settleCommitted(id, workloadID, message string, downtime time.Duration) {
	cleanupErr := o.stopSourceWithRetry(workloadID)
	if cleanupErr != nil {
		o.logger.Error("source cleanup after destination commit", "migration_id", id, "error", cleanupErr)
	}
	if err := o.forceCompleteRecovered(id, message, cleanupErr); err != nil {
		o.logger.Error("force-complete committed migration", "migration_id", id, "error", err)
	}
	o.recordOutcome(id, observability.OutcomeSuccess, downtime)
}

func (o *Orchestrator) handleCancellation(ctx context.Context, id string, peer PeerClient, reserved, committed, sourceFrozen bool, cause error, downtime time.Duration) {
	if cause == nil {
		cause = context.Canceled
	}
	o.logger.Info("migration cancellation requested", "migration_id", id)
	migration, err := o.records.Get(id)
	if err != nil || terminal(migration.Stage) {
		return
	}
	if committed {
		// A committed destination is authoritative; cancellation cannot
		// safely undo it and the migration is a success. The cancellation
		// code must not ride on a COMPLETED record — only a failed source
		// cleanup may — so it is never written on this path.
		_ = o.update(id, func(value *model.Migration) error {
			value.SourcePreserved = true
			value.Metrics.Downtime = downtime
			return nil
		})
		o.settleCommitted(id, migration.WorkloadID, "destination committed before cancellation was processed", downtime)
		return
	}
	_ = o.update(id, func(value *model.Migration) error {
		// The spelling matches the persisted failure code; both Ls are part
		// of the API surface recorded in existing migration rows.
		value.FailureCode = "MIGRATION_CANCELLED" //nolint:misspell // persisted failure code in existing migration rows
		value.FailureReason = cause.Error()
		value.SourcePreserved = true
		value.Metrics.Downtime = downtime
		return nil
	})
	// Cancellation rolls the destination back through the same retrying,
	// escalation-aware path a failure does: a transient rollback blip must
	// never leave a frozen source behind a settled-looking record.
	if migration.Stage == model.MigrationRollingBack {
		_ = o.update(id, func(value *model.Migration) error {
			appendEvent(value, "resuming cancellation rollback after interruption", migrationProgress(value.Stage), 0, 0)
			return nil
		})
	} else if model.CanTransition(migration.Stage, model.MigrationRollingBack) {
		var transitionErr error
		for attempt := 0; attempt < statePersistAttempts; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt) * statePersistBackoff)
			}
			if transitionErr = o.transition(id, model.MigrationRollingBack, "cancellation requested; preserving source workload", migrationProgress(migration.Stage)); transitionErr == nil {
				break
			}
		}
		if transitionErr != nil {
			o.logger.Error("persist cancellation rollback stage", "migration_id", id, "error", transitionErr)
		}
	}
	migration, _ = o.records.Get(id)
	rollbackConfirmed, rollbackErrors := o.rollbackDestination(ctx, id, peer, reserved)
	safeToThaw := rollbackConfirmed
	var loudNotes []string
	if !rollbackConfirmed && sourceFrozen && !o.destinationHoldsLiveCopy(ctx, id, peer) {
		safeToThaw = true
		loudNotes = append(loudNotes, "destination rollback unconfirmed; source resumed because the destination holds no restored workload")
	}
	var resumeErrors []string
	sourceResumed := false
	if sourceFrozen && safeToThaw {
		if workload, getErr := o.runtime.Get(migration.WorkloadID); getErr == nil {
			if workload.Status == model.WorkloadRunning {
				sourceResumed = true
			} else if workload.Process != nil {
				if _, resumeErr := o.runtime.Resume(migration.WorkloadID); resumeErr != nil {
					resumeErrors = append(resumeErrors, "source resume: "+resumeErr.Error())
				} else {
					sourceResumed = true
				}
			}
		}
	}
	if sourceResumed || o.sourceWorkloadRunning(migration.WorkloadID) {
		o.restoreSourceNetwork(ctx, migration)
	} else {
		loudNotes = append(loudNotes, "source forwarding left drained while the workload is not running")
	}
	_ = o.update(id, func(value *model.Migration) error {
		value.SourcePreserved = true
		if len(rollbackErrors) > 0 {
			value.FailureReason += "; rollback incomplete: " + strings.Join(rollbackErrors, "; ")
		}
		if len(resumeErrors) > 0 {
			value.FailureReason += "; " + strings.Join(resumeErrors, "; ")
		}
		for _, note := range loudNotes {
			value.FailureReason += "; " + note
		}
		return nil
	})
	// The two-L spelling matches the failure code this rollback records.
	o.recordSourceOutcome(id, migration.WorkloadID, migration.Network, network.StatusFailed, "migration cancelled: "+cause.Error()) //nolint:misspell // matches the persisted failure-code vocabulary
	migration, _ = o.records.Get(id)
	switch {
	case (rollbackConfirmed || safeToThaw) && len(resumeErrors) == 0 && len(rollbackErrors) == 0:
		if model.CanTransition(migration.Stage, model.MigrationCancelled) {
			_ = o.transition(id, model.MigrationCancelled, "migration cancelled; source workload preserved", 1) //nolint:misspell // stage vocabulary
		} else {
			_ = o.forceSettle(id, model.MigrationCancelled, "migration cancelled; source workload preserved")
		}
		o.recordOutcome(id, observability.OutcomeCancelled, downtime)
	case (rollbackConfirmed || safeToThaw) && len(resumeErrors) == 0:
		// Rollback was unconfirmed but nothing live is left on the
		// destination and the source is back: settle, loudly.
		_ = o.forceSettle(id, model.MigrationCancelled, "source preserved; destination rollback was not confirmed")
		o.recordOutcome(id, observability.OutcomeCancelled, downtime)
	default:
		// Rollback or resume is still outstanding: keep the record
		// non-terminal so recovery re-attempts it instead of hiding a frozen
		// source behind a settled status.
		_ = o.update(id, func(value *model.Migration) error {
			appendEvent(value, "cancellation rollback or source resume incomplete; record kept non-terminal for recovery to re-attempt", migrationProgress(value.Stage), 0, 0)
			return nil
		})
		o.logger.Error("cancellation rollback incomplete; kept for recovery",
			"migration_id", id, "rollback_errors", rollbackErrors, "resume_errors", resumeErrors)
	}
}

// recordSourceOutcome writes the network status document into the source
// workload's root when a migration attempt ends with the source preserved.
// The application that stayed behind is told exactly what happened: the
// migration did not complete, its connections were dropped when the checkpoint
// froze it, and the sockets were not carried. Failing to record the status is
// logged and never fatal — the migration outcome itself is already persisted.
func (o *Orchestrator) recordSourceOutcome(id, workloadID string, plan model.NetworkPlan, outcome, detail string) {
	workload, err := o.runtime.Get(workloadID)
	if err != nil {
		return
	}
	if plan.Identity.IP == "" {
		// The migration failed before a network plan was computed.
		computed, planErr := network.PlanFor(workload.Spec)
		if planErr != nil {
			return
		}
		plan = computed
	}
	socketsPreserved := outcome == network.StatusCompleted && plan.SocketsCarried
	document := network.StatusDocument{
		OperationID: id, Operation: network.OperationMigration, Outcome: outcome,
		Policy: string(plan.Policy), VirtualIP: plan.Identity.IP,
		SocketsPreserved: socketsPreserved, ConnectionsDropped: !socketsPreserved,
		Ports: plan.Ports,
	}
	if detail != "" {
		document.Detail = detail
	}
	if _, err := network.WriteStatus(workload.Spec.RootPath, document); err != nil {
		o.logger.Warn("could not record source migration status",
			"migration_id", id, "error", err)
	}
}

func (o *Orchestrator) recoverOne(parent context.Context, id string) {
	// Recovery runs through the same admission budget as live migrations and
	// registers its cancel handle, so N crashed migrations roll back at most
	// maxConcurrent at a time like any other work, and Cancel answers for a
	// migration that is actively recovering.
	o.semaphore <- struct{}{}
	defer func() {
		<-o.semaphore
		o.mu.Lock()
		if cancel := o.cancel[id]; cancel != nil {
			cancel()
		}
		delete(o.cancel, id)
		o.mu.Unlock()
	}()
	ctx, cancel := context.WithCancel(parent)
	o.mu.Lock()
	o.cancel[id] = cancel
	o.mu.Unlock()
	defer cancel()

	migration, err := o.records.Get(id)
	if err != nil || terminal(migration.Stage) {
		return
	}
	// The persisted freeze facts are the ownership truth: whether this
	// migration stopped its workload (and may therefore thaw it), and since
	// when the workload has been down — recovery reports that frozen outage
	// instead of a zeroed downtime.
	fact, hasFact := o.freezeState(id)
	var factPtr *migrationFreeze
	if hasFact {
		factPtr = &fact
	}
	sourceFrozen := recoveryOwnsFreeze(migration, factPtr)
	downtime := time.Duration(0)
	if hasFact && !fact.DowntimeStart.IsZero() {
		downtime = time.Since(fact.DowntimeStart)
	}
	peer, clientErr := o.clients(migration.Destination)
	// Whether a destination reservation actually exists is a fact about the
	// destination, not about whether a client could be constructed: recovery
	// must only roll back state that is really there, or a crash before the
	// reservation becomes a false "operator intervention" alarm. The probe is
	// cleanup-scoped, so an expired reservation cannot masquerade as "no
	// state" while a restored workload is still live over there. A client
	// that cannot even be constructed never ran that probe — exactly like a
	// failed one, state may exist and must be rolled back, never waved
	// through with a source resume.
	reserved := false
	probeFailed := false
	if clientErr == nil {
		queryCtx, queryCancel := context.WithTimeout(ctx, rollbackProbeTimeout)
		session, queryErr := peer.GetForCleanup(queryCtx, id)
		queryCancel()
		switch {
		case queryErr == nil:
			if session.State == transfer.SessionCommitted {
				// Mirror run(): a source that could not be stopped stays
				// preserved and is named SOURCE_CLEANUP_FAILED instead of
				// being claimed gone.
				cleanupErr := o.stopSourceWithRetry(migration.WorkloadID)
				_ = o.forceCompleteRecovered(id, "destination was already committed before agent restart", cleanupErr)
				o.recordOutcome(id, observability.OutcomeSuccess, downtime)
				return
			}
			reserved = session.State != transfer.SessionRolledBack && session.State != transfer.SessionFailed
		case errors.Is(queryErr, transfer.ErrTransferNotFound):
			// The destination provably holds nothing for this migration.
		default:
			// The probe could not run: assume state may exist so the rollback
			// is attempted instead of declaring a false all-clear.
			probeFailed = true
			reserved = true
		}
	} else {
		probeFailed = true
		reserved = true
	}
	if !reserved {
		_ = o.update(id, func(value *model.Migration) error {
			appendEvent(value, "agent restarted before destination reservation; no destination state to roll back", migrationProgress(value.Stage), 0, 0)
			return nil
		})
	} else if probeFailed {
		_ = o.update(id, func(value *model.Migration) error {
			appendEvent(value, "agent restarted; destination state could not be probed and will be rolled back", migrationProgress(value.Stage), 0, 0)
			return nil
		})
	}
	o.handleFailure(ctx, id, peer, reserved, false, sourceFrozen, "AGENT_RESTARTED", errors.New("agent restarted during migration"), downtime)
}

// recordOutcome turns a finished migration into diagnostics series. Duration is
// measured from the persisted CreatedAt — a fact, not a goroutine's local
// clock — and only terminal migrations are recorded, so restart-recovered ones
// are not lost: Recover routes them through handleFailure or completion too.
func (o *Orchestrator) recordOutcome(id, outcome string, downtime time.Duration) {
	if o.diagnostics == nil {
		return
	}
	migration, err := o.records.Get(id)
	if err != nil {
		return
	}
	duration := time.Since(migration.CreatedAt)
	if migration.CompletedAt != nil {
		duration = migration.CompletedAt.Sub(migration.CreatedAt)
	}
	o.diagnostics.MigrationFinished(outcome, duration, downtime)
}

// forceCompleteRecovered settles a migration whose destination already
// committed as COMPLETED through the direct write path, recording the source
// cleanup outcome the way run() records it: SourcePreserved is false only
// after a confirmed stop, and a failed stop keeps the source preserved and
// names SOURCE_CLEANUP_FAILED instead of claiming a cleanup that never
// happened.
func (o *Orchestrator) forceCompleteRecovered(id, message string, sourceStopErr error) error {
	return o.update(id, func(value *model.Migration) error {
		value.Stage = model.MigrationCompleted
		value.SourcePreserved = sourceStopErr != nil
		if sourceStopErr != nil {
			value.FailureCode = "SOURCE_CLEANUP_FAILED"
			value.FailureReason = sourceStopErr.Error()
		}
		value.UpdatedAt = time.Now().UTC()
		completed := value.UpdatedAt
		value.CompletedAt = &completed
		appendEvent(value, message, 1, 0, 0)
		return nil
	})
}

// forceSettle writes a terminal stage directly when the stage machine's own
// transition could not be persisted (disk full, OOM pressure): a transient
// metadata write failure must never leave the record non-terminal and block
// every later migration of the workload.
func (o *Orchestrator) forceSettle(id string, stage model.MigrationStage, message string) error {
	return o.update(id, func(value *model.Migration) error {
		value.Stage = stage
		value.UpdatedAt = time.Now().UTC()
		completed := value.UpdatedAt
		value.CompletedAt = &completed
		appendEvent(value, message, 1, 0, 0)
		return nil
	})
}

func (o *Orchestrator) stopSourceWithRetry(workloadID string) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		_, lastErr = o.runtime.Stop(workloadID, 10*time.Second)
		if lastErr == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
	}
	return lastErr
}

func (o *Orchestrator) transition(id string, stage model.MigrationStage, message string, progress float64) error {
	return o.update(id, func(value *model.Migration) error {
		return value.Transition(stage, message, progress)
	})
}

func (o *Orchestrator) progress(id, message string, progress float64, done, total int64) error {
	return o.update(id, func(value *model.Migration) error {
		appendEvent(value, message, progress, done, total)
		return nil
	})
}

func (o *Orchestrator) update(id string, update func(*model.Migration) error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := o.records.Update(id, func(value model.Migration) (model.Migration, error) {
		if err := update(&value); err != nil {
			return value, err
		}
		value.UpdatedAt = time.Now().UTC()
		value.Revision++
		return value, nil
	})
	return err
}

func appendEvent(value *model.Migration, message string, progress float64, done, total int64) {
	value.Events = append(value.Events, model.MigrationEvent{
		Sequence: uint64(len(value.Events) + 1), Stage: value.Stage, Message: message,
		Timestamp: time.Now().UTC(), Progress: progress, BytesDone: done, BytesTotal: total,
	})
	if len(value.Events) > 256 {
		value.Events = append([]model.MigrationEvent(nil), value.Events[len(value.Events)-256:]...)
	}
}

func compatibilityFailure(report model.CompatibilityReport) error {
	var reasons []string
	for _, issue := range report.Issues {
		if issue.Severity == "error" {
			reasons = append(reasons, issue.Code+": "+issue.Description)
		}
	}
	return errors.New(strings.Join(reasons, "; "))
}

func terminal(stage model.MigrationStage) bool {
	return stage == model.MigrationCompleted || stage == model.MigrationRolledBack || stage == model.MigrationCancelled || stage == model.MigrationFailed
}

// destinationMatches compares a CreateRequest's destination against the
// stored one the way the idempotency contract promises: a replay is the same
// request when only the resolved machine id differs. run() fills the record's
// Destination.MachineID with the live peer identity after admission, so an
// exact struct compare would reject every retry of a request that left
// machine_id empty — the documented "retry after a lost response" path.
// A request that pinned a machine id still conflicts when the recorded
// destination resolved somewhere else.
func destinationMatches(request, stored model.Destination) bool {
	if request.AgentURL != stored.AgentURL || request.ServerName != stored.ServerName ||
		request.Route != stored.Route || request.RelayEndpoint != stored.RelayEndpoint ||
		request.RelayTicket != stored.RelayTicket {
		return false
	}
	if request.MachineID != "" && stored.MachineID != "" && request.MachineID != stored.MachineID {
		return false
	}
	return true
}

// reservationExpiry derives the transfer reservation expiry from the run
// budget, clamped to the window the destination will actually honor instead
// of requesting more and discovering the cap mid-transfer.
func reservationExpiry(timeout time.Duration) time.Time {
	if timeout <= 0 || timeout > maxReservationWindow {
		timeout = maxReservationWindow
	}
	return time.Now().Add(timeout)
}

// noteReservation surfaces the destination's effective reservation expiry on
// the migration record: the destination caps reservations, and a run — or an
// operator reading it — must see the expiry it actually got.
func (o *Orchestrator) noteReservation(id, message string, expiresAt time.Time) {
	if !expiresAt.IsZero() {
		message += "; transfer reservation expires " + expiresAt.UTC().Format(time.RFC3339)
	}
	_ = o.update(id, func(value *model.Migration) error {
		appendEvent(value, message, 0.28, 0, 0)
		return nil
	})
}

func migrationProgress(stage model.MigrationStage) float64 {
	values := map[model.MigrationStage]float64{
		model.MigrationCreated: 0, model.MigrationDiscover: 0.03, model.MigrationValidate: 0.08,
		model.MigrationSnapshot: 0.12, model.MigrationPrepare: 0.28, model.MigrationTransfer: 0.32,
		model.MigrationVerify: 0.72, model.MigrationRestore: 0.80, model.MigrationPostValidate: 0.90,
		model.MigrationSwitch: 0.94, model.MigrationCommit: 0.97, model.MigrationCleanup: 0.99,
	}
	return values[stage]
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
