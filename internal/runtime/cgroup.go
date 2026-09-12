package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shift.dev/shift/internal/model"
)

type CgroupManager struct {
	root     string
	required bool
	enabled  bool
}

// DefaultCgroupRoot is where workload cgroups live unless the agent is
// configured otherwise. It is a per-agent resource: two agents on one machine
// must not share it, or the workload a migration restores on one of them
// lands in the cgroup the other is about to tear down — the shared root is
// why a removal that should be instant meets EBUSY instead.
const DefaultCgroupRoot = "/sys/fs/cgroup/shift"

// ErrCgroupOccupied reports that a cgroup directory could not be removed
// because it still holds processes that are not this workload's dying
// remnants — another agent's tree in a shared root, or a straggler no retry
// would outlive. The directory is deliberately left in place.
var ErrCgroupOccupied = errors.New("cgroup still holds processes; left in place")

func NewCgroupManager(required bool, root string) (*CgroupManager, error) {
	if root == "" {
		root = DefaultCgroupRoot
	}
	manager := &CgroupManager{root: root, required: required}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		if required {
			return nil, errors.New("cgroup v2 is required but not mounted")
		}
		return manager, nil
	}
	if err := os.MkdirAll(manager.root, 0o755); err != nil {
		if required {
			return nil, fmt.Errorf("create SHIFT cgroup: %w", err)
		}
		return manager, nil
	}
	if err := manager.enableControllers(); err != nil {
		if required {
			return nil, fmt.Errorf("enable SHIFT cgroup controllers: %w", err)
		}
		return manager, nil
	}
	manager.enabled = true
	return manager, nil
}

// shiftControllers are the cgroup controllers SHIFT's resource controls need
// enabled in the SHIFT root's subtree_control.
var shiftControllers = []string{"memory", "pids", "cpu"}

// enableControllers turns on the resource controllers in the SHIFT root's
// subtree_control. cgroup v2 creates a controller's control files in a child
// cgroup only when the controller is enabled in the parent's subtree_control,
// so without this step a workload cgroup has no memory.max or pids.max at all,
// and applying a limit fails with a misleading permission error. Only
// controllers the kernel offers under the SHIFT root are enabled — naming an
// unavailable one would fail the entire write. Enabling is idempotent, and
// requires no process to sit in the SHIFT root itself, which never happens:
// workloads live in their own cgroups below it.
func (m *CgroupManager) enableControllers() error {
	available, err := os.ReadFile(filepath.Join(m.root, "cgroup.controllers"))
	if err != nil {
		return err
	}
	offered := make(map[string]bool)
	for _, controller := range strings.Fields(string(available)) {
		offered[controller] = true
	}
	wanted := make([]string, 0, len(shiftControllers))
	for _, controller := range shiftControllers {
		if offered[controller] {
			wanted = append(wanted, "+"+controller)
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	return writeControl(m.root, "cgroup.subtree_control", strings.Join(wanted, " "))
}

// Open creates the workload's cgroup, applies its resource controls, and
// returns a descriptor on the cgroup directory. Spawning the workload with
// that descriptor — clone's CLONE_INTO_CGROUP — places every process of the
// tree inside the cgroup from the instant it exists. A child the workload
// forks a millisecond after exec is born there too, instead of briefly
// living in the agent's own cgroup where no later attach can reach it and
// where a checkpoint would record a second, unrestoreable cgroup for the
// tree. The descriptor is nil when cgroups are unavailable, in which case
// workloads simply run unconfined.
func (m *CgroupManager) Open(workload model.Workload) (*os.File, error) {
	if !m.enabled {
		return nil, nil
	}
	path := m.path(workload.Spec.ID)
	if err := m.prepare(path, workload.Spec.Resources); err != nil {
		return nil, err
	}
	return os.Open(path)
}

// prepare creates the workload's cgroup directory and applies the resource
// controls its spec declares.
func (m *CgroupManager) prepare(path string, resources model.ResourceRequirements) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if resources.MemoryBytes > 0 {
		if err := writeControl(path, "memory.max", strconv.FormatUint(resources.MemoryBytes, 10)); err != nil {
			return err
		}
		// Cap swap too: memory.max bounds RAM only, and without a swap cap
		// the kernel honors the limit by swapping anonymous pages out, so a
		// workload that blows past its memory limit thrashes swap forever
		// instead of dying. A memory limit that permits unbounded swap is
		// not a limit — exceeding it is a hard OOM kill, the same semantics
		// Kubernetes gives a memory limit.
		if err := writeControl(path, "memory.swap.max", "0"); err != nil {
			return err
		}
	}
	if resources.PIDs > 0 {
		if err := writeControl(path, "pids.max", strconv.FormatUint(resources.PIDs, 10)); err != nil {
			return err
		}
	}
	if resources.CPUCount > 0 {
		period := int64(100000)
		quota := int64(resources.CPUCount * float64(period))
		if quota < 1000 {
			quota = 1000
		}
		if err := writeControl(path, "cpu.max", fmt.Sprintf("%d %d", quota, period)); err != nil {
			return err
		}
	}
	return nil
}

// Attach moves a running process into the workload's cgroup. It is for
// processes the agent did not spawn itself — a tree CRIU restored — because
// a spawned workload is placed in its cgroup at clone time instead.
func (m *CgroupManager) Attach(workload model.Workload, pid int) error {
	if !m.enabled {
		return nil
	}
	path := m.path(workload.Spec.ID)
	if err := m.prepare(path, workload.Spec.Resources); err != nil {
		return err
	}
	return writeControl(path, "cgroup.procs", strconv.Itoa(pid))
}

// parkedPath returns the cgroup that holds a frozen source tree while a
// restore recreates the workload beside it. It is keyed by the restore
// session id — a fresh random UUID per restore — so it can never collide
// with the cgroup of any workload, however the workload is named.
func (m *CgroupManager) parkedPath(key string) string {
	return m.path(key)
}

// ParkFrozenSource moves a frozen source tree out of its workload cgroup
// into a private sibling cgroup for the duration of a restore. A checkpoint
// that does not leave its source running keeps the stopped tree on the
// machine as the rollback copy, and a restore recreates the workload in the
// same cgroup the checkpoint recorded — so without this move both trees sit
// in one cgroup and the workload's own limits count the rollback copy
// against the restore: a tree of thousands of processes exhausts its pids
// budget halfway through being recreated, and a memory-capped restore is
// OOM-killed the moment its pages land. The parked cgroup carries the same
// limits the workload declared, so the frozen copy stays as confined as it
// ever was.
func (m *CgroupManager) ParkFrozenSource(workload model.Workload, key string) error {
	if !m.enabled {
		return nil
	}
	pids, err := m.readProcs(m.path(workload.Spec.ID))
	if err != nil || len(pids) == 0 {
		return err
	}
	parked := m.parkedPath(key)
	if err := m.prepare(parked, workload.Spec.Resources); err != nil {
		return err
	}
	return m.moveProcs(parked, pids)
}

// UnparkFrozenSource moves a parked frozen source tree back into its
// workload cgroup and drops the parking cgroup — the rollback path of a
// restore that did not commit, so a resumed workload always runs in the
// cgroup its checkpoint recorded.
func (m *CgroupManager) UnparkFrozenSource(workloadID, key string) error {
	if !m.enabled {
		return nil
	}
	parked := m.parkedPath(key)
	pids, err := m.readProcs(parked)
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		if err := os.Remove(parked); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	target := m.path(workloadID)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if err := m.moveProcs(target, pids); err != nil {
		return err
	}
	return os.Remove(parked)
}

// ReapFrozenSource kills a parked frozen source tree and removes its cgroup:
// the commit path, once the restored workload has proven itself and the
// rollback copy is no longer needed. cgroup.kill tears the whole subtree
// down at once, and SIGKILL reaches a stopped process directly.
func (m *CgroupManager) ReapFrozenSource(key string) error {
	if !m.enabled {
		return nil
	}
	parked := m.parkedPath(key)
	if _, err := os.Stat(parked); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := writeControl(parked, "cgroup.kill", "1"); err != nil {
		return err
	}
	return m.removeCgroupDir(parked)
}

// readProcs lists the process ids attached to a cgroup. A missing cgroup
// reads as empty: parking and unparking treat that as "nothing to do".
func (m *CgroupManager) readProcs(path string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return strings.Fields(string(data)), nil
}

// moveProcs attaches processes to a cgroup. cgroup.procs accepts many ids in
// one write, but the whole write fails if any listed process is gone by the
// time it runs, so a failed batch falls back to moving the survivors one by
// one.
func (m *CgroupManager) moveProcs(target string, pids []string) error {
	for start := 0; start < len(pids); start += 128 {
		end := min(start+128, len(pids))
		batch := strings.Join(pids[start:end], "\n")
		if err := writeControl(target, "cgroup.procs", batch); err != nil {
			for _, pid := range pids[start:end] {
				if err := writeControl(target, "cgroup.procs", pid); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
	}
	return nil
}

func (m *CgroupManager) Freeze(workloadID string) error {
	if !m.enabled {
		return os.ErrNotExist
	}
	return writeControl(m.path(workloadID), "cgroup.freeze", "1")
}

func (m *CgroupManager) Thaw(workloadID string) error {
	if !m.enabled {
		return os.ErrNotExist
	}
	return writeControl(m.path(workloadID), "cgroup.freeze", "0")
}

func (m *CgroupManager) Remove(workloadID string) error {
	if !m.enabled {
		return nil
	}
	return m.removeCgroupDir(m.path(workloadID))
}

// removeCgroupDir removes a cgroup directory, retrying briefly while the
// kernel still lists dying processes in it: a killed tree releases its
// membership asynchronously, so an immediate removal can race EBUSY. Members
// that persist past that window are not dying remnants — they are processes
// that belong to someone else (a second agent on the machine restoring the
// same workload id into a shared root) or a straggler no bounded retry would
// outlive — and the directory is left standing under ErrCgroupOccupied:
// removing another tenant's cgroup would be destruction, not cleanup.
func (m *CgroupManager) removeCgroupDir(path string) error {
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		err := os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) || time.Now().After(deadline) {
			if errors.Is(err, syscall.EBUSY) {
				if procs, readErr := m.readProcs(path); readErr == nil && len(procs) > 0 {
					return fmt.Errorf("%w (%s)", ErrCgroupOccupied, path)
				}
			}
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (m *CgroupManager) path(workloadID string) string {
	safe := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(workloadID)
	return filepath.Join(m.root, safe)
}

func writeControl(directory, name, value string) error {
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		// cgroup v2 creates a control file in a child cgroup only for
		// controllers the parent has enabled in its subtree_control, and
		// creating a file on cgroupfs reports EACCES — so a missing control
		// reads as a bare permission error. Name the real cause.
		if _, statErr := os.Stat(path); statErr != nil {
			return fmt.Errorf("control %s does not exist (its controller is not enabled under %s): %w", path, directory, err)
		}
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
