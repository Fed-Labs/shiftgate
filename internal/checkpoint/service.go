package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/filesystem"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/observability"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	shiftruntime "shift.dev/shift/internal/runtime"
)

type CreateOptions struct {
	Kind         model.CheckpointKind
	ParentID     string
	LeaveRunning bool
	TCPState     bool
	Timeout      time.Duration
}

type Engine interface {
	Check(context.Context) error
	Version(context.Context) (string, error)
	PreDump(context.Context, DumpOptions) error
	Dump(context.Context, DumpOptions) error
	Restore(context.Context, RestoreOptions) (int, error)
}

type Service struct {
	stateDir        string
	runtime         *shiftruntime.Manager
	identity        *identity.Identity
	inventory       *linuxplatform.Inventory
	chunks          *chunkstore.Store
	repository      *Repository
	mirror          *Mirror
	criu            Engine
	network         DestinationNetwork
	diagnostics     *observability.Diagnostics
	logger          *slog.Logger
	createInFlight  map[string]struct{}
	createInFlightM sync.Mutex
}

func NewService(stateDir string, runtimeManager *shiftruntime.Manager, machineIdentity *identity.Identity, inventory *linuxplatform.Inventory, chunks *chunkstore.Store, repository *Repository, criu Engine, logger *slog.Logger) *Service {
	return &Service{stateDir: stateDir, runtime: runtimeManager, identity: machineIdentity, inventory: inventory, chunks: chunks, repository: repository, criu: criu, logger: logger, createInFlight: make(map[string]struct{})}
}

func (s *Service) SetMirror(mirror *Mirror) {
	s.mirror = mirror
}

// SetDiagnostics attaches the recorder for checkpoint outcome series. Every
// checkpoint attempt — from the API, the CLI, or a migration — is counted
// exactly once, in Create.
func (s *Service) SetDiagnostics(diagnostics *observability.Diagnostics) {
	s.diagnostics = diagnostics
}

// DiscardIndex drops a workload's changed-file index. Deleting a workload
// must not leave its file index behind; the index is derived state, not a
// record, and a reused state directory should not accumulate orphans.
func (s *Service) DiscardIndex(workloadID string) {
	if err := os.Remove(filepath.Join(s.stateDir, "indices", workloadID+".json")); err != nil && !os.IsNotExist(err) {
		s.logger.Warn("could not discard changed-file index", "workload_id", workloadID, "error", err)
	}
}

func (s *Service) Create(parent context.Context, workloadID string, options CreateOptions) (manifest model.CheckpointManifest, err error) {
	defer func() {
		if s.diagnostics == nil {
			return
		}
		if err != nil {
			s.diagnostics.CheckpointFinished(observability.OutcomeFailure)
			return
		}
		s.diagnostics.CheckpointFinished(observability.OutcomeSuccess)
	}()
	started := time.Now().UTC()
	// A checkpoint freezes the workload and mutates its runtime state; two
	// concurrent ones on the same workload would corrupt both. The guard
	// covers every caller — the API, a fork of a running source, a migration,
	// and the periodic policy scheduler — and turns a race into a refusal.
	s.createInFlightM.Lock()
	if _, busy := s.createInFlight[workloadID]; busy {
		s.createInFlightM.Unlock()
		return model.CheckpointManifest{}, fmt.Errorf("a checkpoint of workload %s is already in progress", workloadID)
	}
	s.createInFlight[workloadID] = struct{}{}
	s.createInFlightM.Unlock()
	defer func() {
		s.createInFlightM.Lock()
		delete(s.createInFlight, workloadID)
		s.createInFlightM.Unlock()
	}()
	workload, err := s.runtime.Get(workloadID)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	if workload.Process == nil || !linuxplatform.ProcessAlive(workload.Process.PID, workload.Process.ProcStartTicks) {
		return model.CheckpointManifest{}, errors.New("workload must be running or paused to checkpoint")
	}
	if options.Kind == "" {
		options.Kind = model.CheckpointFull
	}
	if options.ParentID != "" && options.Kind == model.CheckpointFull {
		// A parent is an unambiguous request for an incremental checkpoint.
		// Accepting the omitted kind keeps the local API and CLI ergonomic.
		options.Kind = model.CheckpointIncremental
	}
	if options.Kind != model.CheckpointFull && options.Kind != model.CheckpointIncremental {
		return model.CheckpointManifest{}, fmt.Errorf("unsupported checkpoint kind %q", options.Kind)
	}
	if options.Kind == model.CheckpointIncremental && options.ParentID == "" {
		return model.CheckpointManifest{}, errors.New("incremental checkpoint requires a parent checkpoint")
	}
	if options.Kind == model.CheckpointFull && options.ParentID != "" {
		return model.CheckpointManifest{}, errors.New("full checkpoint cannot have a parent checkpoint")
	}
	ctx, cancel := commandTimeout(parent, options.Timeout)
	defer cancel()
	if err := s.criu.Check(ctx); err != nil {
		return model.CheckpointManifest{}, err
	}
	criuVersion, err := s.criu.Version(ctx)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	checkpointID, err := model.NewID()
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	staging := filepath.Join(s.stateDir, "staging", "checkpoint-"+checkpointID)
	imagesDirectory := filepath.Join(staging, "images")
	if err := os.MkdirAll(imagesDirectory, 0o700); err != nil {
		return model.CheckpointManifest{}, err
	}
	defer os.RemoveAll(staging)
	originallyRunning := workload.Status == model.WorkloadRunning
	resumeOnFailure := originallyRunning
	defer func() {
		if err != nil && resumeOnFailure {
			if _, resumeErr := s.runtime.Resume(workload.Spec.ID); resumeErr != nil {
				s.logger.Error("resume workload after failed checkpoint", "workload_id", workload.Spec.ID, "error", resumeErr)
			}
		}
	}()
	parentImages := ""
	if options.ParentID != "" {
		parentImages = filepath.Join(staging, "parent-images")
		if err := os.MkdirAll(parentImages, 0o700); err != nil {
			return model.CheckpointManifest{}, err
		}
		if err := s.materializeParentImages(ctx, workload.Spec.ID, options.ParentID, parentImages); err != nil {
			return model.CheckpointManifest{}, err
		}
		parentImages = filepath.Join(parentImages, "images")
	}
	freezeStartedAt := time.Time{}
	if originallyRunning {
		if _, err := s.runtime.Pause(workload.Spec.ID); err != nil {
			return model.CheckpointManifest{}, fmt.Errorf("freeze workload: %w", err)
		}
		// The workload provably stopped executing at this instant; the
		// timestamp travels in the manifest so downtime is measured from
		// the real freeze, never from the checkpoint's start — which for a
		// live migration includes pre-copy passes that ran while the
		// workload was still running.
		freezeStartedAt = time.Now().UTC()
	}
	if err := s.criu.Dump(ctx, DumpOptions{
		PID: workload.Process.PID, ImagesDirectory: imagesDirectory, ParentImages: parentImages,
		TCPState: options.TCPState, ShellJob: true, FileLocks: true, ExternalUNIX: true,
		LeaveStopped: true, ManageCgroups: "soft",
		// A checkpoint that leaves the workload running can become the parent
		// of a later incremental checkpoint, so its dump must arm the memory
		// tracker — CRIU refuses to diff against an untracked parent.
		TrackMemory: options.LeaveRunning,
	}); err != nil {
		return model.CheckpointManifest{}, err
	}
	// The final image set references unchanged pages in its parent through the
	// parent symlink CRIU writes beside it, so the sibling image sets must
	// travel with it: pre-copy passes are packaged alongside images/ below,
	// and a retained parent chain is re-materialized at restore time. The sets
	// can never be overlaid into one directory — CRIU image files collide by
	// name across a chain, and the colliding older file is exactly the one
	// restore still needs.
	// The filesystem asset must describe the workload root exactly as it was
	// while frozen. Where the root sits on a snapshot-capable filesystem,
	// SHIFT takes an atomic copy-on-write snapshot and reads the archive from
	// it; everywhere else the archive is read straight from the frozen root.
	// The manifest records which happened — a torn or "probably consistent"
	// capture is never implied.
	filesystemInfo, err := filesystem.Probe(workload.Spec.RootPath)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	capture := model.FilesystemCapture{Filesystem: filesystemInfo.Name}
	captureRoot := workload.Spec.RootPath
	var pendingSnapshot *filesystem.Snapshot
	if provider := filesystem.DetectSnapshotProvider(filesystemInfo); provider == nil {
		capture.FrozenCapture = true
	} else if snapshot, snapshotErr := provider.Create(ctx, workload.Spec.RootPath, checkpointID); snapshotErr == nil {
		pendingSnapshot = &snapshot
		capture.Snapshot = provider.Name()
		captureRoot = snapshot.Path()
	} else {
		capture.FrozenCapture = true
		s.logger.Warn("filesystem snapshot unavailable; capturing from the frozen root",
			"workload_id", workload.Spec.ID, "error", snapshotErr)
	}
	if pendingSnapshot != nil {
		defer func() {
			if releaseErr := pendingSnapshot.Release(); releaseErr != nil {
				s.logger.Error("release filesystem snapshot", "workload_id", workload.Spec.ID, "error", releaseErr)
			}
		}()
	}
	// Changed-file accounting runs against the same view the archive reads,
	// so the counts and the tarball always describe one instant.
	rootExclusions := exclusionsForRoot(workload.Spec)
	indexPath := filepath.Join(s.stateDir, "indices", workload.Spec.ID+".json")
	previousIndex, indexErr := filesystem.LoadIndex(indexPath)
	if indexErr != nil {
		return model.CheckpointManifest{}, indexErr
	}
	currentIndex, indexErr := filesystem.IndexTree(captureRoot, rootExclusions)
	if indexErr != nil {
		return model.CheckpointManifest{}, indexErr
	}
	if previousIndex != nil {
		fileDiff := filesystem.Diff(previousIndex, currentIndex)
		capture.ChangedFiles = len(fileDiff.Changed)
		capture.AddedFiles = len(fileDiff.Added)
		capture.DeletedFiles = len(fileDiff.Deleted)
	}
	// With a consistent snapshot in hand the workload can run again while the
	// archive is read; without one it stays frozen until the bytes are stored,
	// because the archive must not race the process's writes.
	resumedEarly := false
	if pendingSnapshot != nil && originallyRunning && options.LeaveRunning {
		if _, err := s.runtime.Resume(workload.Spec.ID); err != nil {
			return model.CheckpointManifest{}, fmt.Errorf("resume workload after snapshot: %w", err)
		}
		resumedEarly = true
		resumeOnFailure = false
	}
	// The process-state asset carries the checkpoint's own image set. An
	// incremental checkpoint's asset stays delta-only; restore materializes
	// its ancestor chain beside it. A live-migration checkpoint never comes
	// through here — its pre-copy passes are packaged as separate assets by
	// the LiveSession as each pass completes.
	imageResult, err := captureDirectory(ctx, s.chunks, workload.Spec.ID, "process-state", staging, []string{filepath.Base(imagesDirectory)}, []string{"work"}, 20)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	filesystemResult, err := captureDirectory(ctx, s.chunks, workload.Spec.ID, "filesystem-root", filepath.Dir(captureRoot), []string{filepath.Base(captureRoot)}, rootExclusions, 10)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	// Dependency discovery reports the files outside the process image that
	// the command needs to run. Files inside the root travel with the
	// checkpoint; files outside it do not, and the manifest says which is
	// which so a destination cannot be surprised by a missing library.
	environment := make([]string, 0, len(workload.Spec.Environment))
	for key, value := range workload.Spec.Environment {
		environment = append(environment, key+"="+value)
	}
	discovery, discoveryErr := filesystem.DiscoverDependencies(workload.Spec.Command, workload.Spec.RootPath, environment)
	if discoveryErr != nil {
		return model.CheckpointManifest{}, fmt.Errorf("discover workload dependencies: %w", discoveryErr)
	}
	if len(discovery.Unresolved) > 0 {
		s.logger.Warn("some workload dependencies could not be located on this machine",
			"workload_id", workload.Spec.ID, "unresolved", strings.Join(discovery.Unresolved, ", "))
	}
	machine, err := s.inventory.Inspect(ctx)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	assets := []model.AssetManifest{filesystemResult.Asset, imageResult.Asset}
	metrics := model.CheckpointMetrics{StartedAt: started, FreezeStartedAt: freezeStartedAt}
	for _, asset := range assets {
		metrics.PlainBytes += asset.PlainSize
		metrics.StoredBytes += asset.StoredSize
		metrics.ChunkCount += len(asset.Chunks)
	}
	metrics.DeduplicatedBytes = filesystemResult.DeduplicatedBytes + imageResult.DeduplicatedBytes
	metrics.CompletedAt = time.Now().UTC()
	metrics.Duration = metrics.CompletedAt.Sub(started)
	manifest = model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		ID: checkpointID, ParentID: options.ParentID, Kind: options.Kind,
		Workload: workload.Spec, SourceIdentity: s.identity.Machine, SourceMachine: machine,
		CreatedAt: started, Assets: assets, RequiredBytes: metrics.PlainBytes, Metrics: metrics,
		Engine:         model.CheckpointEngineInfo{Name: "CRIU", Version: criuVersion, LeaveRunning: options.LeaveRunning, ParentImages: parentImages != "", TCPState: options.TCPState, ShellJob: true, FileLocks: true},
		StateInventory: stateInventory(options.TCPState), DeviceNeeds: workload.Spec.Resources.GPUs,
		Filesystem:      capture,
		Dependencies:    discovery.Dependencies,
		CompatibilityID: compatibilityID(machine, workload.Spec),
	}
	keyVersion, err := s.chunks.ActiveKeyVersion(workload.Spec.ID)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	manifest.Security.KeyVersion = keyVersion
	if err := SignManifest(&manifest, s.identity); err != nil {
		return model.CheckpointManifest{}, err
	}
	if err := s.repository.Save(manifest); err != nil {
		return model.CheckpointManifest{}, err
	}
	// The checkpoint exists now, so this instant's file index becomes the
	// baseline the next checkpoint's changed-file accounting diffs against.
	if err := os.MkdirAll(filepath.Join(s.stateDir, "indices"), 0o700); err != nil {
		return model.CheckpointManifest{}, err
	}
	if err := filesystem.SaveIndex(indexPath, currentIndex); err != nil {
		return model.CheckpointManifest{}, fmt.Errorf("persist changed-file index: %w", err)
	}
	if s.mirror != nil {
		_, mirrorErr := s.MirrorCheckpoint(ctx, checkpointID)
		if mirrorErr != nil {
			return model.CheckpointManifest{}, fmt.Errorf("checkpoint %s persisted locally but object-store mirror failed: %w", checkpointID, mirrorErr)
		}
	}
	resume := originallyRunning && options.LeaveRunning && !resumedEarly
	if resume {
		if _, err := s.runtime.Resume(workload.Spec.ID); err != nil {
			return model.CheckpointManifest{}, fmt.Errorf("checkpoint stored but source could not resume: %w", err)
		}
		resumeOnFailure = false
	}
	if _, err := s.runtime.MarkCheckpoint(workload.Spec.ID, checkpointID, originallyRunning && options.LeaveRunning); err != nil {
		return model.CheckpointManifest{}, err
	}
	resumeOnFailure = false
	s.logger.Info("checkpoint created", "checkpoint_id", checkpointID, "workload_id", workload.Spec.ID, "plain_bytes", metrics.PlainBytes, "stored_bytes", metrics.StoredBytes)
	return manifest, nil
}

// materializeParentImages reconstructs the parent checkpoint's CRIU image tree
// under destination: the parent's own image set at destination/images and each
// older ancestor nested one parent-images/ level deeper — the layout the
// parent symlinks inside those image sets point at, so the incremental dump
// CRIU is about to run can follow the whole chain.
func (s *Service) materializeParentImages(ctx context.Context, workloadID, parentID, destination string) error {
	parent, err := s.repository.Load(parentID)
	if err != nil {
		return fmt.Errorf("load parent checkpoint %s: %w", parentID, err)
	}
	if parent.Workload.ID != workloadID {
		return fmt.Errorf("parent checkpoint %s belongs to workload %s", parentID, parent.Workload.ID)
	}
	return materializeProcessChain(ctx, s.repository, s.chunks, parent, destination)
}

func (s *Service) Load(id string) (model.CheckpointManifest, error) {
	return s.repository.Load(id)
}

func (s *Service) List(workloadID string) []Summary {
	return s.repository.List(workloadID)
}

// Delete removes a checkpoint from the local repository. It refuses while any
// other retained checkpoint of the workload still needs it as an ancestor: an
// incremental checkpoint's images carry only a delta, so deleting its parent
// would quietly make the survivor unrestorable. Delete the descendant first.
// Chunks are content addressed and shared between checkpoints, so they are
// reclaimed by garbage collection rather than here; removing the manifest is
// what makes a checkpoint unreachable.
func (s *Service) Delete(id string) error {
	manifest, err := s.repository.Load(id)
	if err != nil {
		return err
	}
	if err := s.deleteUnlessNeeded(manifest); err != nil {
		return err
	}
	return s.repository.Delete(id)
}

// deleteUnlessNeeded walks every other checkpoint of the workload and refuses
// when one of them reaches the candidate through its parent chain.
func (s *Service) deleteUnlessNeeded(manifest model.CheckpointManifest) error {
	byID := make(map[string]Summary)
	for _, summary := range s.repository.List(manifest.Workload.ID) {
		byID[summary.ID] = summary
	}
	for _, summary := range byID {
		if summary.ID == manifest.ID {
			continue
		}
		hops := 0
		for parentID := summary.ParentID; parentID != ""; {
			hops++
			if hops > 256 {
				return fmt.Errorf("checkpoint parent chain of %s exceeds 256 entries", summary.ID)
			}
			if parentID == manifest.ID {
				return fmt.Errorf("checkpoint %s is still the parent of retained checkpoint %s", manifest.ID, summary.ID)
			}
			parent, ok := byID[parentID]
			if !ok {
				// The chain is already broken higher up; that is a fact to
				// report elsewhere, not a reason to widen the damage.
				break
			}
			parentID = parent.ParentID
		}
	}
	return nil
}

// PruneWorkload deletes the workload's oldest checkpoints until at most
// keepLast remain, returning the deleted ids. A checkpoint an older
// descendant still needs survives even when that leaves more than keepLast
// behind: keeping a count would mean making a retained incremental
// checkpoint unrestorable. Mirrored objects in the object store are
// content-addressed and shared; local pruning does not delete them there.
func (s *Service) PruneWorkload(workloadID string, keepLast int) ([]string, error) {
	if keepLast < 0 {
		return nil, errors.New("keep count cannot be negative")
	}
	summaries := s.repository.List(workloadID)
	if len(summaries) <= keepLast {
		return nil, nil
	}
	byID := make(map[string]Summary, len(summaries))
	for _, summary := range summaries {
		byID[summary.ID] = summary
	}
	// Retention protects more than the newest keepLast ids: every ancestor
	// those still reference is load-bearing and must survive too.
	protected := make(map[string]bool)
	for _, summary := range summaries[:keepLast] {
		protected[summary.ID] = true
		for parentID := summary.ParentID; parentID != ""; {
			parent, ok := byID[parentID]
			if !ok {
				break
			}
			if protected[parentID] {
				break
			}
			protected[parentID] = true
			parentID = parent.ParentID
		}
	}
	deleted := []string{}
	for _, summary := range summaries[keepLast:] {
		if protected[summary.ID] {
			continue
		}
		if err := s.Delete(summary.ID); err != nil {
			return deleted, err
		}
		deleted = append(deleted, summary.ID)
	}
	return deleted, nil
}

func (s *Service) Import(manifest model.CheckpointManifest) error {
	return s.repository.Save(manifest)
}

// MirrorCheckpoint retries object-store publication for a checkpoint that was
// already committed to the local encrypted repository. It does not alter
// workload runtime state, so callers can safely retry after transient remote
// failures or process restarts.
func (s *Service) MirrorCheckpoint(ctx context.Context, id string) (MirrorResult, error) {
	if s.mirror == nil {
		return MirrorResult{}, ErrMirrorDisabled
	}
	if id == "" {
		return MirrorResult{}, errors.New("checkpoint id is required")
	}
	manifest, err := s.repository.Load(id)
	if err != nil {
		return MirrorResult{}, err
	}
	result, err := s.mirror.Upload(ctx, manifest)
	if err != nil {
		return MirrorResult{}, err
	}
	if s.logger != nil {
		s.logger.Info("checkpoint mirrored", "checkpoint_id", id, "objects", result.Objects, "bytes", result.Bytes)
	}
	return result, nil
}

// CleanupMirrorUploads removes multipart uploads older than before. It is
// intentionally separate from checkpoint creation so operators can schedule
// cleanup without affecting workload or checkpoint state.
func (s *Service) CleanupMirrorUploads(ctx context.Context, before time.Time) (int, error) {
	if s.mirror == nil {
		return 0, ErrMirrorDisabled
	}
	return s.mirror.CleanupUploads(ctx, before)
}

func exclusionsForRoot(spec model.WorkloadSpec) []string {
	for _, path := range spec.Paths {
		if filepath.Clean(path.Path) == filepath.Clean(spec.RootPath) {
			return append([]string(nil), path.Exclusions...)
		}
	}
	return nil
}

func stateInventory(tcpState bool) []model.StateItem {
	networkClass := model.StateExternal
	networkAdapter := "application-reconnect"
	if tcpState {
		networkClass = model.StateMachineSpecific
		networkAdapter = "criu-tcp-repair"
	}
	return []model.StateItem{
		{Name: "process_tree", Class: model.StatePortable, Adapter: "criu", Required: true},
		{Name: "memory", Class: model.StatePortable, Adapter: "criu", Required: true},
		{Name: "threads", Class: model.StatePortable, Adapter: "criu", Required: true},
		{Name: "filesystem", Class: model.StatePortable, Adapter: "gnu-tar", Required: true},
		{Name: "environment", Class: model.StatePortable, Adapter: "shift-manifest", Required: true},
		{Name: "network_connections", Class: networkClass, Adapter: networkAdapter, Required: false, Description: "connections require CRIU TCP repair or application-level reconnect"},
		{Name: "gpu_state", Class: model.StateMachineSpecific, Adapter: "vendor-checkpoint", Required: false, Description: "GPU state is only restorable when the vendor driver explicitly supports it"},
		{Name: "remote_services", Class: model.StateExternal, Adapter: "reconnect", Required: false},
	}
}

func compatibilityID(machine model.MachineCapabilities, workload model.WorkloadSpec) string {
	features := make([]string, 0)
	for feature, enabled := range machine.Features {
		if enabled && len(feature) > 4 && feature[:4] == "cpu." {
			features = append(features, feature)
		}
	}
	sort.Strings(features)
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%s\n%s\n%v", machine.OS, machine.Architecture, machine.Kernel, workload.NetworkPolicy, features)))
	return hex.EncodeToString(digest[:])
}
