package migration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/compatibility"
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
}

type ClientFactory func(model.Destination) (PeerClient, error)

type CreateRequest struct {
	WorkloadID  string              `json:"workload_id"`
	Destination model.Destination   `json:"destination"`
	Mode        model.MigrationMode `json:"mode"`
	Timeout     time.Duration       `json:"timeout"`
}

type Orchestrator struct {
	mu             sync.Mutex
	records        *securestore.EncryptedCollection[model.Migration]
	runtime        *shiftruntime.Manager
	checkpoints    *checkpoint.Service
	checkpointRepo *checkpoint.Repository
	chunks         *chunkstore.Store
	keys           *securestore.Manager
	identity       *identity.Identity
	inventory      *linuxplatform.Inventory
	clients        ClientFactory
	sourceNetwork  SourceNetwork
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
	return &Orchestrator{
		records: records, runtime: runtimeManager, checkpoints: checkpointService,
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

func (o *Orchestrator) Create(parent context.Context, request CreateRequest) (model.Migration, error) {
	if request.WorkloadID == "" {
		return model.Migration{}, errors.New("workload id is required")
	}
	parsed, err := url.Parse(request.Destination.AgentURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return model.Migration{}, errors.New("destination agent URL must use https")
	}
	if request.Mode == "" {
		request.Mode = model.MigrationCold
	}
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
	id, err := model.NewID()
	if err != nil {
		return model.Migration{}, err
	}
	now := time.Now().UTC()
	migration := model.Migration{
		ID: id, WorkloadID: workload.Spec.ID, SourceMachineID: o.identity.Machine.ID,
		Destination: request.Destination, Mode: request.Mode, Stage: model.MigrationCreated,
		CreatedAt: now, UpdatedAt: now, SourcePreserved: true, Revision: 1,
		Events: []model.MigrationEvent{{Sequence: 1, Stage: model.MigrationCreated, Message: "migration created", Timestamp: now, Progress: 0}},
	}
	if err := o.records.Put(id, migration); err != nil {
		return model.Migration{}, err
	}
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
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	// One span covers the whole migration; every peer call made with ctx
	// carries its trace id, so source and destination land in one trace view.
	_, span := o.tracer.Start(ctx, "migration", map[string]string{"migration_id": id})
	defer span.End()
	var peer PeerClient
	reserved := false
	committed := false
	downtimeStart := time.Time{}
	fail := func(code string, cause error) {
		// Downtime is the freeze window: zero when the failure happened
		// before the workload was ever stopped.
		downtime := time.Duration(0)
		if !downtimeStart.IsZero() {
			downtime = time.Since(downtimeStart)
		}
		if errors.Is(cause, context.Canceled) {
			o.handleCancellation(context.Background(), id, peer, reserved, committed, cause, downtime)
			return
		}
		o.handleFailure(context.Background(), id, peer, reserved, committed, code, cause, downtime)
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
		fail("DESTINATION_IS_SOURCE", errors.New("source and destination resolve to the same machine identity"))
		return
	}
	if migration.Destination.MachineID != "" && migration.Destination.MachineID != destination.MachineID {
		fail("DESTINATION_IDENTITY_MISMATCH", errors.New("destination certificate does not match the requested machine"))
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
		snapshotMessage = "running CRIU pre-copy and creating the final checkpoint"
	}
	// A drain-policy workload stops being reachable before it is frozen:
	// its published listeners stop accepting and in-flight connections get
	// the grace period to finish, so the checkpoint captures a process
	// whose connections ended cleanly rather than mid-stream.
	o.drainSourceForwarders(id, workload.Spec.ID, networkPlan)
	if err := o.transition(id, model.MigrationSnapshot, snapshotMessage, 0.12); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	downtimeStart = time.Now()
	preCopy := migration.Mode == model.MigrationLive
	manifest, err := o.checkpoints.Create(ctx, workload.Spec.ID, checkpoint.CreateOptions{
		Kind: model.CheckpointFull, LeaveRunning: false,
		TCPState: workload.Spec.NetworkPolicy == model.NetworkPreserve,
		PreCopy:  preCopy,
		Timeout:  timeout / 2,
	})
	if err != nil {
		fail("CHECKPOINT_FAILED", err)
		return
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
	if err := o.transition(id, model.MigrationPrepare, "reserving destination and establishing workload key", 0.28); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	if _, err := peer.Reserve(ctx, transfer.ReserveRequest{
		ID: id, SourceMachineID: o.identity.Machine.ID, WorkloadID: workload.Spec.ID,
		EstimatedBytes: manifest.Metrics.StoredBytes, ExpiresAt: time.Now().Add(timeout),
	}); err != nil {
		fail("DESTINATION_RESERVATION_FAILED", err)
		return
	}
	reserved = true
	dataKey, err := o.keys.ExportWorkloadKey(workload.Spec.ID, manifest.Security.KeyVersion)
	if err != nil {
		fail("KEY_EXPORT_FAILED", err)
		return
	}
	if err := peer.ImportKey(ctx, id, workload.Spec.ID, manifest.Security.KeyVersion, dataKey); err != nil {
		fail("KEY_TRANSFER_FAILED", err)
		return
	}
	if err := o.transition(id, model.MigrationTransfer, "negotiating missing chunks", 0.32); err != nil {
		fail("STATE_PERSIST_FAILED", err)
		return
	}
	missing, err := peer.ImportManifest(ctx, id, manifest)
	if err != nil {
		fail("MANIFEST_TRANSFER_FAILED", err)
		return
	}
	transferStarted := time.Now()
	transferred, err := o.uploadChunks(ctx, id, peer, missing.Missing)
	transferDuration := time.Since(transferStarted)
	if err != nil {
		fail("CHUNK_TRANSFER_FAILED", err)
		return
	}
	if o.diagnostics != nil {
		o.diagnostics.Transferred("upload", transferred, transferDuration)
	}
	if err := o.update(id, func(value *model.Migration) error {
		value.Metrics.TransferredBytes = transferred
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
	_ = o.update(id, func(value *model.Migration) error {
		value.Metrics.RestoreDuration = restoreDuration
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
		value.Metrics.Downtime = time.Since(downtimeStart)
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
		o.logger.Error("persist migration metrics", "migration_id", id, "error", err)
		return
	}
	message := "migration completed"
	if cleanupErr != nil {
		message = "destination committed; source remains preserved because cleanup failed"
	}
	if err := o.transition(id, model.MigrationCompleted, message, 1); err != nil {
		o.logger.Error("persist migration completion", "migration_id", id, "error", err)
		return
	}
	o.logger.Info("migration completed", "migration_id", id, "workload_id", workload.Spec.ID, "destination", destination.MachineID, "source_preserved", cleanupErr != nil)
	o.recordOutcome(id, observability.OutcomeSuccess, time.Since(downtimeStart))
}

func (o *Orchestrator) uploadChunks(ctx context.Context, migrationID string, peer PeerClient, refs []model.ChunkRef) (int64, error) {
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
				for attempt := 0; attempt < 3; attempt++ {
					err = peer.UploadChunk(ctx, migrationID, ref, o.chunks)
					if err == nil || ctx.Err() != nil {
						break
					}
					select {
					case <-ctx.Done():
					case <-time.After(time.Duration(1<<attempt) * 250 * time.Millisecond):
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
			_ = o.progress(migrationID, "transferring encrypted checkpoint chunks", 0.32+0.38*float64(transferred)/float64(maxInt64(total, 1)), transferred, total)
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

func (o *Orchestrator) handleFailure(ctx context.Context, id string, peer PeerClient, reserved, committed bool, code string, cause error, downtime time.Duration) {
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
		value.FailureCode = code
		value.FailureReason = cause.Error()
		return nil
	})
	if committed {
		// The destination is authoritative; the migration completed its data
		// movement even though a later step failed, so it counts as a success
		// with the source cleanup problem recorded on the migration.
		_ = o.stopSourceWithRetry(migration.WorkloadID)
		o.recordOutcome(id, observability.OutcomeSuccess, downtime)
		return
	}
	if model.CanTransition(migration.Stage, model.MigrationFailed) {
		_ = o.transition(id, model.MigrationFailed, cause.Error(), migrationProgress(migration.Stage))
	}
	migration, _ = o.records.Get(id)
	if model.CanTransition(migration.Stage, model.MigrationRollingBack) {
		_ = o.transition(id, model.MigrationRollingBack, "rolling back destination and preserving source", migrationProgress(migration.Stage))
	}
	var rollbackErrors []string
	if reserved && peer != nil {
		rollbackCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_, rollbackErr := peer.Rollback(rollbackCtx, id)
		cancel()
		if rollbackErr != nil {
			rollbackErrors = append(rollbackErrors, "destination rollback: "+rollbackErr.Error())
		}
	}
	if workload, getErr := o.runtime.Get(migration.WorkloadID); getErr == nil && workload.Process != nil && workload.Status != model.WorkloadRunning {
		if _, resumeErr := o.runtime.Resume(migration.WorkloadID); resumeErr != nil {
			rollbackErrors = append(rollbackErrors, "source resume: "+resumeErr.Error())
		}
	}
	o.restoreSourceNetwork(ctx, migration)
	_ = o.update(id, func(value *model.Migration) error {
		value.SourcePreserved = true
		if len(rollbackErrors) > 0 {
			value.FailureReason += "; rollback incomplete: " + strings.Join(rollbackErrors, "; ")
		}
		return nil
	})
	o.recordSourceOutcome(id, migration.WorkloadID, migration.Network, network.StatusFailed, cause.Error())
	migration, _ = o.records.Get(id)
	if len(rollbackErrors) == 0 && model.CanTransition(migration.Stage, model.MigrationRolledBack) {
		_ = o.transition(id, model.MigrationRolledBack, "migration rolled back; source workload preserved", 1)
		o.recordOutcome(id, observability.OutcomeFailure, downtime)
	} else if len(rollbackErrors) > 0 && model.CanTransition(migration.Stage, model.MigrationFailed) {
		_ = o.transition(id, model.MigrationFailed, "rollback requires operator intervention", migrationProgress(migration.Stage))
		o.recordOutcome(id, observability.OutcomeFailure, downtime)
	}
}

func (o *Orchestrator) handleCancellation(ctx context.Context, id string, peer PeerClient, reserved, committed bool, cause error, downtime time.Duration) {
	if cause == nil {
		cause = context.Canceled
	}
	o.logger.Info("migration cancellation requested", "migration_id", id)
	migration, err := o.records.Get(id)
	if err != nil || terminal(migration.Stage) {
		return
	}
	_ = o.update(id, func(value *model.Migration) error {
		value.FailureCode = "MIGRATION_CANCELLED"
		value.FailureReason = cause.Error()
		value.SourcePreserved = true
		return nil
	})
	if committed {
		// A committed destination is authoritative; cancellation cannot safely
		// undo it. Keep the source-preservation invariant for cleanup/recovery.
		return
	}
	var rollbackErrors []string
	if reserved && peer != nil {
		rollbackCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_, rollbackErr := peer.Rollback(rollbackCtx, id)
		cancel()
		if rollbackErr != nil {
			rollbackErrors = append(rollbackErrors, "destination rollback: "+rollbackErr.Error())
		}
	}
	if workload, getErr := o.runtime.Get(migration.WorkloadID); getErr == nil && workload.Process != nil && workload.Status != model.WorkloadRunning {
		if _, resumeErr := o.runtime.Resume(migration.WorkloadID); resumeErr != nil {
			rollbackErrors = append(rollbackErrors, "source resume: "+resumeErr.Error())
		}
	}
	o.restoreSourceNetwork(ctx, migration)
	migration, _ = o.records.Get(id)
	if model.CanTransition(migration.Stage, model.MigrationRollingBack) {
		_ = o.transition(id, model.MigrationRollingBack, "cancellation requested; preserving source workload", migrationProgress(migration.Stage))
		migration, _ = o.records.Get(id)
	}
	_ = o.update(id, func(value *model.Migration) error {
		value.SourcePreserved = true
		if len(rollbackErrors) > 0 {
			value.FailureReason += "; rollback incomplete: " + strings.Join(rollbackErrors, "; ")
		}
		return nil
	})
	o.recordSourceOutcome(id, migration.WorkloadID, migration.Network, network.StatusFailed, "migration cancelled: "+cause.Error())
	migration, _ = o.records.Get(id)
	if len(rollbackErrors) == 0 && model.CanTransition(migration.Stage, model.MigrationCancelled) {
		_ = o.transition(id, model.MigrationCancelled, "migration cancelled; source workload preserved", 1)
		o.recordOutcome(id, observability.OutcomeCancelled, downtime)
	} else if len(rollbackErrors) > 0 && model.CanTransition(migration.Stage, model.MigrationFailed) {
		_ = o.transition(id, model.MigrationFailed, "cancellation rollback requires operator intervention", migrationProgress(migration.Stage))
		o.recordOutcome(id, observability.OutcomeCancelled, downtime)
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

func (o *Orchestrator) recoverOne(ctx context.Context, id string) {
	migration, err := o.records.Get(id)
	if err != nil || terminal(migration.Stage) {
		return
	}
	peer, clientErr := o.clients(migration.Destination)
	if clientErr == nil {
		queryCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		session, queryErr := peer.Get(queryCtx, id)
		cancel()
		if queryErr == nil && session.State == transfer.SessionCommitted {
			_ = o.stopSourceWithRetry(migration.WorkloadID)
			_ = o.forceCompleteRecovered(id, "destination was already committed before agent restart")
			o.recordOutcome(id, observability.OutcomeSuccess, 0)
			return
		}
	}
	o.handleFailure(ctx, id, peer, clientErr == nil, false, "AGENT_RESTARTED", errors.New("agent restarted during migration"), 0)
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

func (o *Orchestrator) forceCompleteRecovered(id, message string) error {
	return o.update(id, func(value *model.Migration) error {
		value.Stage = model.MigrationCompleted
		value.SourcePreserved = false
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
	return stage == model.MigrationCompleted || stage == model.MigrationRolledBack || stage == model.MigrationCancelled
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
