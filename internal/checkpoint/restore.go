package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/compatibility"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/network"
	"shift.dev/shift/internal/observability"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	"shift.dev/shift/internal/securestore"
)

type RestoreState string

const (
	RestorePreparing          RestoreState = "PREPARING"
	RestoreFilesystemSwitched RestoreState = "FILESYSTEM_SWITCHED"
	RestoreProcessRunning     RestoreState = "PROCESS_RUNNING"
	RestoreValidated          RestoreState = "VALIDATED"
	RestoreCommitted          RestoreState = "COMMITTED"
	RestoreRollingBack        RestoreState = "ROLLING_BACK"
	RestoreRolledBack         RestoreState = "ROLLED_BACK"
	RestoreFailed             RestoreState = "FAILED"
)

type RestoreRecord struct {
	ID               string          `json:"id"`
	CheckpointID     string          `json:"checkpoint_id"`
	WorkloadID       string          `json:"workload_id"`
	State            RestoreState    `json:"state"`
	TargetRoot       string          `json:"target_root"`
	BackupRoot       string          `json:"backup_root,omitempty"`
	StagingRoot      string          `json:"staging_root"`
	RestoreDirectory string          `json:"restore_directory"`
	PID              int             `json:"pid,omitempty"`
	CreatedWorkload  bool            `json:"created_workload"`
	PreviousWorkload *model.Workload `json:"previous_workload,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	Error            string          `json:"error,omitempty"`
	// Lazy marks a restore that started its process before the memory image
	// was fully loaded: a lazy-pages daemon serves the rest through
	// userfaultfd as the workload touches it. The restore directory is
	// retained for that daemon, which reads pages from it, and is removed
	// once the daemon exits — when it has finished serving: every image page
	// transferred, or the process gone.
	Lazy bool `json:"lazy,omitempty"`
	// TimeToFirstExecutionMS measures the CRIU restore call: from its start
	// to the moment the restored tree is executing. For a lazy restore that
	// is long before the memory is resident (pages keep streaming); for an
	// eager one it is the full restore. Both are recorded so the two paths
	// can be compared honestly.
	TimeToFirstExecutionMS int64 `json:"time_to_first_execution_ms,omitempty"`
	// LazyPagesPID is the daemon serving this restore's memory, recorded so
	// the retained image set can be cleaned up when the daemon is gone.
	LazyPagesPID int `json:"lazy_pages_pid,omitempty"`
}

// PrepareOptions carries what a restore caller asks for beyond the checkpoint
// itself. Zero values mean the defaults: the standard timeout, an eager
// restore.
type PrepareOptions struct {
	Timeout time.Duration
	// Lazy starts the restored process before its memory image is fully
	// loaded, serving pages on demand through userfaultfd. It needs a machine
	// and engine that support it, and it is refused — clearly, up front —
	// when either does not.
	Lazy bool
}

type Restorer struct {
	service *Service
	records *securestore.EncryptedCollection[RestoreRecord]
	logger  *slog.Logger
}

func OpenRestorer(service *Service) (*Restorer, error) {
	records, err := securestore.OpenEncryptedCollection[RestoreRecord](filepath.Join(service.stateDir, "metadata", "restores.enc.json"), "restore-transactions-v1", service.repository.keys)
	if err != nil {
		return nil, err
	}
	restorer := &Restorer{service: service, records: records, logger: service.logger}
	if err := restorer.Recover(context.Background()); err != nil {
		return nil, err
	}
	return restorer, nil
}

func (r *Restorer) Prepare(parent context.Context, checkpointID string, options PrepareOptions) (record RestoreRecord, err error) {
	ctx, cancel := commandTimeout(parent, options.Timeout)
	defer cancel()
	// A lazy restore's prerequisites are about the machine, not the
	// checkpoint, so they are checked before anything is staged or extracted:
	// an unsupported kernel must fail in milliseconds, not after minutes of
	// materializing an image set it can never serve lazily.
	var lazyEngine LazyPagesEngine
	if options.Lazy {
		capable, ok := r.service.criu.(LazyPagesEngine)
		if !ok {
			return RestoreRecord{}, errors.New("this checkpoint engine cannot serve lazy restores")
		}
		if checkErr := capable.CheckFeature(ctx, "uffd"); checkErr != nil {
			return RestoreRecord{}, fmt.Errorf("lazy restore is unavailable: userfaultfd is not supported here: %w", checkErr)
		}
		lazyEngine = capable
	}
	// A restore rejected before a transaction record exists (incompatible
	// destination, unreservable ports, missing chunks) still failed; the
	// in-transaction failures are counted by rollback.
	defer func() {
		if err != nil && record.ID == "" && r.service.diagnostics != nil {
			r.service.diagnostics.RestoreFinished(observability.OutcomeFailure)
		}
	}()
	manifest, err := r.service.repository.Load(checkpointID)
	if err != nil {
		return RestoreRecord{}, err
	}
	destination, err := r.service.inventory.Inspect(ctx)
	if err != nil {
		return RestoreRecord{}, err
	}
	report := (compatibility.Checker{}).Check(manifest, destination)
	if !report.Compatible {
		return RestoreRecord{}, compatibilityError(report)
	}
	// The workload's declared host ports are reserved before any restore work
	// begins, so a collision with another workload on this machine fails here
	// instead of halfway through the restore. A workload that declares ports
	// cannot be restored without a network layer behind it: restoring it
	// silently unreachable would be a false success state.
	if len(manifest.Workload.Ports) > 0 {
		if r.service.network == nil {
			return RestoreRecord{}, errors.New("this agent has no network layer configured; a workload with declared ports cannot be restored")
		}
		mappings, mappingsErr := network.MappingsFor(manifest.Workload)
		if mappingsErr != nil {
			return RestoreRecord{}, fmt.Errorf("resolve network mappings: %w", mappingsErr)
		}
		if err := r.service.network.Prepare(manifest.Workload.ID, mappings); err != nil {
			return RestoreRecord{}, fmt.Errorf("reserve host ports: %w", err)
		}
		defer func() {
			if err != nil {
				r.service.network.Deactivate(manifest.Workload.ID, rollbackDrainGrace)
			}
		}()
	}
	for _, asset := range manifest.Assets {
		if err := r.service.chunks.ValidateAsset(ctx, manifest.Workload.ID, asset); err != nil {
			return RestoreRecord{}, fmt.Errorf("validate asset %s: %w", asset.Name, err)
		}
	}
	filesystemAsset, ok := assetByName(manifest.Assets, "filesystem-root")
	if !ok {
		return RestoreRecord{}, errors.New("checkpoint has no filesystem root asset")
	}
	if _, err := processChain(ctx, r.service.repository, r.service.chunks, manifest); err != nil {
		return RestoreRecord{}, err
	}
	sessionID, err := model.NewID()
	if err != nil {
		return RestoreRecord{}, err
	}
	targetRoot := filepath.Clean(manifest.Workload.RootPath)
	if targetRoot == string(filepath.Separator) || !filepath.IsAbs(targetRoot) {
		return RestoreRecord{}, errors.New("unsafe restore target")
	}
	if isMountpoint(targetRoot) {
		return RestoreRecord{}, errors.New("restoring directly over a mountpoint requires a filesystem snapshot adapter")
	}
	parentDirectory := filepath.Dir(targetRoot)
	if err := os.MkdirAll(parentDirectory, 0o755); err != nil {
		return RestoreRecord{}, err
	}
	stagingRoot := filepath.Join(parentDirectory, ".shift-restore-"+sessionID)
	backupRoot := filepath.Join(parentDirectory, ".shift-backup-"+sessionID)
	restoreDirectory := filepath.Join(r.service.stateDir, "restores", sessionID)
	record = RestoreRecord{
		ID: sessionID, CheckpointID: checkpointID, WorkloadID: manifest.Workload.ID,
		State: RestorePreparing, TargetRoot: targetRoot, BackupRoot: backupRoot,
		StagingRoot: stagingRoot, RestoreDirectory: restoreDirectory,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if previous, getErr := r.service.runtime.Get(manifest.Workload.ID); getErr == nil {
		record.PreviousWorkload = &previous
	}
	if err := r.records.Put(record.ID, record); err != nil {
		return RestoreRecord{}, err
	}
	defer func() {
		if err != nil {
			if rollbackErr := r.rollback(ctx, &record, err.Error()); rollbackErr != nil {
				r.logger.Error("restore rollback failed", "restore_id", record.ID, "error", rollbackErr)
			}
		}
	}()
	err = os.MkdirAll(stagingRoot, 0o700)
	if err != nil {
		return record, err
	}
	err = os.MkdirAll(restoreDirectory, 0o700)
	if err != nil {
		return record, err
	}
	err = extractDirectory(ctx, r.service.chunks, manifest.Workload.ID, filesystemAsset, stagingRoot)
	if err != nil {
		return record, err
	}
	restoredRoot, err := validateExtractedRoot(stagingRoot, filepath.Base(targetRoot))
	if err != nil {
		return record, err
	}
	err = materializeProcessChain(ctx, r.service.repository, r.service.chunks, manifest, restoreDirectory)
	if err != nil {
		return record, err
	}
	imagesDirectory := filepath.Join(restoreDirectory, "images")
	if info, statErr := os.Stat(imagesDirectory); statErr != nil || !info.IsDir() {
		return record, errors.New("restored process image directory is missing")
	}
	if targetInfo, lstatErr := os.Lstat(targetRoot); lstatErr == nil {
		if targetInfo.Mode()&os.ModeSymlink != 0 {
			return record, errors.New("refusing to replace a symlinked workload root")
		}
		err = os.Rename(targetRoot, backupRoot)
		if err != nil {
			return record, fmt.Errorf("preserve destination root: %w", err)
		}
	} else if !errors.Is(lstatErr, os.ErrNotExist) {
		return record, lstatErr
	} else {
		record.BackupRoot = ""
	}
	err = materializeRoot(restoredRoot, targetRoot)
	if err != nil {
		if record.BackupRoot != "" {
			_ = os.Rename(record.BackupRoot, targetRoot)
		}
		return record, fmt.Errorf("activate restored filesystem: %w", err)
	}
	record.State = RestoreFilesystemSwitched
	record.UpdatedAt = time.Now().UTC()
	err = syncDir(parentDirectory)
	if err != nil {
		return record, err
	}
	err = r.records.Put(record.ID, record)
	if err != nil {
		return record, err
	}
	_, created, err := r.service.runtime.PrepareRestore(manifest.Workload)
	if err != nil {
		return record, err
	}
	record.CreatedWorkload = created
	err = r.records.Put(record.ID, record)
	if err != nil {
		return record, err
	}
	// The frozen source this checkpoint left as its rollback copy must not
	// share the workload's cgroup with the tree CRIU is about to recreate
	// there: the workload's pids and memory limits would count both trees,
	// and the restore would exhaust the very budget it restores under. Park
	// it in a private cgroup for the duration — a rollback moves it back, a
	// commit reaps it.
	if previous := record.PreviousWorkload; previous != nil && previous.Process != nil &&
		previous.Status == model.WorkloadCheckpointed &&
		linuxplatform.ProcessAlive(previous.Process.PID, previous.Process.ProcStartTicks) {
		err = r.service.runtime.ParkFrozenSource(*previous, record.ID)
		if err != nil {
			return record, fmt.Errorf("park the frozen source for the restore: %w", err)
		}
	}
	// A frozen checkpoint source that declared network ports still holds
	// them, and CRIU must rebind exactly those ports for the restored
	// process — one kernel address cannot be shared by two sockets, so a
	// listening source would block its own restore. Its tree is therefore
	// reaped here rather than at commit: the checkpoint on this machine is
	// the rollback, and a restore that fails past this point leaves the
	// workload checkpointed and restorable again instead of silently dead.
	// A source with no declared ports blocks nothing, so it stays frozen
	// until the restore commits and a rollback can still resume it.
	if len(manifest.Workload.Ports) > 0 {
		r.reapSupersededSource(record)
	}
	restoreStarted := time.Now()
	// A lazy restore's memory server starts before the restore itself: CRIU
	// connects to the daemon's socket and hands page serving to it. The
	// daemon reads pages from the image set — faults on demand, the rest
	// streamed in the background — which is why the restore directory
	// outlives this transaction: see Commit.
	var lazyDaemon LazyPagesProcess
	if options.Lazy {
		workDirectory := filepath.Join(imagesDirectory, "work")
		lazyDaemon, err = lazyEngine.StartLazyPages(ctx, imagesDirectory, workDirectory)
		if err != nil {
			return record, fmt.Errorf("start lazy-pages: %w", err)
		}
		record.Lazy = true
		record.LazyPagesPID = lazyDaemon.PID()
		if err = r.records.Put(record.ID, record); err != nil {
			lazyDaemon.Stop()
			return record, err
		}
		defer func() {
			// Every failure past this point either killed the restored
			// process (the daemon follows it out) or never created one (the
			// daemon would wait forever) — either way it must not linger.
			if err != nil {
				lazyDaemon.Stop()
			}
		}()
	}
	pid, err := r.service.criu.Restore(ctx, RestoreOptions{
		ImagesDirectory: imagesDirectory,
		TCPState:        manifest.Engine.TCPState, ShellJob: manifest.Engine.ShellJob,
		FileLocks: manifest.Engine.FileLocks, ExternalUNIX: true, ManageCgroups: "soft",
		LazyPages: options.Lazy,
	})
	if err != nil {
		return record, err
	}
	if _, err = r.service.runtime.AdoptRestored(manifest.Workload.ID, pid); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return record, err
	}
	if _, err = r.service.runtime.Resume(manifest.Workload.ID); err != nil {
		return record, fmt.Errorf("resume restored process: %w", err)
	}
	// The tree exists and is executing. For a lazy restore it got here
	// without its memory being resident — pages fault in from the daemon from
	// now on — so this moment, not the workload's later readiness, is what
	// "first execution" means and what the number measures.
	record.TimeToFirstExecutionMS = time.Since(restoreStarted).Milliseconds()
	record.PID = pid
	record.State = RestoreProcessRunning
	record.UpdatedAt = time.Now().UTC()
	err = r.records.Put(record.ID, record)
	if err != nil {
		return record, err
	}
	err = r.validateHealth(ctx, manifest.Workload)
	if err != nil {
		return record, err
	}
	// The process is up and healthy: publish its listeners and tell it what
	// happened to its network. Forwarders for non-direct mappings start now
	// — after health, so a forwarder that cannot reach its upstream is a
	// real fault — and the status document written into the workload's root
	// reports the sockets' true disposition: preserved only when the
	// checkpoint actually carried TCP state, recreated otherwise, whatever
	// the workload's policy asked for.
	if r.service.network != nil {
		plan, planErr := network.PlanFor(manifest.Workload)
		if planErr != nil {
			return record, fmt.Errorf("network plan: %w", planErr)
		}
		mappings, mappingsErr := network.MappingsFor(manifest.Workload)
		if mappingsErr != nil {
			return record, fmt.Errorf("resolve network mappings: %w", mappingsErr)
		}
		if err = r.service.network.Activate(ctx, manifest.Workload.ID, mappings); err != nil {
			return record, fmt.Errorf("publish workload ports: %w", err)
		}
		if _, err = r.service.network.RecordStatus(manifest.Workload, plan, network.OperationRestore, record.ID, network.StatusCompleted, "", manifest.Engine.TCPState); err != nil {
			return record, err
		}
	}
	record.State = RestoreValidated
	record.UpdatedAt = time.Now().UTC()
	err = r.records.Put(record.ID, record)
	if err != nil {
		return record, err
	}
	r.logger.Info("checkpoint restored and validated", "restore_id", record.ID, "checkpoint_id", checkpointID, "pid", pid, "duration", time.Since(restoreStarted))
	return record, nil
}

// processChain walks a checkpoint's parent lineage, newest first, validating
// every member's process-state asset on the way. An incremental checkpoint's
// image set only carries its own delta, so the ancestors are not optional:
// restore needs each of their image sets present and intact.
func processChain(ctx context.Context, repository *Repository, chunks *chunkstore.Store, manifest model.CheckpointManifest) ([]model.CheckpointManifest, error) {
	chain := []model.CheckpointManifest{manifest}
	seen := map[string]struct{}{manifest.ID: {}}
	for current := manifest; current.ParentID != ""; {
		if len(chain) >= 256 {
			return nil, errors.New("checkpoint parent chain exceeds 256 entries")
		}
		parent, err := repository.Load(current.ParentID)
		if err != nil {
			return nil, fmt.Errorf("load checkpoint parent %s: %w", current.ParentID, err)
		}
		if parent.Workload.ID != manifest.Workload.ID {
			return nil, fmt.Errorf("checkpoint parent %s belongs to workload %s", parent.ID, parent.Workload.ID)
		}
		if _, exists := seen[parent.ID]; exists {
			return nil, errors.New("checkpoint parent chain contains a cycle")
		}
		seen[parent.ID] = struct{}{}
		chain = append(chain, parent)
		current = parent
	}
	for _, member := range chain {
		final, passes, assetErr := processImageAssets(member)
		if assetErr != nil {
			return nil, assetErr
		}
		for _, asset := range append([]model.AssetManifest{final}, passes...) {
			if err := chunks.ValidateAsset(ctx, manifest.Workload.ID, asset); err != nil {
				return nil, fmt.Errorf("validate process state %s: %w", member.ID, err)
			}
		}
	}
	return chain, nil
}

// materializeProcessChain extracts a checkpoint's process image assets and
// every ancestor's into the nested layout CRIU's parent symlinks describe:
// the checkpoint's own image set at destination/images, its parent's tree at
// destination/parent-images/, the grandparent's at
// destination/parent-images/parent-images/, and so on. A live-migration
// checkpoint carries its pre-copy passes as sibling assets beside the final
// image set — they land next to images/ exactly where the parent symlinks
// point, because CRIU image files collide by name across a chain and each set
// references unchanged pages in its parent, so the sets must sit in sibling
// directories, never overlaid into one where the newest file would silently
// shadow the older file restore still needs.
func materializeProcessChain(ctx context.Context, repository *Repository, chunks *chunkstore.Store, manifest model.CheckpointManifest, destination string) error {
	chain, err := processChain(ctx, repository, chunks, manifest)
	if err != nil {
		return err
	}
	for index, member := range chain {
		final, passes, assetErr := processImageAssets(member)
		if assetErr != nil {
			return assetErr
		}
		target := destination
		for level := 0; level < index; level++ {
			target = filepath.Join(target, "parent-images")
		}
		for _, asset := range append([]model.AssetManifest{final}, passes...) {
			if err := extractDirectory(ctx, chunks, manifest.Workload.ID, asset, target); err != nil {
				return fmt.Errorf("extract process state %s: %w", member.ID, err)
			}
		}
	}
	return nil
}

// processImageAssets returns a checkpoint's process image assets: the final
// image set ("process-state") plus each pre-copy pass's
// ("process-state-precopy-1", ...), ordered by pass. A live-migration
// checkpoint packages every pre-dump pass as its own asset the moment the
// pass completes so the pass can be transferred while the workload still
// runs; the pass numbers must be contiguous from 1, because each pass's
// images parent the previous pass's — a gap is a chain restore can never
// resolve.
func processImageAssets(manifest model.CheckpointManifest) (final model.AssetManifest, passes []model.AssetManifest, err error) {
	final, ok := assetByName(manifest.Assets, "process-state")
	if !ok {
		return model.AssetManifest{}, nil, fmt.Errorf("checkpoint %s has no process-state asset", manifest.ID)
	}
	byIndex := make(map[int]model.AssetManifest)
	for _, asset := range manifest.Assets {
		if asset.Name == "process-state" || !strings.HasPrefix(asset.Name, "process-state-precopy-") {
			continue
		}
		number, parseErr := strconv.Atoi(strings.TrimPrefix(asset.Name, "process-state-precopy-"))
		if parseErr != nil || number < 1 {
			return model.AssetManifest{}, nil, fmt.Errorf("checkpoint %s carries a malformed pre-copy asset %q",
				manifest.ID, asset.Name)
		}
		if _, duplicate := byIndex[number]; duplicate {
			return model.AssetManifest{}, nil, fmt.Errorf("checkpoint %s carries two assets for pre-copy pass %d",
				manifest.ID, number)
		}
		byIndex[number] = asset
	}
	passes = make([]model.AssetManifest, 0, len(byIndex))
	for index := 1; index <= len(byIndex); index++ {
		asset, present := byIndex[index]
		if !present {
			return model.AssetManifest{}, nil, fmt.Errorf("checkpoint %s is missing pre-copy pass %d of %d; the image chain is broken",
				manifest.ID, index, len(byIndex))
		}
		passes = append(passes, asset)
	}
	return final, passes, nil
}

func (r *Restorer) Commit(id string) (RestoreRecord, error) {
	record, err := r.records.Get(id)
	if err != nil {
		return RestoreRecord{}, err
	}
	if record.State == RestoreCommitted {
		return record, nil
	}
	if record.State != RestoreValidated {
		return RestoreRecord{}, fmt.Errorf("restore %s is not validated", id)
	}
	// A restore over a checkpointed workload supersedes the frozen tree that
	// checkpoint left on the machine as its rollback copy. The restored
	// process is now the live one, so the frozen original — kept alive until
	// this moment so a rollback could still resume it — is reaped.
	r.reapSupersededSource(record)
	if record.BackupRoot != "" {
		if err := os.RemoveAll(record.BackupRoot); err != nil {
			return RestoreRecord{}, fmt.Errorf("remove committed filesystem backup: %w", err)
		}
	}
	_ = os.RemoveAll(record.StagingRoot)
	// An eager restore has consumed its image set; the directory is derived
	// state and goes. A lazy restore's daemon is still reading pages from it
	// — faults on demand, the rest streamed in the background — so the
	// directory is retained until the daemon exits (once it has served every
	// image page, or the process is gone) and a watcher removes it then.
	if record.Lazy {
		go r.watchRetainedImages(record)
	} else {
		_ = os.RemoveAll(record.RestoreDirectory)
	}
	record.State = RestoreCommitted
	record.UpdatedAt = time.Now().UTC()
	if err := r.records.Put(id, record); err != nil {
		return RestoreRecord{}, err
	}
	if r.service.diagnostics != nil {
		r.service.diagnostics.RestoreFinished(observability.OutcomeSuccess)
	}
	return record, nil
}

func (r *Restorer) Rollback(ctx context.Context, id, reason string) (RestoreRecord, error) {
	record, err := r.records.Get(id)
	if err != nil {
		return RestoreRecord{}, err
	}
	if record.State == RestoreRolledBack {
		return record, nil
	}
	if record.State == RestoreCommitted {
		return RestoreRecord{}, errors.New("a committed restore cannot be rolled back")
	}
	if err := r.rollback(ctx, &record, reason); err != nil {
		return record, err
	}
	return record, nil
}

func (r *Restorer) Get(id string) (RestoreRecord, error) {
	return r.records.Get(id)
}

func (r *Restorer) List() []RestoreRecord {
	values := r.records.List()
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.After(values[j].CreatedAt) })
	return values
}

func (r *Restorer) Recover(ctx context.Context) error {
	for _, record := range r.records.List() {
		switch record.State {
		case RestorePreparing, RestoreFilesystemSwitched, RestoreProcessRunning, RestoreRollingBack:
			if err := r.rollback(ctx, &record, "agent restarted before restore validation"); err != nil {
				return fmt.Errorf("recover restore %s: %w", record.ID, err)
			}
		case RestoreValidated:
			workload, err := r.service.runtime.Get(record.WorkloadID)
			if err != nil || workload.Process == nil || !linuxplatform.ProcessAlive(workload.Process.PID, workload.Process.ProcStartTicks) {
				if err := r.rollback(ctx, &record, "validated destination process missing after agent restart"); err != nil {
					return err
				}
			}
		}
	}
	// Committed (or validated and left standing) lazy restores retain their
	// image set for the daemon still serving pages from it. After a restart
	// nobody is watching those daemons: re-arm the watch for the living ones
	// and clean up after the dead — a workload that ended while the agent was
	// down leaves its daemon exited and its directory behind.
	for _, record := range r.records.List() {
		if !record.Lazy || (record.State != RestoreCommitted && record.State != RestoreValidated) {
			continue
		}
		if lazyPagesDaemonAlive(record.LazyPagesPID, filepath.Join(record.RestoreDirectory, "images")) {
			go r.watchRetainedImages(record)
			continue
		}
		if err := os.RemoveAll(record.RestoreDirectory); err != nil {
			r.logger.Warn("could not remove a dead lazy restore's retained images",
				"restore_id", record.ID, "path", record.RestoreDirectory, "error", err)
		}
	}
	return nil
}

// watchRetainedImages removes the image set a lazy restore's daemon still
// reads once that daemon is gone. The daemon exits when it has finished
// serving — every image page transferred, or the process gone — so this
// observes, from outside, the moment the image set stops being needed.
// Liveness is proven from /proc against the daemon's own command line, so a
// recycled pid reads as dead and a live daemon is never deleted under.
func (r *Restorer) watchRetainedImages(record RestoreRecord) {
	images := filepath.Join(record.RestoreDirectory, "images")
	for {
		time.Sleep(lazyPagesWatchInterval)
		if lazyPagesDaemonAlive(record.LazyPagesPID, images) {
			continue
		}
		if err := os.RemoveAll(record.RestoreDirectory); err != nil {
			r.logger.Warn("could not remove a lazy restore's retained images",
				"restore_id", record.ID, "path", record.RestoreDirectory, "error", err)
		}
		return
	}
}

// stopLazyPages ends a lazy restore's memory server before a rollback removes
// the image set it reads from. By the time a rollback runs the restored
// process is going away, so the daemon is exiting on its own; this waits out
// that exit and forces it if it stalls. The pid is proven to still be this
// restore's daemon before any signal reaches it.
func (r *Restorer) stopLazyPages(record RestoreRecord) {
	if !record.Lazy || record.LazyPagesPID <= 0 {
		return
	}
	images := filepath.Join(record.RestoreDirectory, "images")
	deadline := time.Now().Add(lazyPagesStopGrace)
	for lazyPagesDaemonAlive(record.LazyPagesPID, images) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if lazyPagesDaemonAlive(record.LazyPagesPID, images) {
		_ = syscall.Kill(record.LazyPagesPID, syscall.SIGKILL)
	}
}

func (r *Restorer) rollback(ctx context.Context, record *RestoreRecord, reason string) error {
	originalState := record.State
	record.State = RestoreRollingBack
	record.Error = reason
	record.UpdatedAt = time.Now().UTC()
	_ = r.records.Put(record.ID, *record)
	// Count one failed restore per rollback; a rollback retried while already
	// rolling back (agent restart racing an explicit rollback) must not count
	// twice.
	if originalState != RestoreRollingBack && r.service.diagnostics != nil {
		r.service.diagnostics.RestoreFinished(observability.OutcomeFailure)
	}
	if workload, err := r.service.runtime.Get(record.WorkloadID); err == nil && workload.Process != nil {
		_, _ = r.service.runtime.Stop(record.WorkloadID, 5*time.Second)
	}
	// Withdraw whatever network presence this restore established: the
	// forwarders stop accepting, in-flight connections get a short grace
	// period to finish, and the host-port reservations go back.
	if r.service.network != nil {
		r.service.network.Deactivate(record.WorkloadID, rollbackDrainGrace)
	}
	filesystemWasSwitched := originalState == RestoreFilesystemSwitched || originalState == RestoreProcessRunning || originalState == RestoreValidated || originalState == RestoreRollingBack
	if filesystemWasSwitched {
		if _, err := os.Lstat(record.TargetRoot); err == nil {
			failedRoot := record.TargetRoot + ".shift-failed-" + record.ID
			if renameErr := os.Rename(record.TargetRoot, failedRoot); renameErr != nil {
				return renameErr
			}
			defer func() {
				if removeErr := os.RemoveAll(failedRoot); removeErr != nil {
					r.logger.Error("remove the failed restore root", "path", failedRoot, "error", removeErr)
				}
			}()
		}
		if record.BackupRoot != "" {
			if _, err := os.Lstat(record.BackupRoot); err == nil {
				if err := os.Rename(record.BackupRoot, record.TargetRoot); err != nil {
					return fmt.Errorf("restore destination backup: %w", err)
				}
			}
		}
	}
	// A frozen source parked aside for this restore comes back before the
	// workload resumes: a running tree always lives in the cgroup its
	// checkpoint recorded. Nothing parked — an early failure, or a restore
	// onto a fresh machine — makes this a no-op.
	if err := r.service.runtime.UnparkFrozenSource(record.WorkloadID, record.ID); err != nil {
		r.logger.Error("reinstate the parked frozen source", "workload_id", record.WorkloadID, "restore_id", record.ID, "error", err)
	}
	switch {
	case record.CreatedWorkload:
		_ = r.service.runtime.Delete(record.WorkloadID)
	case record.PreviousWorkload != nil:
		previous := *record.PreviousWorkload
		if previous.Process != nil && !linuxplatform.ProcessAlive(previous.Process.PID, previous.Process.ProcStartTicks) {
			// The frozen source was reaped because its listening sockets
			// blocked the restore. What survives it is the checkpoint
			// itself — the workload stays checkpointed and restorable — and
			// the record must not claim a process that is no longer on the
			// machine.
			previous.Process = nil
		}
		_ = r.service.runtime.RestoreMetadata(previous)
		if previous.Process != nil {
			// The superseded checkpointed source is still frozen on the
			// machine. With the restored copy gone and its files back in
			// place, the original resumes — the application never
			// disappeared.
			if _, resumeErr := r.service.runtime.Resume(record.WorkloadID); resumeErr != nil {
				r.logger.Error("resume the checkpointed source after restore rollback", "workload_id", record.WorkloadID, "error", resumeErr)
			}
		}
	default:
		_ = r.service.runtime.MarkRestoreFailed(record.WorkloadID, reason)
	}
	_ = os.RemoveAll(record.StagingRoot)
	r.stopLazyPages(*record)
	_ = os.RemoveAll(record.RestoreDirectory)
	record.State = RestoreRolledBack
	record.UpdatedAt = time.Now().UTC()
	if err := r.records.Put(record.ID, *record); err != nil {
		return err
	}
	return syncDir(filepath.Dir(record.TargetRoot))
}

// reapSupersededSource kills the frozen process tree a checkpoint left on the
// machine when the restore that supersedes it commits. It runs only after the
// restored process has been validated as the live copy: until then the frozen
// tree is the rollback path and stays untouched. SIGKILL reaches a stopped
// process directly, the tree's init has no default SIGTERM disposition to
// honor, and the restored copy runs in its own PID namespace and process
// group, so nothing here can reach it.
func (r *Restorer) reapSupersededSource(record RestoreRecord) {
	previous := record.PreviousWorkload
	if previous == nil {
		return
	}
	// The frozen tree also occupies the cgroup this restore parked it in;
	// tear that down whether or not the recorded pid still answers, so even
	// a crash between park and reap cannot leak it.
	if err := r.service.runtime.ReapFrozenSource(record.ID); err != nil {
		r.logger.Warn("reap the parked frozen source cgroup", "workload_id", record.WorkloadID, "restore_id", record.ID, "error", err)
	}
	if previous.Process == nil {
		return
	}
	process := previous.Process
	if !linuxplatform.ProcessAlive(process.PID, process.ProcStartTicks) {
		return
	}
	// The workload is the init of its own PID namespace, so killing that one
	// process tears down the whole tree: the kernel SIGKILLs every remaining
	// process in a namespace whose init died, however deeply a descendant
	// detached from the process group. The group signal covers a tree that
	// somehow runs without a namespace.
	// The exit this kill produces is a retirement the agent ordered, not a
	// failure: mark it before the signal so the pending waiter records an
	// ordinary stop.
	r.service.runtime.Retire(record.WorkloadID)
	pidErr := syscall.Kill(process.PID, syscall.SIGKILL)
	groupErr := syscall.Kill(-process.PGID, syscall.SIGKILL)
	if errors.Is(pidErr, syscall.ESRCH) {
		pidErr = nil
	}
	if errors.Is(groupErr, syscall.ESRCH) {
		groupErr = nil
	}
	if pidErr != nil && groupErr != nil {
		r.logger.Warn("kill the superseded checkpointed source", "workload_id", record.WorkloadID, "pid", process.PID, "error", errors.Join(pidErr, groupErr))
		return
	}
	r.logger.Info("reaped the superseded checkpointed source", "workload_id", record.WorkloadID, "pid", process.PID)
}

func assetByName(assets []model.AssetManifest, name string) (model.AssetManifest, bool) {
	for _, asset := range assets {
		if asset.Name == name {
			return asset, true
		}
	}
	return model.AssetManifest{}, false
}

func compatibilityError(report model.CompatibilityReport) error {
	var reasons []string
	for _, issue := range report.Issues {
		if issue.Severity == "error" {
			reasons = append(reasons, issue.Code+": "+issue.Description)
		}
	}
	return errors.New("destination is incompatible: " + strings.Join(reasons, "; "))
}

func (r *Restorer) validateHealth(ctx context.Context, workload model.WorkloadSpec) error {
	check := workload.HealthCheck
	if check == nil {
		timer := time.NewTimer(500 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			restored, err := r.service.runtime.Get(workload.ID)
			if err != nil || restored.Process == nil || !linuxplatform.ProcessAlive(restored.Process.PID, restored.Process.ProcStartTicks) {
				return errors.New("restored process exited during validation")
			}
			return nil
		}
	}
	if check.Retries < 1 {
		check.Retries = 3
	}
	if check.Timeout <= 0 {
		check.Timeout = 5 * time.Second
	}
	if check.Interval <= 0 {
		check.Interval = time.Second
	}
	if check.StartDelay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(check.StartDelay):
		}
	}
	var lastErr error
	for attempt := 0; attempt < check.Retries; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, check.Timeout)
		lastErr = runHealthCheck(attemptCtx, workload, *check)
		cancel()
		if lastErr == nil {
			return nil
		}
		if attempt+1 < check.Retries {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(check.Interval):
			}
		}
	}
	return fmt.Errorf("workload health validation failed: %w", lastErr)
}

func runHealthCheck(ctx context.Context, workload model.WorkloadSpec, check model.HealthCheckSpec) error {
	switch check.Type {
	case "tcp":
		if err := requireLoopbackAddress(check.Address); err != nil {
			return err
		}
		dialer := net.Dialer{}
		connection, err := dialer.DialContext(ctx, "tcp", check.Address)
		if err != nil {
			return err
		}
		return connection.Close()
	case "http":
		parsed, err := url.Parse(check.Address)
		if err != nil {
			return err
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return errors.New("health URL must use http or https")
		}
		if err := requireLoopbackHost(parsed.Hostname()); err != nil {
			return err
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), http.NoBody)
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer func() { _ = response.Body.Close() }()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode < 200 || response.StatusCode >= 400 {
			return fmt.Errorf("health endpoint returned %s", response.Status)
		}
		return nil
	case "command":
		if len(check.Command) == 0 {
			return errors.New("health command is empty")
		}
		command := exec.CommandContext(ctx, check.Command[0], check.Command[1:]...)
		command.Dir = workload.WorkingDir
		command.Env = environmentForHealth(workload.Environment)
		command.SysProcAttr = &syscall.SysProcAttr{}
		if os.Geteuid() == 0 {
			command.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(workload.UID), Gid: uint32(workload.GID)}
		}
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("health command: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return nil
	default:
		return fmt.Errorf("unsupported health check type %q", check.Type)
	}
}

func requireLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	return requireLoopbackHost(host)
}

func requireLoopbackHost(host string) error {
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("health checks are restricted to loopback addresses")
	}
	return nil
}

func environmentForHealth(overrides map[string]string) []string {
	values := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	for key, value := range overrides {
		values = append(values, key+"="+value)
	}
	return values
}

func isMountpoint(path string) bool {
	content, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	clean := filepath.Clean(path)
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 4 && filepath.Clean(strings.ReplaceAll(fields[4], "\\040", " ")) == clean {
			return true
		}
	}
	return false
}

func syncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
