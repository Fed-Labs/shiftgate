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
	"sync"
	"syscall"
	"time"

	"shift.dev/shift/internal/compatibility"
	"shift.dev/shift/internal/filesystem"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/network"
	"shift.dev/shift/internal/observability"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	"shift.dev/shift/internal/securestore"
)

// Clone set bounds. The count cap keeps one mistyped command from grinding a
// machine under a hundred restore materializations; the parallelism cap bounds
// concurrent CRIU restores, each of which holds a full copy of the workload's
// memory while it runs.
const (
	MaxCloneCount        = 128
	MaxCloneParallel     = 16
	DefaultCloneParallel = 4
)

type CloneState string

const (
	ClonePreparing   CloneState = "PREPARING"
	CloneCloning     CloneState = "CLONING"
	CloneCommitted   CloneState = "COMMITTED"
	CloneRollingBack CloneState = "ROLLING_BACK"
	CloneRolledBack  CloneState = "ROLLED_BACK"
)

// CloneMember is one workload inside a clone set. Members are decided before
// any work starts — names, roots, and identities all exist in the record from
// the PREPARING state — so recovery after a crash knows every path a partial
// set may have touched.
type CloneMember struct {
	Index      int    `json:"index"`
	WorkloadID string `json:"workload_id"`
	Name       string `json:"name"`
	RootPath   string `json:"root_path"`
	State      string `json:"state"`
	PID        int    `json:"pid,omitempty"`
	// CreatedWorkload marks members whose workload record exists in the
	// runtime manager; recovery deletes exactly those.
	CreatedWorkload bool `json:"created_workload"`
	// FilesystemCloned is true when this member's filesystem copy took the
	// copy-on-write fast path, so the record never claims shared extents on a
	// filesystem that cannot share them.
	FilesystemCloned bool `json:"filesystem_cloned"`
	// RestoreAttempted marks members that reached the CRIU restore step — the
	// point from which a clone is a restore outcome in the diagnostics
	// series. Members that failed earlier never restored anything.
	RestoreAttempted bool   `json:"restore_attempted,omitempty"`
	DurationMS       int64  `json:"duration_ms,omitempty"`
	Error            string `json:"error,omitempty"`
}

func (member CloneMember) attemptedRestore() bool {
	return member.RestoreAttempted
}

// CloneRecord is the persisted transaction for one clone set. Like a fork
// record it is written before any filesystem mutation and updated as members
// progress, so an agent restart can reverse a partially materialized set.
//
// A clone set is deliberately cheaper than the same number of forks: every
// member reads the source checkpoint's stored chunks directly — the way a
// restore does — so no new chunks are written and the state is stored once
// for the whole set. What each member owns outright is its workload identity,
// its root directory, and every checkpoint it takes afterwards. The trade is
// recorded here: the set names its source checkpoint, and that checkpoint's
// manifest must exist for the set to be created again.
type CloneRecord struct {
	ID               string        `json:"id"`
	CheckpointID     string        `json:"checkpoint_id"`
	SourceWorkloadID string        `json:"source_workload_id"`
	SourceRootPath   string        `json:"source_root_path"`
	Count            int           `json:"count"`
	NamePrefix       string        `json:"name_prefix,omitempty"`
	Parallel         int           `json:"parallel"`
	State            CloneState    `json:"state"`
	SetDirectory     string        `json:"set_directory"`
	StagingRoot      string        `json:"staging_root"`
	ImagesRoot       string        `json:"images_root"`
	Members          []CloneMember `json:"members"`
	PlainBytes       int64         `json:"plain_bytes"`
	StoredBytes      int64         `json:"stored_bytes"`
	ClonedFiles      int           `json:"cloned_files"`
	CopiedFiles      int           `json:"copied_files"`
	DurationMS       int64         `json:"duration_ms,omitempty"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
	Error            string        `json:"error,omitempty"`
}

// CloneOptions describes one clone request. Zero values mean the defaults:
// one member, the derived name prefix, the default parallelism, and the
// standard command timeout.
type CloneOptions struct {
	Count      int
	NamePrefix string
	Parallel   int
	Timeout    time.Duration
}

type Cloner struct {
	service *Service
	records *securestore.EncryptedCollection[CloneRecord]
	logger  *slog.Logger
	// mu serializes record mutations across the set's parallel member
	// workers; the encrypted collection itself is only ever written under it.
	mu sync.Mutex
}

func OpenCloner(service *Service) (*Cloner, error) {
	records, err := securestore.OpenEncryptedCollection[CloneRecord](filepath.Join(service.stateDir, "metadata", "clones.enc.json"), "clone-transactions-v1", service.repository.keys)
	if err != nil {
		return nil, err
	}
	cloner := &Cloner{service: service, records: records, logger: service.logger}
	if err := cloner.Recover(context.Background()); err != nil {
		return nil, err
	}
	return cloner, nil
}

func (c *Cloner) Get(id string) (CloneRecord, error) {
	return c.records.Get(id)
}

func (c *Cloner) List() []CloneRecord {
	values := c.records.List()
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.After(values[j].CreatedAt) })
	return values
}

// Clone derives count independent, running workloads from one checkpoint, all
// on this machine. The set is all-or-nothing: any member failure rolls back
// every member — processes stopped, workloads deleted, roots removed — and the
// record names the member that failed and why. Nothing half-built survives.
func (c *Cloner) Clone(parent context.Context, checkpointID string, options CloneOptions) (record CloneRecord, err error) {
	if options.Count < 1 {
		return CloneRecord{}, errors.New("clone count must be at least 1")
	}
	if options.Count > MaxCloneCount {
		return CloneRecord{}, fmt.Errorf("clone count is capped at %d per set", MaxCloneCount)
	}
	parallel := options.Parallel
	if parallel == 0 {
		parallel = DefaultCloneParallel
	}
	if parallel < 1 || parallel > MaxCloneParallel {
		return CloneRecord{}, fmt.Errorf("clone parallelism must be between 1 and %d", MaxCloneParallel)
	}
	ctx, cancel := commandTimeout(parent, options.Timeout)
	defer cancel()
	manifest, err := c.service.repository.Load(checkpointID)
	if err != nil {
		return CloneRecord{}, err
	}
	destination, err := c.service.inventory.Inspect(ctx)
	if err != nil {
		return CloneRecord{}, err
	}
	if report := (compatibility.Checker{}).Check(manifest, destination); !report.Compatible {
		return CloneRecord{}, compatibilityError(report)
	}
	// Every clone restores the checkpointed process, which rebinds the
	// workload's declared ports in the shared host network namespace — two
	// clones of a port-declaring workload would fight over the same address,
	// and a set that silently came up unreachable would be a false success.
	// One clone is exactly a fork's posture: it fails clearly at port
	// reservation when the address is taken.
	if options.Count > 1 && len(manifest.Workload.Ports) > 0 {
		return CloneRecord{}, errors.New("this workload declares TCP ports, and every clone would rebind them on the same machine; a port-declaring workload can only be cloned one at a time (use fork semantics for a single copy)")
	}
	filesystemAsset, ok := assetByName(manifest.Assets, "filesystem-root")
	if !ok {
		return CloneRecord{}, errors.New("checkpoint has no filesystem root asset")
	}
	if err := c.service.chunks.ValidateAsset(ctx, manifest.Workload.ID, filesystemAsset); err != nil {
		return CloneRecord{}, fmt.Errorf("validate clone filesystem: %w", err)
	}
	if _, err := processChain(ctx, c.service.repository, c.service.chunks, manifest); err != nil {
		return CloneRecord{}, err
	}
	setID, err := model.NewID()
	if err != nil {
		return CloneRecord{}, err
	}
	sourceRoot := filepath.Clean(manifest.Workload.RootPath)
	specs := make([]model.WorkloadSpec, options.Count)
	members := make([]CloneMember, options.Count)
	taken := make(map[string]bool, options.Count)
	for index := range members {
		spec, memberErr := cloneSpecification(manifest.Workload, setID, manifest.ID, index+1, options.NamePrefix)
		if memberErr != nil {
			return CloneRecord{}, memberErr
		}
		if taken[spec.Name] {
			return CloneRecord{}, fmt.Errorf("clone name %q is used twice inside the set", spec.Name)
		}
		taken[spec.Name] = true
		specs[index] = spec
		members[index] = CloneMember{Index: index + 1, WorkloadID: spec.ID, Name: spec.Name, RootPath: spec.RootPath, State: "PENDING"}
	}
	// A name colliding with an existing workload would fail that member
	// mid-set and roll the whole set back; refusing before any work starts
	// turns the same mistake into a millisecond error.
	for _, existing := range c.service.runtime.List() {
		if taken[existing.Spec.Name] {
			return CloneRecord{}, fmt.Errorf("workload name %q already exists", existing.Spec.Name)
		}
	}
	setDirectory := filepath.Join(c.service.stateDir, "clones", setID)
	now := time.Now().UTC()
	record = CloneRecord{
		ID: setID, CheckpointID: manifest.ID, SourceWorkloadID: manifest.Workload.ID,
		SourceRootPath: sourceRoot, Count: options.Count, NamePrefix: options.NamePrefix,
		Parallel: parallel, State: ClonePreparing, SetDirectory: setDirectory,
		StagingRoot: filepath.Join(setDirectory, "staging"), ImagesRoot: filepath.Join(setDirectory, "images", "images"),
		Members: members, PlainBytes: manifest.Metrics.PlainBytes, StoredBytes: manifest.Metrics.StoredBytes,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := c.put(record); err != nil {
		return CloneRecord{}, err
	}
	defer func() {
		if err != nil {
			if rollbackErr := c.rollback(ctx, &record, err.Error()); rollbackErr != nil {
				c.logger.Error("clone rollback failed", "clone_id", record.ID, "error", rollbackErr)
			}
			record = c.snapshot(record.ID)
		}
	}()
	// The set's shared staging: the checkpoint's filesystem and process image
	// chain are extracted from the chunk store exactly once, then every member
	// clones from that extraction — copy-on-write where the filesystem allows
	// it, a plain copy where it does not. This is what makes a clone set cost
	// one extraction plus N cheap clones instead of N full decrypt-and-extract
	// passes over the same chunks.
	err = os.MkdirAll(setDirectory, 0o700)
	if err != nil {
		return record, err
	}
	err = os.MkdirAll(record.StagingRoot, 0o700)
	if err != nil {
		return record, err
	}
	err = extractDirectory(ctx, c.service.chunks, manifest.Workload.ID, filesystemAsset, record.StagingRoot)
	if err != nil {
		return record, err
	}
	extractedRoot, err := validateExtractedRoot(record.StagingRoot, filepath.Base(sourceRoot))
	if err != nil {
		return record, err
	}
	err = materializeProcessChain(ctx, c.service.repository, c.service.chunks, manifest, filepath.Dir(record.ImagesRoot))
	if err != nil {
		return record, err
	}
	if info, statErr := os.Stat(record.ImagesRoot); statErr != nil || !info.IsDir() {
		return record, errors.New("checkpoint process image directory is missing")
	}
	err = c.transition(&record, CloneCloning, "")
	if err != nil {
		return record, err
	}
	// Bounded worker pool. The first member failure cancels the workers'
	// context, so in-flight CRIU restores abort instead of stacking more
	// work behind a set that is already doomed; the deferred rollback then
	// removes whatever the finished members left behind.
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	indices := make(chan int, options.Count)
	for index := range members {
		indices <- index
	}
	close(indices)
	failures := make(chan error, options.Count)
	workers := parallel
	if workers > options.Count {
		workers = options.Count
	}
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range indices {
				if memberErr := c.cloneMember(workerCtx, &record, manifest, specs[index], extractedRoot); memberErr != nil {
					failures <- fmt.Errorf("clone member %d: %w", index+1, memberErr)
					cancelWorkers()
					return
				}
			}
		}()
	}
	wait.Wait()
	close(failures)
	if failure := <-failures; failure != nil {
		return record, failure
	}
	record.DurationMS = time.Since(now).Milliseconds()
	err = c.commit(&record)
	if err != nil {
		return record, err
	}
	c.logger.Info("checkpoint cloned", "clone_id", record.ID, "checkpoint_id", manifest.ID,
		"count", record.Count, "cloned_files", record.ClonedFiles, "copied_files", record.CopiedFiles,
		"duration_ms", record.DurationMS)
	return record, nil
}

// commit removes the set's shared staging — the extraction every member
// cloned from — and each member's image copy, all under the set directory.
// Member roots and processes stay: a committed clone is a normal independent
// workload, controlled from here through the workload API like any other.
func (c *Cloner) commit(record *CloneRecord) error {
	if err := os.RemoveAll(record.SetDirectory); err != nil {
		return fmt.Errorf("remove clone staging: %w", err)
	}
	if c.service.diagnostics != nil {
		for _, member := range record.Members {
			// Members that never reached a restore are not restore outcomes;
			// counting them would inflate the series with workloads that
			// never existed.
			if member.attemptedRestore() {
				c.service.diagnostics.RestoreFinished(observability.OutcomeSuccess)
			}
		}
	}
	return c.transition(record, CloneCommitted, "")
}

// cloneMember materializes and restores one member of the set. Every step is
// member-local; the only shared inputs are the extracted staging root and the
// process image chain, which are read-only from here on.
func (c *Cloner) cloneMember(ctx context.Context, record *CloneRecord, manifest model.CheckpointManifest, spec model.WorkloadSpec, extractedRoot string) error {
	started := time.Now()
	index := memberIndex(record, spec.ID)
	target := &record.Members[index]
	c.setMember(record, index, func(m *CloneMember) { m.State = "MATERIALIZING" })
	if _, statErr := os.Lstat(spec.RootPath); statErr == nil {
		return fmt.Errorf("clone root %s already exists", spec.RootPath)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	stats, err := filesystem.CloneTree(extractedRoot, spec.RootPath)
	if err != nil {
		return fmt.Errorf("materialize clone filesystem: %w", err)
	}
	c.mu.Lock()
	record.ClonedFiles += stats.ClonedFiles
	record.CopiedFiles += stats.Files - stats.ClonedFiles
	target.FilesystemCloned = stats.Files > 0 && stats.ClonedFiles == stats.Files
	c.mu.Unlock()
	// Each member restores from its own copy of the image chain: CRIU writes
	// its work directory beside the images it reads, so a shared directory
	// would have concurrent restores overwriting each other's scratch state.
	// The copy is copy-on-write where the filesystem allows it, so on btrfs
	// or xfs it costs nothing but metadata.
	memberDirectory := filepath.Join(record.SetDirectory, fmt.Sprintf("member-%d", target.Index))
	if err := os.MkdirAll(memberDirectory, 0o700); err != nil {
		return err
	}
	memberImages := filepath.Join(memberDirectory, "images")
	if _, err = filesystem.CloneTree(record.ImagesRoot, memberImages); err != nil {
		return fmt.Errorf("materialize clone images: %w", err)
	}
	_, created, err := c.service.runtime.PrepareRestore(spec)
	if err != nil {
		return err
	}
	c.setMember(record, index, func(m *CloneMember) { m.CreatedWorkload = created })
	// A single clone of a port-declaring workload reserves its ports before
	// the process comes up — the same clear-conflict-instead-of-dying-inside-
	// CRIU behavior a fork gets. Sets larger than one were refused up front.
	if len(spec.Ports) > 0 {
		if c.service.network == nil {
			return errors.New("this agent has no network layer configured; a workload with declared ports cannot be cloned")
		}
		mappings, mappingsErr := network.MappingsFor(spec)
		if mappingsErr != nil {
			return fmt.Errorf("resolve clone ports: %w", mappingsErr)
		}
		if err := c.service.network.Prepare(spec.ID, mappings); err != nil {
			return fmt.Errorf("reserve clone host ports: %w", err)
		}
	}
	// The checkpointed images record the source's absolute root path, so the
	// clone's own copy is bind mounted there inside a private mount
	// namespace — the same mechanism a fork uses — and every clone of the set
	// runs at the path it was checkpointed at without observing any other
	// member's files.
	mounts := []BindMount{}
	if spec.RootPath != record.SourceRootPath {
		mounts = append(mounts, BindMount{Source: spec.RootPath, Target: record.SourceRootPath})
	}
	pid, err := c.service.criu.Restore(ctx, RestoreOptions{
		ImagesDirectory: memberImages,
		TCPState:        manifest.Engine.TCPState, ShellJob: manifest.Engine.ShellJob,
		FileLocks: manifest.Engine.FileLocks, ExternalUNIX: true, ManageCgroups: "soft",
		BindMounts: mounts,
	})
	if err != nil {
		return err
	}
	c.setMember(record, index, func(m *CloneMember) { m.RestoreAttempted = true })
	if _, err = c.service.runtime.AdoptRestored(spec.ID, pid); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return err
	}
	if _, err = c.service.runtime.Resume(spec.ID); err != nil {
		return fmt.Errorf("resume cloned process: %w", err)
	}
	deadline := time.NewTimer(500 * time.Millisecond)
	defer deadline.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-deadline.C:
	}
	alive, err := c.service.runtime.Get(spec.ID)
	if err != nil {
		return err
	}
	if alive.Process == nil || !linuxplatform.ProcessAlive(alive.Process.PID, alive.Process.ProcStartTicks) {
		return errors.New("cloned process exited during validation")
	}
	// The clone's ports publish and its status document is written only after
	// the process proved it is alive, exactly like a fork's activation: a
	// clone that came up unreachable would be a false success state.
	if c.service.network != nil && len(spec.Ports) > 0 {
		mappings, mappingsErr := network.MappingsFor(spec)
		if mappingsErr != nil {
			return fmt.Errorf("resolve clone ports: %w", mappingsErr)
		}
		if err := c.service.network.Activate(ctx, spec.ID, mappings); err != nil {
			return fmt.Errorf("publish clone ports: %w", err)
		}
		plan, planErr := network.PlanFor(spec)
		if planErr != nil {
			return fmt.Errorf("network plan: %w", planErr)
		}
		if _, err := c.service.network.RecordStatus(spec, plan, network.OperationClone, record.ID, network.StatusCompleted, "cloned from checkpoint "+record.CheckpointID, false); err != nil {
			return err
		}
	}
	duration := time.Since(started).Milliseconds()
	c.setMember(record, index, func(m *CloneMember) {
		m.PID = pid
		m.State = "RUNNING"
		m.DurationMS = duration
	})
	return nil
}

// Rollback reverses a clone set that has not been committed. A committed set
// is a fleet of normal independent workloads and must be torn down through
// the workload API — removing state is always an explicit operator action.
func (c *Cloner) Rollback(ctx context.Context, id, reason string) (CloneRecord, error) {
	record, err := c.records.Get(id)
	if err != nil {
		return CloneRecord{}, err
	}
	if record.State == CloneRolledBack {
		return record, nil
	}
	if record.State == CloneCommitted {
		return CloneRecord{}, errors.New("a committed clone set is a set of independent workloads; delete the workloads instead")
	}
	if err := c.rollback(ctx, &record, reason); err != nil {
		return record, err
	}
	return c.records.Get(id)
}

func (c *Cloner) rollback(ctx context.Context, record *CloneRecord, reason string) error {
	c.mu.Lock()
	originalState := record.State
	record.State = CloneRollingBack
	record.Error = reason
	record.UpdatedAt = time.Now().UTC()
	_ = c.records.Put(record.ID, *record)
	c.mu.Unlock()
	for index := range record.Members {
		member := &record.Members[index]
		if workload, err := c.service.runtime.Get(member.WorkloadID); err == nil && workload.Process != nil {
			_, _ = c.service.runtime.Stop(member.WorkloadID, 5*time.Second)
		}
		// Withdraw each member's network presence along with everything else.
		if c.service.network != nil {
			c.service.network.Deactivate(member.WorkloadID, rollbackDrainGrace)
		}
		if member.CreatedWorkload {
			_ = c.service.runtime.Delete(member.WorkloadID)
		}
		if member.RootPath != "" && member.RootPath != record.SourceRootPath {
			if err := os.RemoveAll(member.RootPath); err != nil {
				return fmt.Errorf("remove clone root: %w", err)
			}
		}
		member.State = "ROLLED_BACK"
	}
	// Each member that reached a restore is one failed restore outcome — a
	// member that never got that far restored nothing and counts nothing.
	if originalState != CloneRollingBack && c.service.diagnostics != nil {
		for _, member := range record.Members {
			if member.attemptedRestore() {
				c.service.diagnostics.RestoreFinished(observability.OutcomeFailure)
			}
		}
	}
	_ = os.RemoveAll(record.SetDirectory)
	record.State = CloneRolledBack
	record.UpdatedAt = time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.records.Put(record.ID, *record)
}

// Recover reverses clone sets interrupted before commit. A set that was
// mid-flight when the agent died left member workloads, roots, and a staging
// directory behind; removing all of them keeps the workload list truthful.
func (c *Cloner) Recover(ctx context.Context) error {
	for _, record := range c.records.List() {
		switch record.State {
		case ClonePreparing, CloneCloning, CloneRollingBack:
			if err := c.rollback(ctx, &record, "agent restarted before the clone set was committed"); err != nil {
				return fmt.Errorf("recover clone %s: %w", record.ID, err)
			}
		}
	}
	return nil
}

func (c *Cloner) transition(record *CloneRecord, to CloneState, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if record.State != to && record.State != CloneRollingBack && !cloneCanTransition(record.State, to) {
		return fmt.Errorf("invalid clone transition %s -> %s", record.State, to)
	}
	record.State = to
	if reason != "" {
		record.Error = reason
	}
	record.UpdatedAt = time.Now().UTC()
	return c.records.Put(record.ID, *record)
}

var cloneTransitions = map[CloneState]map[CloneState]bool{
	ClonePreparing:   {CloneCloning: true},
	CloneCloning:     {CloneCommitted: true},
	CloneRollingBack: {CloneRolledBack: true},
}

func cloneCanTransition(from, to CloneState) bool {
	return cloneTransitions[from][to]
}

// setMember applies one member mutation under the cloner lock and persists the
// whole record, so a set interrupted between members leaves a record naming
// exactly how far each member got.
func (c *Cloner) setMember(record *CloneRecord, index int, apply func(*CloneMember)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	apply(&record.Members[index])
	record.UpdatedAt = time.Now().UTC()
	_ = c.records.Put(record.ID, *record)
}

func (c *Cloner) put(record CloneRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.records.Put(record.ID, record)
}

// snapshot re-reads a record so the value returned to the caller reflects
// what the rollback actually left behind, not the in-flight copy.
func (c *Cloner) snapshot(id string) CloneRecord {
	if current, err := c.records.Get(id); err == nil {
		return current
	}
	return CloneRecord{}
}

func memberIndex(record *CloneRecord, workloadID string) int {
	for index := range record.Members {
		if record.Members[index].WorkloadID == workloadID {
			return index
		}
	}
	return -1
}

// cloneSpecification rewrites the checkpointed specification for one member:
// a new identity and name, a new root beside the source root, paths rebased
// onto that root, and published host ports dropped exactly the way a fork
// drops them. The lineage records the checkpoint every member of the set came
// from, so a future state-merge implementation could find the common
// ancestor from recorded facts.
func cloneSpecification(source model.WorkloadSpec, setID, checkpointID string, index int, prefix string) (model.WorkloadSpec, error) {
	spec := source
	workloadID, err := model.NewID()
	if err != nil {
		return model.WorkloadSpec{}, err
	}
	spec.ID = workloadID
	sourceRoot := filepath.Clean(source.RootPath)
	spec.RootPath = fmt.Sprintf("%s-clone-%s-%d", sourceRoot, setID[:8], index)
	trimmed := strings.TrimSpace(prefix)
	if trimmed != "" {
		spec.Name = fmt.Sprintf("%s-%d", trimmed, index)
	} else {
		spec.Name = fmt.Sprintf("%s-clone-%s-%d", source.Name, setID[:8], index)
	}
	rebased, err := rebasePath(sourceRoot, spec.RootPath, source.WorkingDir)
	if err != nil {
		return model.WorkloadSpec{}, err
	}
	spec.WorkingDir = rebased
	spec.Paths = make([]model.PathSpec, 0, len(source.Paths))
	for _, path := range source.Paths {
		rebasedPath, err := rebasePath(sourceRoot, spec.RootPath, path.Path)
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
	generation := uint32(1)
	if source.Lineage != nil {
		generation = source.Lineage.Generation + 1
	}
	spec.Lineage = &model.Lineage{
		SourceWorkloadID: source.ID, SourceCheckpointID: checkpointID, SourceRootPath: sourceRoot,
		Generation: generation, ForkedAt: time.Now().UTC(),
	}
	spec.CreatedAt = time.Time{}
	if err := spec.Normalize(); err != nil {
		return model.WorkloadSpec{}, err
	}
	return spec, nil
}
