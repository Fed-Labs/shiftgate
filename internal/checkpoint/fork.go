package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/network"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	"shift.dev/shift/internal/securestore"
)

type ForkState string

const (
	ForkPreparing    ForkState = "PREPARING"
	ForkMaterialized ForkState = "MATERIALIZED"
	ForkActivating   ForkState = "ACTIVATING"
	ForkRunning      ForkState = "RUNNING"
	ForkCommitted    ForkState = "COMMITTED"
	ForkRollingBack  ForkState = "ROLLING_BACK"
	ForkRolledBack   ForkState = "ROLLED_BACK"
	ForkFailed       ForkState = "FAILED"
)

var forkTransitions = map[ForkState]map[ForkState]bool{
	ForkPreparing:    {ForkMaterialized: true, ForkRollingBack: true, ForkFailed: true},
	ForkMaterialized: {ForkActivating: true, ForkCommitted: true, ForkRollingBack: true, ForkFailed: true},
	ForkActivating:   {ForkRunning: true, ForkRollingBack: true, ForkFailed: true},
	ForkRunning:      {ForkCommitted: true, ForkRollingBack: true, ForkFailed: true},
	ForkRollingBack:  {ForkRolledBack: true, ForkFailed: true},
	ForkFailed:       {ForkRollingBack: true},
}

func CanTransitionFork(from, to ForkState) bool {
	return forkTransitions[from][to]
}

// ForkRecord is the persisted transaction for one fork. It is written before
// any filesystem mutation and updated at every state change so that an agent
// restart can finish or reverse a partially materialized fork.
type ForkRecord struct {
	ID                 string    `json:"id"`
	SourceWorkloadID   string    `json:"source_workload_id"`
	SourceCheckpointID string    `json:"source_checkpoint_id"`
	SourceRootPath     string    `json:"source_root_path"`
	ForkWorkloadID     string    `json:"fork_workload_id"`
	ForkCheckpointID   string    `json:"fork_checkpoint_id,omitempty"`
	ForkName           string    `json:"fork_name"`
	State              ForkState `json:"state"`
	RootPath           string    `json:"root_path"`
	StagingRoot        string    `json:"staging_root"`
	ImagesDirectory    string    `json:"images_directory"`
	Generation         uint32    `json:"generation"`
	Activated          bool      `json:"activated"`
	PID                int       `json:"pid,omitempty"`
	CreatedWorkload    bool      `json:"created_workload"`
	PlainBytes         int64     `json:"plain_bytes"`
	StoredBytes        int64     `json:"stored_bytes"`
	DeduplicatedBytes  int64     `json:"deduplicated_bytes"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	Error              string    `json:"error,omitempty"`
}

// ForkOptions describes one fork request. RootPath and Name are optional; the
// forker derives collision-free defaults from the source workload.
type ForkOptions struct {
	Name         string
	RootPath     string
	CheckpointID string
	Activate     bool
	Timeout      time.Duration
}

type Forker struct {
	service *Service
	records *securestore.EncryptedCollection[ForkRecord]
	logger  *slog.Logger
}

func OpenForker(service *Service) (*Forker, error) {
	records, err := securestore.OpenEncryptedCollection[ForkRecord](filepath.Join(service.stateDir, "metadata", "forks.enc.json"), "fork-transactions-v1", service.repository.keys)
	if err != nil {
		return nil, err
	}
	forker := &Forker{service: service, records: records, logger: service.logger}
	if err := forker.Recover(context.Background()); err != nil {
		return nil, err
	}
	return forker, nil
}

func (f *Forker) Get(id string) (ForkRecord, error) {
	return f.records.Get(id)
}

func (f *Forker) List() []ForkRecord {
	values := f.records.List()
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.After(values[j].CreatedAt) })
	return values
}

// Fork derives an independent workload from a point-in-time checkpoint of
// source. The fork receives its own workload identity, its own root directory,
// its own encryption key namespace, and its own full checkpoint, so afterwards
// the two workloads are controlled entirely separately. SHIFT records the fork
// lineage but deliberately implements no state-merge operation.
func (f *Forker) Fork(parent context.Context, sourceWorkloadID string, options ForkOptions) (record ForkRecord, err error) {
	ctx, cancel := commandTimeout(parent, options.Timeout)
	defer cancel()
	source, err := f.service.runtime.Get(sourceWorkloadID)
	if err != nil {
		return ForkRecord{}, err
	}
	manifest, err := f.forkPoint(ctx, source, options)
	if err != nil {
		return ForkRecord{}, err
	}
	filesystemAsset, ok := assetByName(manifest.Assets, "filesystem-root")
	if !ok {
		return ForkRecord{}, errors.New("fork point has no filesystem root asset")
	}
	if err := f.service.chunks.ValidateAsset(ctx, manifest.Workload.ID, filesystemAsset); err != nil {
		return ForkRecord{}, fmt.Errorf("validate fork point filesystem: %w", err)
	}
	processAssets, err := processAssetChain(ctx, f.service.repository, f.service.chunks, manifest)
	if err != nil {
		return ForkRecord{}, err
	}
	sessionID, err := model.NewID()
	if err != nil {
		return ForkRecord{}, err
	}
	forkWorkloadID, err := model.NewID()
	if err != nil {
		return ForkRecord{}, err
	}
	sourceRoot := filepath.Clean(manifest.Workload.RootPath)
	forkRoot, err := forkRootPath(sourceRoot, options.RootPath, sessionID)
	if err != nil {
		return ForkRecord{}, err
	}
	forkSpec, err := forkSpecification(manifest.Workload, forkWorkloadID, options.Name, forkRoot, sessionID, manifest.ID)
	if err != nil {
		return ForkRecord{}, err
	}
	record = ForkRecord{
		ID: sessionID, SourceWorkloadID: manifest.Workload.ID, SourceCheckpointID: manifest.ID,
		SourceRootPath: sourceRoot, ForkWorkloadID: forkWorkloadID, ForkName: forkSpec.Name,
		State: ForkPreparing, RootPath: forkRoot,
		StagingRoot:     filepath.Join(filepath.Dir(forkRoot), ".shift-fork-"+sessionID),
		ImagesDirectory: filepath.Join(f.service.stateDir, "forks", sessionID),
		Generation:      forkSpec.Lineage.Generation, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := f.records.Put(record.ID, record); err != nil {
		return ForkRecord{}, err
	}
	defer func() {
		if err != nil {
			if rollbackErr := f.rollback(ctx, &record, err.Error()); rollbackErr != nil {
				f.logger.Error("fork rollback failed", "fork_id", record.ID, "error", rollbackErr)
			}
		}
	}()
	// The fork's declared ports are reserved when the fork is activated —
	// a fork that is only materialized runs nothing and holds nothing. See
	// activate().
	if err = f.materialize(ctx, &record, manifest, filesystemAsset, processAssets, forkSpec); err != nil {
		return record, err
	}
	if !options.Activate {
		f.logger.Info("workload forked", "fork_id", record.ID, "source_workload_id", record.SourceWorkloadID,
			"fork_workload_id", record.ForkWorkloadID, "fork_checkpoint_id", record.ForkCheckpointID, "root_path", record.RootPath)
		return f.commit(&record)
	}
	if err = f.activate(ctx, &record, manifest, forkSpec); err != nil {
		return record, err
	}
	f.logger.Info("workload forked and activated", "fork_id", record.ID, "source_workload_id", record.SourceWorkloadID,
		"fork_workload_id", record.ForkWorkloadID, "pid", record.PID, "root_path", record.RootPath)
	return f.commit(&record)
}

// forkPoint resolves the checkpoint the fork derives from. A running source is
// checkpointed on the spot and left running, which is what makes forking a live
// workload possible without disturbing it.
func (f *Forker) forkPoint(ctx context.Context, source model.Workload, options ForkOptions) (model.CheckpointManifest, error) {
	if options.CheckpointID != "" {
		manifest, err := f.service.repository.Load(options.CheckpointID)
		if err != nil {
			return model.CheckpointManifest{}, err
		}
		if manifest.Workload.ID != source.Spec.ID {
			return model.CheckpointManifest{}, fmt.Errorf("checkpoint %s belongs to workload %s", manifest.ID, manifest.Workload.ID)
		}
		return manifest, nil
	}
	if source.Process != nil && linuxplatform.ProcessAlive(source.Process.PID, source.Process.ProcStartTicks) {
		return f.service.Create(ctx, source.Spec.ID, CreateOptions{LeaveRunning: true, Timeout: options.Timeout})
	}
	if source.LatestCheckpointID == "" {
		return model.CheckpointManifest{}, errors.New("source workload is not running and has no checkpoint to fork from")
	}
	return f.service.repository.Load(source.LatestCheckpointID)
}

// materialize writes the fork's own filesystem copy and its own full checkpoint.
// Chunks are re-encrypted under the fork's key namespace so that deleting or
// rotating the source workload can never invalidate the fork's state.
func (f *Forker) materialize(ctx context.Context, record *ForkRecord, manifest model.CheckpointManifest, filesystemAsset model.AssetManifest, processAssets []model.AssetManifest, forkSpec model.WorkloadSpec) error {
	if err := os.MkdirAll(filepath.Dir(record.RootPath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(record.StagingRoot, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(record.ImagesDirectory, 0o700); err != nil {
		return err
	}
	if err := extractDirectory(ctx, f.service.chunks, manifest.Workload.ID, filesystemAsset, record.StagingRoot); err != nil {
		return err
	}
	extracted, err := validateExtractedRoot(record.StagingRoot, filepath.Base(record.SourceRootPath))
	if err != nil {
		return err
	}
	for _, processAsset := range processAssets {
		if err := extractDirectory(ctx, f.service.chunks, manifest.Workload.ID, processAsset, record.ImagesDirectory); err != nil {
			return err
		}
	}
	images := filepath.Join(record.ImagesDirectory, "images")
	if info, statErr := os.Stat(images); statErr != nil || !info.IsDir() {
		return errors.New("fork point process image directory is missing")
	}
	if _, statErr := os.Lstat(record.RootPath); statErr == nil {
		return fmt.Errorf("fork root %s already exists", record.RootPath)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := materializeRoot(extracted, record.RootPath); err != nil {
		return fmt.Errorf("activate fork filesystem: %w", err)
	}
	if err := syncDir(filepath.Dir(record.RootPath)); err != nil {
		return err
	}
	_, created, err := f.service.runtime.PrepareRestore(forkSpec)
	if err != nil {
		return err
	}
	record.CreatedWorkload = created
	forkManifest, err := f.captureFork(ctx, manifest, forkSpec, images)
	if err != nil {
		return err
	}
	record.ForkCheckpointID = forkManifest.ID
	record.PlainBytes = forkManifest.Metrics.PlainBytes
	record.StoredBytes = forkManifest.Metrics.StoredBytes
	record.DeduplicatedBytes = forkManifest.Metrics.DeduplicatedBytes
	if _, err := f.service.runtime.MarkCheckpoint(forkSpec.ID, forkManifest.ID, false); err != nil {
		return err
	}
	return f.transition(record, ForkMaterialized, "")
}

// captureFork writes a self-contained full checkpoint owned by the fork. The
// CRIU image chain is flattened first, so the fork's checkpoint has no parent
// and stays valid independently of the source's retention policy.
func (f *Forker) captureFork(ctx context.Context, manifest model.CheckpointManifest, forkSpec model.WorkloadSpec, images string) (model.CheckpointManifest, error) {
	started := time.Now().UTC()
	checkpointID, err := model.NewID()
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	imageResult, err := captureDirectory(ctx, f.service.chunks, forkSpec.ID, "process-state", images, nil, 20)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	filesystemResult, err := captureDirectory(ctx, f.service.chunks, forkSpec.ID, "filesystem-root", forkSpec.RootPath, exclusionsForRoot(forkSpec), 10)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	machine, err := f.service.inventory.Inspect(ctx)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	assets := []model.AssetManifest{filesystemResult.Asset, imageResult.Asset}
	metrics := model.CheckpointMetrics{StartedAt: started}
	for _, asset := range assets {
		metrics.PlainBytes += asset.PlainSize
		metrics.StoredBytes += asset.StoredSize
		metrics.ChunkCount += len(asset.Chunks)
	}
	metrics.DeduplicatedBytes = filesystemResult.DeduplicatedBytes + imageResult.DeduplicatedBytes
	metrics.CompletedAt = time.Now().UTC()
	metrics.Duration = metrics.CompletedAt.Sub(started)
	forkManifest := model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		ID: checkpointID, Kind: model.CheckpointFull, Workload: forkSpec,
		SourceIdentity: f.service.identity.Machine, SourceMachine: machine,
		CreatedAt: started, Assets: assets, RequiredBytes: metrics.PlainBytes, Metrics: metrics,
		Engine:          manifest.Engine,
		StateInventory:  manifest.StateInventory,
		DeviceNeeds:     forkSpec.Resources.GPUs,
		CompatibilityID: compatibilityID(machine, forkSpec),
	}
	forkManifest.Engine.LeaveRunning = false
	forkManifest.Engine.ParentImages = false
	forkManifest.Engine.PreCopy = false
	keyVersion, err := f.service.chunks.ActiveKeyVersion(forkSpec.ID)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	forkManifest.Security.KeyVersion = keyVersion
	if err := SignManifest(&forkManifest, f.service.identity); err != nil {
		return model.CheckpointManifest{}, err
	}
	if err := f.service.repository.Save(forkManifest); err != nil {
		return model.CheckpointManifest{}, err
	}
	if f.service.mirror != nil {
		if _, mirrorErr := f.service.MirrorCheckpoint(ctx, checkpointID); mirrorErr != nil {
			return model.CheckpointManifest{}, fmt.Errorf("fork checkpoint %s persisted locally but object-store mirror failed: %w", checkpointID, mirrorErr)
		}
	}
	return forkManifest, nil
}

// activate restores the fork's process tree. The images record the source's
// absolute root path, so the fork's own copy is bind mounted there inside a
// private mount namespace; both process trees then run at once without either
// observing the other's files.
func (f *Forker) activate(ctx context.Context, record *ForkRecord, manifest model.CheckpointManifest, forkSpec model.WorkloadSpec) error {
	if err := f.transition(record, ForkActivating, ""); err != nil {
		return err
	}
	// The fork's declared host ports are reserved before the process comes
	// up: forking a port-publishing workload on the machine where the
	// original still runs fails here with a clear conflict instead of dying
	// inside the CRIU restore with an address-in-use error.
	if len(forkSpec.Ports) > 0 {
		if f.service.network == nil {
			return errors.New("this agent has no network layer configured; a workload with declared ports cannot be forked")
		}
		mappings, mappingsErr := network.MappingsFor(forkSpec)
		if mappingsErr != nil {
			return fmt.Errorf("resolve fork ports: %w", mappingsErr)
		}
		if err := f.service.network.Prepare(forkSpec.ID, mappings); err != nil {
			return fmt.Errorf("reserve fork host ports: %w", err)
		}
	}
	images := filepath.Join(record.ImagesDirectory, "images")
	mounts := []BindMount{}
	if record.RootPath != record.SourceRootPath {
		mounts = append(mounts, BindMount{Source: record.RootPath, Target: record.SourceRootPath})
	}
	pid, err := f.service.criu.Restore(ctx, RestoreOptions{
		ImagesDirectory: images,
		TCPState:        manifest.Engine.TCPState, ShellJob: manifest.Engine.ShellJob,
		FileLocks: manifest.Engine.FileLocks, ExternalUNIX: true, ManageCgroups: "soft",
		BindMounts: mounts,
	})
	if err != nil {
		return err
	}
	if _, err := f.service.runtime.AdoptRestored(forkSpec.ID, pid); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return err
	}
	if _, err := f.service.runtime.Resume(forkSpec.ID); err != nil {
		return fmt.Errorf("resume forked process: %w", err)
	}
	record.PID = pid
	record.Activated = true
	if err := f.transition(record, ForkRunning, ""); err != nil {
		return err
	}
	deadline := time.NewTimer(500 * time.Millisecond)
	defer deadline.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-deadline.C:
	}
	alive, err := f.service.runtime.Get(forkSpec.ID)
	if err != nil {
		return err
	}
	if alive.Process == nil || !linuxplatform.ProcessAlive(alive.Process.PID, alive.Process.ProcStartTicks) {
		return errors.New("forked process exited during validation")
	}
	// The forked process is up: publish its ports and tell it what its
	// network looks like. A fork gets its own virtual identity (it is a new
	// workload), and its status document says it was forked — the sockets
	// it holds are copies from the checkpoint, and peers that find it do so
	// at the fork's own ports.
	if f.service.network != nil && len(forkSpec.Ports) > 0 {
		mappings, mappingsErr := network.MappingsFor(forkSpec)
		if mappingsErr != nil {
			return fmt.Errorf("resolve fork ports: %w", mappingsErr)
		}
		if err := f.service.network.Activate(ctx, forkSpec.ID, mappings); err != nil {
			return fmt.Errorf("publish fork ports: %w", err)
		}
		plan, planErr := network.PlanFor(forkSpec)
		if planErr != nil {
			return fmt.Errorf("network plan: %w", planErr)
		}
		if _, err := f.service.network.RecordStatus(forkSpec, plan, network.OperationFork, record.ID, network.StatusCompleted, "forked from workload "+record.SourceWorkloadID, false); err != nil {
			return err
		}
	}
	return nil
}

func (f *Forker) commit(record *ForkRecord) (ForkRecord, error) {
	_ = os.RemoveAll(record.StagingRoot)
	if !record.Activated {
		_ = os.RemoveAll(record.ImagesDirectory)
	}
	if err := f.transition(record, ForkCommitted, ""); err != nil {
		return *record, err
	}
	return *record, nil
}

func (f *Forker) transition(record *ForkRecord, to ForkState, reason string) error {
	if record.State != to && !CanTransitionFork(record.State, to) {
		return fmt.Errorf("invalid fork transition %s -> %s", record.State, to)
	}
	record.State = to
	if reason != "" {
		record.Error = reason
	}
	record.UpdatedAt = time.Now().UTC()
	return f.records.Put(record.ID, *record)
}

// Rollback reverses a fork that has not been committed. A committed fork is a
// normal independent workload and must be deleted through the workload API
// instead, so that removing state is always an explicit operator action.
func (f *Forker) Rollback(ctx context.Context, id, reason string) (ForkRecord, error) {
	record, err := f.records.Get(id)
	if err != nil {
		return ForkRecord{}, err
	}
	if record.State == ForkRolledBack {
		return record, nil
	}
	if record.State == ForkCommitted {
		return ForkRecord{}, errors.New("a committed fork is an independent workload; delete the workload instead")
	}
	if err := f.rollback(ctx, &record, reason); err != nil {
		return record, err
	}
	return record, nil
}

func (f *Forker) rollback(ctx context.Context, record *ForkRecord, reason string) error {
	record.State = ForkRollingBack
	record.Error = reason
	record.UpdatedAt = time.Now().UTC()
	_ = f.records.Put(record.ID, *record)
	if workload, err := f.service.runtime.Get(record.ForkWorkloadID); err == nil && workload.Process != nil {
		_, _ = f.service.runtime.Stop(record.ForkWorkloadID, 5*time.Second)
	}
	// Withdraw the fork's network presence along with everything else.
	if f.service.network != nil {
		f.service.network.Deactivate(record.ForkWorkloadID, rollbackDrainGrace)
	}
	if record.CreatedWorkload {
		_ = f.service.runtime.Delete(record.ForkWorkloadID)
	}
	if record.ForkCheckpointID != "" {
		if err := f.service.repository.Delete(record.ForkCheckpointID); err != nil {
			return fmt.Errorf("remove fork checkpoint: %w", err)
		}
	}
	if record.RootPath != "" && record.RootPath != record.SourceRootPath {
		if err := os.RemoveAll(record.RootPath); err != nil {
			return fmt.Errorf("remove fork root: %w", err)
		}
	}
	_ = os.RemoveAll(record.StagingRoot)
	_ = os.RemoveAll(record.ImagesDirectory)
	record.State = ForkRolledBack
	record.UpdatedAt = time.Now().UTC()
	if err := f.records.Put(record.ID, *record); err != nil {
		return err
	}
	return nil
}

// Recover reverses forks that were interrupted before commit. A fork that was
// materialized but never committed left a half-registered workload behind, so
// removing it keeps the workload list truthful after a crash.
func (f *Forker) Recover(ctx context.Context) error {
	for _, record := range f.records.List() {
		switch record.State {
		case ForkPreparing, ForkMaterialized, ForkActivating, ForkRollingBack:
			if err := f.rollback(ctx, &record, "agent restarted before the fork was committed"); err != nil {
				return fmt.Errorf("recover fork %s: %w", record.ID, err)
			}
		case ForkRunning:
			workload, err := f.service.runtime.Get(record.ForkWorkloadID)
			if err != nil || workload.Process == nil || !linuxplatform.ProcessAlive(workload.Process.PID, workload.Process.ProcStartTicks) {
				if err := f.rollback(ctx, &record, "forked process missing after agent restart"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// forkRootPath validates an operator-supplied fork root or derives one beside
// the source root. Nesting either root inside the other is rejected because a
// later checkpoint of one would capture the other's state.
func forkRootPath(sourceRoot, requested, sessionID string) (string, error) {
	candidate := requested
	if candidate == "" {
		candidate = sourceRoot + "-fork-" + sessionID[:8]
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if absolute == string(filepath.Separator) {
		return "", errors.New("the filesystem root cannot be a fork root")
	}
	if absolute == sourceRoot {
		return "", errors.New("fork root must differ from the source workload root")
	}
	if pathContains(sourceRoot, absolute) || pathContains(absolute, sourceRoot) {
		return "", errors.New("fork root and source root must not be nested")
	}
	return absolute, nil
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// forkSpecification rewrites the source specification for the fork: a new
// identity, a new root, paths rebased onto that root, and published host ports
// dropped because two workloads cannot occupy the same host port.
func forkSpecification(source model.WorkloadSpec, forkID, name, forkRoot, sessionID, checkpointID string) (model.WorkloadSpec, error) {
	spec := source
	spec.ID = forkID
	spec.Name = name
	if spec.Name == "" {
		spec.Name = source.Name + "-fork-" + sessionID[:8]
	}
	sourceRoot := filepath.Clean(source.RootPath)
	spec.RootPath = forkRoot
	rebased, err := rebasePath(sourceRoot, forkRoot, source.WorkingDir)
	if err != nil {
		return model.WorkloadSpec{}, err
	}
	spec.WorkingDir = rebased
	spec.Paths = make([]model.PathSpec, 0, len(source.Paths))
	for _, path := range source.Paths {
		rebasedPath, err := rebasePath(sourceRoot, forkRoot, path.Path)
		if err != nil {
			return model.WorkloadSpec{}, err
		}
		path.Path = rebasedPath
		spec.Paths = append(spec.Paths, path)
	}
	spec.Ports = make([]model.PortSpec, 0, len(source.Ports))
	for _, port := range source.Ports {
		port.HostPort = 0
		spec.Ports = append(spec.Ports, port)
	}
	if source.Environment != nil {
		environment := make(map[string]string, len(source.Environment))
		for key, value := range source.Environment {
			environment[key] = value
		}
		spec.Environment = environment
	}
	forkGeneration := uint32(1)
	if source.Lineage != nil {
		forkGeneration = source.Lineage.Generation + 1
	}
	spec.Lineage = &model.Lineage{
		SourceWorkloadID: source.ID, SourceCheckpointID: checkpointID,
		SourceRootPath: sourceRoot, Generation: forkGeneration, ForkedAt: time.Now().UTC(),
	}
	spec.CreatedAt = time.Time{}
	if err := spec.Normalize(); err != nil {
		return model.WorkloadSpec{}, err
	}
	return spec, nil
}

func rebasePath(sourceRoot, forkRoot, path string) (string, error) {
	clean := filepath.Clean(path)
	if clean == sourceRoot {
		return forkRoot, nil
	}
	relative, err := filepath.Rel(sourceRoot, clean)
	if err != nil {
		return "", err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is outside the source workload root", path)
	}
	return filepath.Join(forkRoot, relative), nil
}
