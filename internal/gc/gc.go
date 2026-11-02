package gc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/model"
)

var (
	ErrLiveSweepDisabled              = errors.New("online checkpoint garbage collection is disabled; stop the agent and use offline maintenance")
	ErrSharedCloudPrefixUncoordinated = errors.New("cloud garbage collection is disabled for shared prefixes without a distributed ownership lock")
	ErrPlanNotFound                   = errors.New("garbage collection plan not found")
)

// minGrace is the hard floor under the caller's grace period and the minimum
// age a chunk file must reach before any sweep may delete it. A checkpoint
// writes its chunks long before its manifest is installed, so a chunk that is
// younger than this window is never provably orphaned — it may belong to a
// capture still in flight, a mirror import still landing, or a transfer still
// uploading. Callers may request a shorter grace (or none); the floor stands
// regardless, so an operator-supplied grace_seconds=1 on the agent API cannot
// open the mid-write deletion window.
const minGrace = time.Hour

// maxEncryptedManifestBytes mirrors the repository's manifest size limit used
// to bound the on-disk manifest scan.
const maxEncryptedManifestBytes = int64(8 << 20)

type Candidate struct {
	Ref         model.ChunkRef `json:"ref"`
	Bytes       int64          `json:"bytes"`
	Modified    time.Time      `json:"modified"`
	FirstSeenAt time.Time      `json:"first_seen_at"`
	EligibleAt  time.Time      `json:"eligible_at"`
	Status      string         `json:"status"`
	Error       string         `json:"error,omitempty"`
}

type Plan struct {
	ID              string               `json:"id"`
	GeneratedAt     time.Time            `json:"generated_at"`
	GracePeriod     time.Duration        `json:"grace_period"`
	Referenced      int                  `json:"referenced"`
	CheckpointCount int                  `json:"checkpoint_count"`
	Candidates      []Candidate          `json:"candidates"`
	Cursor          int                  `json:"cursor"`
	Status          string               `json:"status"`
	DryRun          bool                 `json:"dry_run"`
	UpdatedAt       time.Time            `json:"updated_at"`
	Orphans         map[string]time.Time `json:"orphans,omitempty"`
	// UnindexedManifests names checkpoints that exist on disk as
	// manifests but are missing from the repository index (a crash
	// between the manifest write and the index write, or an interrupted
	// delete). Their chunks are treated as referenced so a maintenance
	// run can never destroy the recovery bytes of a half-saved
	// checkpoint; the list is surfaced so the operator can repair the
	// index.
	UnindexedManifests []string `json:"unindexed_manifests,omitempty"`
}

type StateLock struct {
	file *os.File
	once sync.Once
}

// AcquireStateLock takes the same exclusive lock the agent must hold for its
// lifecycle. Maintenance therefore cannot race checkpoint creation, restore,
// transfer, or repository index writes. It fails fast if the agent is running.
func AcquireStateLock(stateDir string) (*StateLock, error) { return acquireStateLock(stateDir) }
func (l *StateLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	var err error
	l.once.Do(func() { err = l.file.Close() })
	return err
}

// WriteGuard is the in-process exclusion the owning agent holds around
// checkpoint, repository, and transfer writes. The offline collector relies
// on the cross-process state lock instead; an online collector — running
// inside the agent that is still serving — takes this guard around every
// reference scan and every delete, so a sweep cannot delete a chunk between
// the moment a writer resolves it and the moment it installs a reference.
//
// The exclusion is only as complete as writer participation: every writer
// that resolves chunk content and installs a manifest reference must hold
// the guard across the whole window, including writers that run outside an
// HTTP request (scheduled policy checkpoints, migration stages, mirror
// imports, standby replication). The collector does not depend solely on
// that cooperation — deletion additionally requires the two-phase grace
// (see Plan) and the minGrace chunk-age floor — but a capture expected to
// outlast minGrace is only safe while its writers hold the guard.
type WriteGuard interface {
	Lock()
	Unlock()
}

type Collector struct {
	stateDir string
	lock     *StateLock
	guard    WriteGuard
	repo     *checkpoint.Repository
	chunks   *chunkstore.Store
}

// stateDirMutexes serializes collectors that share one state directory: two
// concurrent runs (e.g. overlapping POST /v1/gc calls) would otherwise
// interleave their load-modify-write cycles over gc/orphans.json and the plan
// files and silently clobber each other's first-seen observations.
var (
	stateDirMutexesMu sync.Mutex
	stateDirMutexes   = map[string]*sync.Mutex{}
)

func lockStateDir(stateDir string) func() {
	key := filepath.Clean(stateDir)
	stateDirMutexesMu.Lock()
	mutex, ok := stateDirMutexes[key]
	if !ok {
		mutex = &sync.Mutex{}
		stateDirMutexes[key] = mutex
	}
	stateDirMutexesMu.Unlock()
	mutex.Lock()
	return mutex.Unlock
}

func OpenOffline(stateDir string, repo *checkpoint.Repository, chunks *chunkstore.Store) (*Collector, error) {
	if stateDir == "" || repo == nil || chunks == nil {
		return nil, errors.New("state directory, repository, and chunk store are required")
	}
	lock, err := AcquireStateLock(stateDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "gc"), 0o700); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &Collector{stateDir: stateDir, lock: lock, repo: repo, chunks: chunks}, nil
}

// OpenOnline builds a collector that runs while the agent is still up: it
// deliberately does not take the cross-process state lock (the running agent
// holds it) and instead serializes against the agent's own writers through
// guard. The two-phase grace period from Plan is unchanged — a chunk must be
// observed unreferenced across two passes before it becomes eligible — and
// each candidate is re-validated under the guard immediately before its
// delete, so a host that has filled its disk can reclaim space in place
// instead of only through stopped-agent maintenance.
func OpenOnline(stateDir string, repo *checkpoint.Repository, chunks *chunkstore.Store, guard WriteGuard) (*Collector, error) {
	if stateDir == "" || repo == nil || chunks == nil || guard == nil {
		return nil, errors.New("state directory, repository, chunk store, and write guard are required")
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "gc"), 0o700); err != nil {
		return nil, err
	}
	return &Collector{stateDir: stateDir, guard: guard, repo: repo, chunks: chunks}, nil
}
func (c *Collector) Close() error {
	if c == nil {
		return nil
	}
	return c.lock.Close()
}

// lockGuard/unlockGuard take the online writer guard when one is attached;
// offline collectors own the whole state directory via the state lock and
// need no in-process exclusion.
func (c *Collector) lockGuard() {
	if c.guard != nil {
		c.guard.Lock()
	}
}

func (c *Collector) unlockGuard() {
	if c.guard != nil {
		c.guard.Unlock()
	}
}

// Plan records the first observation of every orphan and only makes it
// deletable after grace has elapsed across a persisted observation. A newly
// orphaned object therefore cannot be deleted by a single maintenance run.
//
// The effective grace is never below minGrace: the two-phase bookkeeping is
// the only thing standing between a chunk written by an in-flight checkpoint
// and its deletion, so a caller-supplied grace below the mid-write window is
// clamped rather than honored. Plan reports the effective value.
func (c *Collector) Plan(ctx context.Context, grace time.Duration, now time.Time) (Plan, error) {
	if c == nil {
		return Plan{}, errors.New("collector is not open")
	}
	if grace < 0 {
		return Plan{}, errors.New("gc grace period cannot be negative")
	}
	if grace < minGrace {
		grace = minGrace
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	unlock := lockStateDir(c.stateDir)
	defer unlock()
	previous, err := c.loadOrphans()
	if err != nil {
		return Plan{}, err
	}
	// The reference scan and the disk enumeration run as one critical section
	// against in-process writers (online sweeps only) so a chunk cannot gain
	// its first reference between the two views.
	c.lockGuard()
	referenced, count, unindexed, err := c.referencedChunks()
	if err != nil {
		c.unlockGuard()
		return Plan{}, err
	}
	onDisk, err := c.chunks.Enumerate(ctx)
	c.unlockGuard()
	if err != nil {
		return Plan{}, err
	}
	current := make(map[string]time.Time, len(previous))
	plan := Plan{ID: newPlanID(), GeneratedAt: now, GracePeriod: grace, Referenced: len(referenced), CheckpointCount: count, Status: "planned", UpdatedAt: now, Orphans: current, UnindexedManifests: unindexed}
	for _, info := range onDisk {
		key := chunkKey(info.Ref)
		if _, live := referenced[key]; live {
			continue
		}
		first := previous[key]
		if first.IsZero() {
			first = now
		}
		current[key] = first
		candidate := Candidate{Ref: info.Ref, Bytes: info.Ref.StoredSize, Modified: info.Modified, FirstSeenAt: first, EligibleAt: first.Add(grace), Status: "waiting"}
		if !candidate.EligibleAt.After(now) {
			candidate.Status = "eligible"
		}
		plan.Candidates = append(plan.Candidates, candidate)
	}
	sort.Slice(plan.Candidates, func(i, j int) bool { return plan.Candidates[i].EligibleAt.Before(plan.Candidates[j].EligibleAt) })
	if err := c.saveOrphans(current); err != nil {
		return Plan{}, err
	}
	if err := c.savePlan(plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// Sweep is intentionally impossible as a free function: sweeping requires a
// collector that owns its exclusion story — the offline state lock, or an
// online WriteGuard (see OpenOnline). This prevents accidental reintroduction
// of an unguarded live TOCTOU collector through an old API.
func Sweep(context.Context, *checkpoint.Repository, *chunkstore.Store, Plan, bool) (int, int, error) {
	return 0, 0, ErrLiveSweepDisabled
}

func (c *Collector) Sweep(ctx context.Context, id string, dryRun bool) (Plan, error) {
	unlock := lockStateDir(c.stateDir)
	defer unlock()
	plan, err := c.loadPlan(id)
	if err != nil {
		return Plan{}, err
	}
	if dryRun {
		plan.DryRun = true
		plan.UpdatedAt = time.Now().UTC()
		_ = c.savePlan(plan)
		return plan, nil
	}
	now := time.Now().UTC()
	plan.DryRun = false
	plan.Status = "running"
	// The reference scan and every delete share one guard section:
	// referenced is derived once (not once per candidate), no participating
	// writer can install a manifest reference between a candidate's
	// revalidation and its delete, and the exclusive hold keeps background
	// chunk writers from landing content mid-phase. The scan is one pass
	// over the repository regardless of how many candidates the plan holds.
	c.lockGuard()
	referenced, _, unindexed, refErr := c.referencedChunks()
	if refErr == nil {
		plan.UnindexedManifests = unindexed
		for plan.Cursor < len(plan.Candidates) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				c.unlockGuard()
				plan.Status = "paused"
				_ = c.savePlan(plan)
				return plan, ctxErr
			}
			candidate := &plan.Candidates[plan.Cursor]
			plan.Cursor++
			if candidate.Status == "deleted" || candidate.Status == "skipped" {
				continue
			}
			// Two-phase grace is enforced at deletion time: only a candidate
			// this plan already observed as past its grace window may be
			// deleted. A "waiting" candidate — first observed in this very
			// plan, or observed with grace still to run — is left untouched
			// for a later run instead of being swept in the same breath.
			if candidate.Status != "eligible" || (!candidate.EligibleAt.IsZero() && candidate.EligibleAt.After(now)) {
				continue
			}
			// Mid-write floor: content younger than minGrace may belong to a
			// checkpoint whose manifest is not installed yet, whatever the
			// bookkeeping says (or said before orphans.json was lost).
			if candidate.Modified.After(now.Add(-minGrace)) {
				continue
			}
			if _, live := referenced[chunkKey(candidate.Ref)]; live {
				candidate.Status, candidate.Error = "skipped", "chunk became referenced"
				continue
			}
			if err := c.chunks.DeleteRef(candidate.Ref, candidate.Modified); err != nil {
				candidate.Status, candidate.Error = "skipped", err.Error()
			} else {
				candidate.Status = "deleted"
			}
			if plan.Cursor%128 == 0 {
				_ = c.savePlan(plan)
			}
		}
	}
	c.unlockGuard()
	if refErr != nil {
		plan.Status = "paused"
		_ = c.savePlan(plan)
		return plan, refErr
	}
	c.reclaimStaleTemps()
	plan.Status = "completed"
	plan.UpdatedAt = time.Now().UTC()
	return plan, c.savePlan(plan)
}

func (c *Collector) Cancel(id string) (Plan, error) {
	unlock := lockStateDir(c.stateDir)
	defer unlock()
	plan, err := c.loadPlan(id)
	if err != nil {
		return Plan{}, err
	}
	plan.Status = "cancelled"
	plan.UpdatedAt = time.Now().UTC()
	return plan, c.savePlan(plan)
}

// validPlanID rejects anything that could escape gc/plans when joined into a
// file name: separators, dot-segments, absolute paths, and non-identifier
// characters. Plan IDs are generated as lowercase hex.
func validPlanID(id string) bool {
	if id == "" || len(id) > 64 || filepath.Base(id) != id {
		return false
	}
	for _, char := range id {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
			continue
		}
		return false
	}
	return id[0] != '-'
}

func (c *Collector) loadPlan(id string) (Plan, error) {
	if !validPlanID(id) {
		return Plan{}, ErrPlanNotFound
	}
	var plan Plan
	if err := readJSON(filepath.Join(c.stateDir, "gc", "plans", id+".json"), &plan); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Plan{}, ErrPlanNotFound
		}
		return Plan{}, err
	}
	if plan.ID != id {
		return Plan{}, ErrPlanNotFound
	}
	return plan, nil
}
func (c *Collector) savePlan(plan Plan) error {
	if !validPlanID(plan.ID) {
		return errors.New("gc plan id is required")
	}
	if err := os.MkdirAll(filepath.Join(c.stateDir, "gc", "plans"), 0o700); err != nil {
		return err
	}
	return writeJSON(filepath.Join(c.stateDir, "gc", "plans", plan.ID+".json"), plan)
}
func (c *Collector) loadOrphans() (map[string]time.Time, error) {
	var values map[string]time.Time
	err := readJSON(filepath.Join(c.stateDir, "gc", "orphans.json"), &values)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]time.Time{}, nil
	}
	if err != nil {
		return nil, err
	}
	if values == nil {
		values = map[string]time.Time{}
	}
	return values, nil
}
func (c *Collector) saveOrphans(values map[string]time.Time) error {
	return writeJSON(filepath.Join(c.stateDir, "gc", "orphans.json"), values)
}

// reclaimStaleTemps removes crashed-write temp files: chunkstore stages every
// chunk as .chunk-<rand> before the hardlink into place and gc state writes
// stage as .gc-<rand>, both of which survive a SIGKILL/power loss and are
// invisible to Enumerate. Anything older than minGrace belongs to a dead
// writer; fresh temps are left alone because a live writer may still be
// staging.
func (c *Collector) reclaimStaleTemps() {
	cutoff := time.Now().Add(-minGrace)
	for _, dir := range []string{
		filepath.Join(c.stateDir, "gc"),
		filepath.Join(c.stateDir, "objects", "chunks"),
	} {
		_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}
			name := entry.Name()
			if !strings.HasPrefix(name, ".gc-") && !strings.HasPrefix(name, ".chunk-") {
				return nil
			}
			info, statErr := entry.Info()
			if statErr != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
				return nil
			}
			_ = os.Remove(path)
			return nil
		})
	}
}

func newPlanID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("gc-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".gc-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// referencedChunks derives the set of chunks any live checkpoint still needs:
// every manifest the repository index lists, plus every manifest that exists
// on disk but is not (or not yet) indexed. checkpoint.Save writes the
// manifest before the index entry and Delete removes the index entry before
// the directory, so a crash in either window leaves a manifest file whose
// chunks the index alone would call orphaned — deleting them would destroy
// the only recovery bytes of a half-saved checkpoint. An on-disk manifest
// that cannot be authenticated is a hard error (fail closed): its chunk set
// is unknown, so nothing may be deleted until the operator resolves it.
func (c *Collector) referencedChunks() (map[string]struct{}, int, []string, error) {
	referenced := make(map[string]struct{})
	count := 0
	indexed := make(map[string]struct{})
	for _, summary := range c.repo.List("") {
		manifest, err := c.repo.Load(summary.ID)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("load checkpoint %s for gc: %w", summary.ID, err)
		}
		count++
		indexed[summary.ID] = struct{}{}
		for _, asset := range manifest.Assets {
			for _, ref := range asset.Chunks {
				referenced[chunkKey(ref)] = struct{}{}
			}
		}
	}
	unindexed, err := c.onDiskManifestChunks(indexed, referenced)
	if err != nil {
		return nil, 0, nil, err
	}
	return referenced, count, unindexed, nil
}

func (c *Collector) onDiskManifestChunks(indexed, referenced map[string]struct{}) ([]string, error) {
	root := filepath.Join(c.stateDir, "checkpoints")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var unindexed []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "manifest.enc.json")
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		encoded, readErr := io.ReadAll(io.LimitReader(file, maxEncryptedManifestBytes+1))
		_ = file.Close()
		if readErr != nil {
			return nil, readErr
		}
		manifest, err := c.repo.DecodeEncryptedManifest(entry.Name(), encoded)
		if err != nil {
			return nil, fmt.Errorf("on-disk checkpoint manifest %s is unreadable for gc: %w", entry.Name(), err)
		}
		if _, ok := indexed[manifest.ID]; !ok {
			unindexed = append(unindexed, manifest.ID)
		}
		for _, asset := range manifest.Assets {
			for _, ref := range asset.Chunks {
				referenced[chunkKey(ref)] = struct{}{}
			}
		}
	}
	sort.Strings(unindexed)
	return unindexed, nil
}

func chunkKey(ref model.ChunkRef) string { return fmt.Sprintf("v%d/%s", ref.KeyVersion, ref.Address) }
