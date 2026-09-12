package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"shift.dev/shift/internal/filesystem"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/observability"
	linuxplatform "shift.dev/shift/internal/platform/linux"
)

// LiveOptions configures an iterative pre-copy checkpoint: pre-dump passes
// that run while the workload keeps executing, each one packaged the moment
// it completes so a live migration can transfer it immediately, and a short
// frozen window at the end that carries only the final dump.
type LiveOptions struct {
	// Passes caps the pre-dump iterations. Each pass parents the previous
	// one, so memory that stops changing is carried while the workload keeps
	// running and only the remaining delta is written under the freeze.
	// Clamped to 1..16; zero means a single pass.
	Passes int
	// Convergence ends the pass loop early once a pass's image bytes fall to
	// this fraction of the first pass's: past that point the workload is no
	// longer dirtying memory faster than pre-dump drains it, and freezing now
	// is cheaper than another pass. Zero means the default.
	Convergence float64
	// LeaveRunning resumes the workload after the checkpoint completes; false
	// leaves it frozen on the machine as the migration's rollback copy.
	LeaveRunning bool
	TCPState     bool
	Timeout      time.Duration
}

// PreCopyPass reports one completed pre-copy pass. Asset is the pass's
// packaged image set, and its chunk refs are what a live migration transfers
// to the destination right away — while the workload still runs — because an
// asset's archive starts at its own first byte, so its content chunks to the
// same addresses no matter when the destination receives it.
type PreCopyPass struct {
	Index      int
	DeltaBytes int64
	Asset      model.AssetManifest
	// KeyVersion is the workload key version every chunk of the pass asset is
	// encrypted under — the version the destination must import before it can
	// store any of the pass's chunks.
	KeyVersion uint32
	// More reports whether another pass is warranted: the cap is not reached
	// and the deltas have not converged.
	More bool
}

// LiveSession drives one iterative pre-copy checkpoint across calls, so a
// live migration can interleave: run a pass, transfer its images, run the
// next, transfer it, and only then freeze for the final dump. BeginLive
// takes the same single-checkpoint guard Create takes; the guard is held
// until Finalize or Abort closes the session.
type LiveSession struct {
	service           *Service
	workload          model.Workload
	options           LiveOptions
	checkpointID      string
	staging           string
	imagesDirectory   string
	passImages        []string
	passAssets        []model.AssetManifest
	baseline          int64
	deduplicated      int64
	passLimit         int
	convergence       float64
	startedAt         time.Time
	criuVersion       string
	keyVersion        uint32
	originallyRunning bool
	frozen            bool
	freezeStartedAt   time.Time
	closed            bool
}

// BeginLive opens an iterative pre-copy session on a running (or paused)
// workload. The session holds the workload's single-checkpoint guard until
// Finalize or Abort closes it, so no other checkpoint of the workload can
// interleave with the passes.
func (s *Service) BeginLive(parent context.Context, workloadID string, options LiveOptions) (*LiveSession, error) {
	s.createInFlightM.Lock()
	if _, busy := s.createInFlight[workloadID]; busy {
		s.createInFlightM.Unlock()
		return nil, fmt.Errorf("a checkpoint of workload %s is already in progress", workloadID)
	}
	s.createInFlight[workloadID] = struct{}{}
	s.createInFlightM.Unlock()
	release := func() {
		s.createInFlightM.Lock()
		delete(s.createInFlight, workloadID)
		s.createInFlightM.Unlock()
	}
	workload, err := s.runtime.Get(workloadID)
	if err != nil {
		release()
		return nil, err
	}
	if workload.Process == nil || !linuxplatform.ProcessAlive(workload.Process.PID, workload.Process.ProcStartTicks) {
		release()
		return nil, errors.New("workload must be running or paused to checkpoint")
	}
	ctx, cancel := commandTimeout(parent, options.Timeout)
	checkErr := s.criu.Check(ctx)
	var version string
	if checkErr == nil {
		version, checkErr = s.criu.Version(ctx)
	}
	cancel()
	if checkErr != nil {
		release()
		return nil, checkErr
	}
	checkpointID, err := model.NewID()
	if err != nil {
		release()
		return nil, err
	}
	staging := filepath.Join(s.stateDir, "staging", "checkpoint-"+checkpointID)
	imagesDirectory := filepath.Join(staging, "images")
	if err := os.MkdirAll(imagesDirectory, 0o700); err != nil {
		release()
		return nil, err
	}
	// The key version is pinned now, before any pass packages a chunk: the
	// destination imports this version before the first transfer, and every
	// packaged chunk is verified against it, so a key rotated mid-session can
	// only fail the session cleanly — never strand passes the destination
	// cannot decrypt.
	keyVersion, err := s.chunks.ActiveKeyVersion(workloadID)
	if err != nil {
		_ = os.RemoveAll(staging)
		release()
		return nil, err
	}
	passLimit := options.Passes
	if passLimit < 1 {
		passLimit = 1
	}
	if passLimit > 16 {
		passLimit = 16
	}
	convergence := options.Convergence
	if convergence <= 0 || convergence >= 1 {
		convergence = 0.25
	}
	return &LiveSession{
		service: s, workload: workload, options: options,
		checkpointID: checkpointID, staging: staging, imagesDirectory: imagesDirectory,
		passLimit: passLimit, convergence: convergence,
		startedAt: time.Now().UTC(), criuVersion: version, keyVersion: keyVersion,
		originallyRunning: workload.Status == model.WorkloadRunning,
	}, nil
}

// PassLimit reports the clamped pass cap — the basis for a live migration's
// transfer reservation.
func (l *LiveSession) PassLimit() int { return l.passLimit }

// KeyVersion reports the workload key version the session pinned at BeginLive.
func (l *LiveSession) KeyVersion() uint32 { return l.keyVersion }

// CanPass reports whether pre-copy passes are possible: only a workload that
// was running when the session began has anything to pre-copy. A paused
// workload goes straight to Finalize — its final dump parents nothing.
func (l *LiveSession) CanPass() bool { return l.originallyRunning && !l.closed }

// FreezeStartedAt reports the instant Finalize froze the workload — zero when
// it never did. It stays readable after the session closes, so a caller whose
// Finalize failed mid-window can still measure the downtime its freeze
// actually caused instead of charging the whole session — whose passes ran
// while the workload was live — to the freeze.
func (l *LiveSession) FreezeStartedAt() time.Time { return l.freezeStartedAt }

// Pass runs one pre-dump pass while the workload keeps executing, then
// packages that pass's image set as its own asset — a live migration
// transfers the asset's chunks immediately, before the next pass runs, which
// is what keeps the later frozen window down to the final delta.
func (l *LiveSession) Pass(parent context.Context) (PreCopyPass, error) {
	if l.closed {
		return PreCopyPass{}, errors.New("live checkpoint session is closed")
	}
	if !l.originallyRunning {
		return PreCopyPass{}, errors.New("pre-copy passes require a workload that was running when the session began")
	}
	if len(l.passImages) >= l.passLimit {
		return PreCopyPass{}, errors.New("pre-copy pass cap reached")
	}
	ctx, cancel := commandTimeout(parent, l.options.Timeout)
	defer cancel()
	pass := len(l.passImages) + 1
	images := filepath.Join(l.staging, fmt.Sprintf("precopy-%d", pass))
	if err := os.MkdirAll(images, 0o700); err != nil {
		return PreCopyPass{}, err
	}
	previous := ""
	if pass > 1 {
		previous = l.passImages[pass-2]
	}
	if err := l.service.criu.PreDump(ctx, DumpOptions{
		PID: l.workload.Process.PID, ImagesDirectory: images, ParentImages: previous,
		TCPState: l.options.TCPState, ShellJob: true, FileLocks: true,
		ExternalUNIX: true, ManageCgroups: "soft",
	}); err != nil {
		return PreCopyPass{}, fmt.Errorf("criu pre-dump pass %d: %w", pass, err)
	}
	result, err := captureDirectory(ctx, l.service.chunks, l.workload.Spec.ID,
		fmt.Sprintf("process-state-precopy-%d", pass), l.staging,
		[]string{filepath.Base(images)}, []string{"work"}, 20)
	if err != nil {
		return PreCopyPass{}, err
	}
	if err := l.requireKeyVersion(result.Asset); err != nil {
		return PreCopyPass{}, err
	}
	l.passImages = append(l.passImages, images)
	l.passAssets = append(l.passAssets, result.Asset)
	l.deduplicated += result.DeduplicatedBytes
	delta := directoryBytes(images)
	more := len(l.passImages) < l.passLimit
	if pass == 1 {
		l.baseline = delta
	} else if l.baseline > 0 && delta <= int64(l.convergence*float64(l.baseline)) {
		l.service.logger.Debug("pre-copy converged before the pass cap",
			"workload_id", l.workload.Spec.ID, "pass", pass,
			"delta_bytes", delta, "baseline_bytes", l.baseline)
		more = false
	}
	return PreCopyPass{
		Index: pass, DeltaBytes: delta, Asset: result.Asset,
		KeyVersion: l.keyVersion, More: more,
	}, nil
}

// requireKeyVersion rejects a packaged asset whose chunks are encrypted under
// a workload key version other than the one the session pinned: the
// destination imported the pinned version, so a rotated key would strand
// passes it cannot decrypt.
func (l *LiveSession) requireKeyVersion(asset model.AssetManifest) error {
	for _, ref := range asset.Chunks {
		if ref.KeyVersion != l.keyVersion {
			return fmt.Errorf("workload key rotated during the live checkpoint (asset %s uses version %d, session pinned %d)",
				asset.Name, ref.KeyVersion, l.keyVersion)
		}
	}
	return nil
}

// Finalize ends the pass loop and performs the short frozen window: freeze,
// final dump parented on the last pass, filesystem capture, packaging of the
// final image set, and the manifest — which lists every pass's asset beside
// the final one, so the destination learns the whole image chain at once.
// The manifest's own asset carries only the final image set; each pass's set
// travels as the asset Pass already packaged, which is what lets the passes
// be transferred before the freeze. On success — or failure — the session is
// closed; a failure thaws a workload this Finalize froze.
func (l *LiveSession) Finalize(parent context.Context) (manifest model.CheckpointManifest, err error) {
	if l.closed {
		return model.CheckpointManifest{}, errors.New("live checkpoint session is closed")
	}
	l.closed = true
	defer func() {
		if l.service.diagnostics != nil {
			if err != nil {
				l.service.diagnostics.CheckpointFinished(observability.OutcomeFailure)
			} else {
				l.service.diagnostics.CheckpointFinished(observability.OutcomeSuccess)
			}
		}
		if err != nil && l.frozen && l.originallyRunning {
			if _, resumeErr := l.service.runtime.Resume(l.workload.Spec.ID); resumeErr != nil {
				l.service.logger.Error("resume workload after failed live checkpoint",
					"workload_id", l.workload.Spec.ID, "error", resumeErr)
			}
		}
		l.close()
	}()
	ctx, cancel := commandTimeout(parent, l.options.Timeout)
	defer cancel()
	freezeStartedAt := time.Time{}
	if l.originallyRunning {
		if _, err = l.service.runtime.Pause(l.workload.Spec.ID); err != nil {
			return model.CheckpointManifest{}, fmt.Errorf("freeze workload: %w", err)
		}
		l.frozen = true
		// The workload provably stopped executing at this instant; the
		// timestamp travels in the manifest so downtime is measured from the
		// real freeze, never from the session's start — which includes
		// pre-copy passes that ran while the workload was still running. It is
		// also kept on the session so a Finalize that fails after this point
		// can still be measured honestly.
		freezeStartedAt = time.Now().UTC()
		l.freezeStartedAt = freezeStartedAt
	}
	parentImages := ""
	if len(l.passImages) > 0 {
		parentImages = l.passImages[len(l.passImages)-1]
	}
	err = l.service.criu.Dump(ctx, DumpOptions{
		PID: l.workload.Process.PID, ImagesDirectory: l.imagesDirectory, ParentImages: parentImages,
		TCPState: l.options.TCPState, ShellJob: true, FileLocks: true, ExternalUNIX: true,
		LeaveStopped: true, ManageCgroups: "soft",
		// A checkpoint that leaves the workload running can become the parent
		// of a later incremental checkpoint, so its dump must arm the memory
		// tracker — CRIU refuses to diff against an untracked parent.
		TrackMemory: l.options.LeaveRunning,
	})
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	// The final image set references unchanged pages in its parent through the
	// parent symlink CRIU writes beside it, so the pass image sets must be
	// restorable beside it: each travels as its own packaged asset, and the
	// sets can never be overlaid into one directory — CRIU image files collide
	// by name across a chain, and the colliding older file is exactly the one
	// restore still needs.
	// The filesystem asset must describe the workload root exactly as it was
	// while frozen. Where the root sits on a snapshot-capable filesystem,
	// SHIFT takes an atomic copy-on-write snapshot and reads the archive from
	// it; everywhere else the archive is read straight from the frozen root.
	// The manifest records which happened — a torn or "probably consistent"
	// capture is never implied.
	filesystemInfo, err := filesystem.Probe(l.workload.Spec.RootPath)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	capture := model.FilesystemCapture{Filesystem: filesystemInfo.Name}
	captureRoot := l.workload.Spec.RootPath
	var pendingSnapshot *filesystem.Snapshot
	if provider := filesystem.DetectSnapshotProvider(filesystemInfo); provider == nil {
		capture.FrozenCapture = true
	} else if snapshot, snapshotErr := provider.Create(ctx, l.workload.Spec.RootPath, l.checkpointID); snapshotErr == nil {
		pendingSnapshot = &snapshot
		capture.Snapshot = provider.Name()
		captureRoot = snapshot.Path()
	} else {
		capture.FrozenCapture = true
		l.service.logger.Warn("filesystem snapshot unavailable; capturing from the frozen root",
			"workload_id", l.workload.Spec.ID, "error", snapshotErr)
	}
	if pendingSnapshot != nil {
		defer func() {
			if releaseErr := pendingSnapshot.Release(); releaseErr != nil {
				l.service.logger.Error("release filesystem snapshot", "workload_id", l.workload.Spec.ID, "error", releaseErr)
			}
		}()
	}
	// Changed-file accounting runs against the same view the archive reads,
	// so the counts and the tarball always describe one instant.
	rootExclusions := exclusionsForRoot(l.workload.Spec)
	indexPath := filepath.Join(l.service.stateDir, "indices", l.workload.Spec.ID+".json")
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
	if pendingSnapshot != nil && l.originallyRunning && l.options.LeaveRunning {
		if _, err = l.service.runtime.Resume(l.workload.Spec.ID); err != nil {
			return model.CheckpointManifest{}, fmt.Errorf("resume workload after snapshot: %w", err)
		}
		l.frozen = false
	}
	imageResult, err := captureDirectory(ctx, l.service.chunks, l.workload.Spec.ID,
		"process-state", l.staging, []string{filepath.Base(l.imagesDirectory)}, []string{"work"}, 20)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	err = l.requireKeyVersion(imageResult.Asset)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	filesystemResult, err := captureDirectory(ctx, l.service.chunks, l.workload.Spec.ID,
		"filesystem-root", filepath.Dir(captureRoot), []string{filepath.Base(captureRoot)}, rootExclusions, 10)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	err = l.requireKeyVersion(filesystemResult.Asset)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	// Dependency discovery reports the files outside the process image that
	// the command needs to run. Files inside the root travel with the
	// checkpoint; files outside it do not, and the manifest says which is
	// which so a destination cannot be surprised by a missing library.
	environment := make([]string, 0, len(l.workload.Spec.Environment))
	for key, value := range l.workload.Spec.Environment {
		environment = append(environment, key+"="+value)
	}
	discovery, discoveryErr := filesystem.DiscoverDependencies(l.workload.Spec.Command, l.workload.Spec.RootPath, environment)
	if discoveryErr != nil {
		return model.CheckpointManifest{}, fmt.Errorf("discover workload dependencies: %w", discoveryErr)
	}
	if len(discovery.Unresolved) > 0 {
		l.service.logger.Warn("some workload dependencies could not be located on this machine",
			"workload_id", l.workload.Spec.ID, "unresolved", strings.Join(discovery.Unresolved, ", "))
	}
	machine, err := l.service.inventory.Inspect(ctx)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	assets := []model.AssetManifest{filesystemResult.Asset, imageResult.Asset}
	assets = append(assets, l.passAssets...)
	metrics := model.CheckpointMetrics{StartedAt: l.startedAt, FreezeStartedAt: freezeStartedAt}
	for _, asset := range assets {
		metrics.PlainBytes += asset.PlainSize
		metrics.StoredBytes += asset.StoredSize
		metrics.ChunkCount += len(asset.Chunks)
	}
	metrics.DeduplicatedBytes = filesystemResult.DeduplicatedBytes + imageResult.DeduplicatedBytes + l.deduplicated
	metrics.CompletedAt = time.Now().UTC()
	metrics.Duration = metrics.CompletedAt.Sub(l.startedAt)
	manifest = model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		ID: l.checkpointID, Kind: model.CheckpointFull,
		Workload: l.workload.Spec, SourceIdentity: l.service.identity.Machine, SourceMachine: machine,
		CreatedAt: l.startedAt, Assets: assets, RequiredBytes: metrics.PlainBytes, Metrics: metrics,
		Engine: model.CheckpointEngineInfo{
			Name: "CRIU", Version: l.criuVersion, LeaveRunning: l.options.LeaveRunning,
			ParentImages: parentImages != "", PreCopy: len(l.passImages) > 0, PreCopyPasses: len(l.passImages),
			TCPState: l.options.TCPState, ShellJob: true, FileLocks: true,
		},
		StateInventory: stateInventory(l.options.TCPState), DeviceNeeds: l.workload.Spec.Resources.GPUs,
		Filesystem:      capture,
		Dependencies:    discovery.Dependencies,
		CompatibilityID: compatibilityID(machine, l.workload.Spec),
	}
	manifest.Security.KeyVersion = l.keyVersion
	err = SignManifest(&manifest, l.service.identity)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	err = l.service.repository.Save(manifest)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	// The checkpoint exists now, so this instant's file index becomes the
	// baseline the next checkpoint's changed-file accounting diffs against.
	err = os.MkdirAll(filepath.Join(l.service.stateDir, "indices"), 0o700)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	err = filesystem.SaveIndex(indexPath, currentIndex)
	if err != nil {
		return model.CheckpointManifest{}, fmt.Errorf("persist changed-file index: %w", err)
	}
	if l.service.mirror != nil {
		if _, mirrorErr := l.service.MirrorCheckpoint(ctx, l.checkpointID); mirrorErr != nil {
			return model.CheckpointManifest{}, fmt.Errorf("checkpoint %s persisted locally but object-store mirror failed: %w", l.checkpointID, mirrorErr)
		}
	}
	if l.originallyRunning && l.options.LeaveRunning && l.frozen {
		if _, err = l.service.runtime.Resume(l.workload.Spec.ID); err != nil {
			return model.CheckpointManifest{}, fmt.Errorf("checkpoint stored but source could not resume: %w", err)
		}
		l.frozen = false
	}
	if _, err = l.service.runtime.MarkCheckpoint(l.workload.Spec.ID, l.checkpointID, l.originallyRunning && l.options.LeaveRunning); err != nil {
		return model.CheckpointManifest{}, err
	}
	l.service.logger.Info("live checkpoint created", "checkpoint_id", l.checkpointID, "workload_id", l.workload.Spec.ID,
		"passes", len(l.passImages), "plain_bytes", metrics.PlainBytes, "stored_bytes", metrics.StoredBytes)
	return manifest, nil
}

// Abort abandons the session without a final checkpoint: a live migration
// whose transfer failed mid-pass calls this instead of Finalize. The attempt
// counts as a failed checkpoint — it began work — the workload is thawed if
// the session froze it, and the staging tree is removed. Abort after a
// Finalize, successful or failed, is a no-op; those paths closed the session.
func (l *LiveSession) Abort() {
	if l.closed {
		return
	}
	l.closed = true
	if l.service.diagnostics != nil {
		l.service.diagnostics.CheckpointFinished(observability.OutcomeFailure)
	}
	if l.frozen && l.originallyRunning {
		if _, err := l.service.runtime.Resume(l.workload.Spec.ID); err != nil {
			l.service.logger.Error("resume workload after aborted live checkpoint",
				"workload_id", l.workload.Spec.ID, "error", err)
		}
	}
	l.close()
}

// close releases the session's resources: the staging tree and the
// single-checkpoint guard both belong to the session's lifetime.
func (l *LiveSession) close() {
	_ = os.RemoveAll(l.staging)
	l.service.createInFlightM.Lock()
	delete(l.service.createInFlight, l.workload.Spec.ID)
	l.service.createInFlightM.Unlock()
}

// EstimateLiveTransferBytes bounds the ciphertext a live migration of this
// workload can upload across its whole session, measured once the first
// pre-copy pass has been packaged: the root tree's tar archive at worst-case
// framing, plus every image set — the remaining passes and the final dump —
// at the first pass's measured stored size. Each later image set carries only
// pages dirtied since the previous one, which for a fixed-memory workload
// never exceeds the first pass's full dirty set, so passLimit+1 sets bound
// the loop (pass 1 is the measurement itself). A workload that grows its
// memory mid-flight can exceed the bound; the destination then rejects the
// excess with TRANSFER_QUOTA_EXCEEDED and the migration rolls back to the
// preserved source — a safe failure, never a silent overflow. A root-walk
// error returns the partial sum with the error, so the caller can warn and
// proceed: the checkpoint itself fails on the same unreadable tree, so the
// reservation never outlives the capture it described.
func EstimateLiveTransferBytes(spec model.WorkloadSpec, firstPassStored int64, passLimit int) (int64, error) {
	if passLimit < 1 {
		passLimit = 1
	}
	root, walkErr := treeBound(spec.RootPath, exclusionsForRoot(spec))
	return root + firstPassStored*int64(passLimit+1), walkErr
}

// treeBound sums the bytes the root's tar archive can occupy: each entry's
// data rounded up to tar's 512-byte blocks, plus a per-entry allowance
// covering the header and the pax records --format=pax writes for xattrs and
// ACLs, with long names charged separately. It deliberately overestimates
// where exactness would be fragile — sparse files, hard links, and files tar
// skips via glob exclusions are all charged in full — because the bound's
// failure mode is asymmetric: too loose costs a little destination headroom,
// too tight fails an honest transfer.
func treeBound(root string, exclusions []string) (int64, error) {
	var total int64
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if relative != "." && excludedMember(relative, exclusions) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		allowance := int64(2048) // header + block padding + xattr/ACL pax records
		if name := entry.Name(); len(name) > 100 {
			allowance += int64((len(name)+1023)/512*512 + 512) // pax long-name record
		}
		if info, infoErr := entry.Info(); infoErr == nil && info.Mode().IsRegular() {
			total += allowance + (info.Size()+511)/512*512
		} else {
			total += allowance
		}
		return nil
	})
	return total + 4096, walkErr // the archive trailer and the base directory's own header
}

// excludedMember reports whether a base-relative path is covered by a plain
// exclusion pattern — an exact match or a member of an excluded directory,
// mirroring how tar skips an excluded directory's whole subtree. Patterns with
// glob metacharacters never match here: those files stay in the bound, which
// can only loosen it, never undercount what tar will actually archive.
func excludedMember(relative string, exclusions []string) bool {
	for _, pattern := range exclusions {
		if strings.ContainsAny(pattern, "*?[") {
			continue
		}
		if relative == pattern || strings.HasPrefix(relative, pattern+"/") {
			return true
		}
	}
	return false
}
