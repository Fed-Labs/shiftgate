package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/network"
	"shift.dev/shift/internal/persistence"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	"shift.dev/shift/internal/securestore"
)

type Manager struct {
	mu       sync.Mutex
	stateDir string
	records  *securestore.EncryptedCollection[model.Workload]
	cgroups  *CgroupManager
	logger   *slog.Logger
	commands map[string]*exec.Cmd
	// stopping marks workloads whose end the agent itself is ordering — the
	// operator's stop, the retirement of a superseded source — so the exit
	// the pending waiter records is a stop, not a failure.
	stopping map[string]bool
}

func OpenManager(stateDir string, requireCgroup bool, logger *slog.Logger) (*Manager, error) {
	keys, err := securestore.Open(filepath.Join(stateDir, "keys"))
	if err != nil {
		return nil, err
	}
	return OpenManagerWithKeys(stateDir, requireCgroup, logger, keys)
}

func OpenManagerWithKeys(stateDir string, requireCgroup bool, logger *slog.Logger, keys *securestore.Manager) (*Manager, error) {
	return OpenManagerWithCgroups(stateDir, "", requireCgroup, logger, keys)
}

// OpenManagerWithCgroups opens a manager whose workload cgroups live under
// cgroupRoot instead of the default root. Distinct roots are how two agents
// on one machine stay out of each other's cgroups: every path the manager
// derives — workload cgroups, parked frozen sources — lands under the root it
// was given, so an agent only ever touches trees it owns.
func OpenManagerWithCgroups(stateDir, cgroupRoot string, requireCgroup bool, logger *slog.Logger, keys *securestore.Manager) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	records, err := securestore.OpenEncryptedCollection[model.Workload](filepath.Join(stateDir, "metadata", "workloads.enc.json"), "agent-workloads-v1", keys)
	if err != nil {
		return nil, err
	}
	cgroups, err := NewCgroupManager(requireCgroup, cgroupRoot)
	if err != nil {
		return nil, err
	}
	manager := &Manager{
		stateDir: stateDir,
		records:  records,
		cgroups:  cgroups,
		logger:   logger,
		commands: make(map[string]*exec.Cmd),
		stopping: make(map[string]bool),
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "logs"), 0o700); err != nil {
		return nil, err
	}
	if err := manager.Reconcile(); err != nil {
		return nil, err
	}
	// The port claims of workloads that survived the restart are rebuilt
	// here, before the agent wires its network coordinator: the NAT table is
	// in-memory and starts empty, and without this a fresh agent hands a
	// still-running workload's host ports to the next restore. A conflict
	// between surviving claims is logged, not fatal — the agent must come up
	// even when its records disagree, and the next VALIDATE will refuse the
	// contested port either way.
	network.RebuildReservations(manager.portHoldingWorkloads())
	return manager, nil
}

// portHoldingWorkloads lists the records whose host-port claims still hold
// after Reconcile: a record whose process is alive (Reconcile cleared the
// dead ones) or whose status is checkpointed — the ports of a checkpoint
// belong to its next restore until the record goes away.
func (m *Manager) portHoldingWorkloads() []model.Workload {
	var held []model.Workload
	for _, workload := range m.records.List() {
		if workload.Process != nil || workload.Status == model.WorkloadCheckpointed {
			held = append(held, workload)
		}
	}
	return held
}

func (m *Manager) Create(spec model.WorkloadSpec) (model.Workload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := spec.Normalize(); err != nil {
		return model.Workload{}, err
	}
	info, err := os.Stat(spec.RootPath)
	if err != nil {
		return model.Workload{}, fmt.Errorf("inspect workload root: %w", err)
	}
	if !info.IsDir() {
		return model.Workload{}, errors.New("workload root must be a directory")
	}
	if info, err := os.Stat(spec.WorkingDir); err != nil || !info.IsDir() {
		return model.Workload{}, errors.New("working directory does not exist or is not a directory")
	}
	for _, existing := range m.records.List() {
		if strings.EqualFold(existing.Spec.Name, spec.Name) {
			return model.Workload{}, fmt.Errorf("workload name %q already exists", spec.Name)
		}
		if existing.Spec.ID == spec.ID {
			return model.Workload{}, fmt.Errorf("workload ID %q already exists", spec.ID)
		}
		if strings.EqualFold(existing.Spec.Name, spec.ID) {
			return model.Workload{}, fmt.Errorf("workload ID %q collides with the name of workload %s", spec.ID, existing.Spec.ID)
		}
		if existing.Spec.ID == spec.Name {
			return model.Workload{}, fmt.Errorf("workload name %q collides with the ID of workload %s", spec.Name, existing.Spec.ID)
		}
	}
	workload := model.Workload{Spec: spec, Status: model.WorkloadRegistered, Generation: 1}
	if err := m.records.Put(spec.ID, workload); err != nil {
		return model.Workload{}, err
	}
	return workload, nil
}

// SetCheckpointPolicy replaces a workload's periodic-checkpoint policy and
// persists it, so a schedule survives agent restarts. A workload's spec is
// otherwise immutable — the policy is the one operator-facing mutation
// because schedules change with operational reality while a workload's
// identity does not. A nil policy disables scheduling.
func (m *Manager) SetCheckpointPolicy(idOrName string, policy *model.CheckpointPolicySpec) (model.Workload, error) {
	if err := policy.Validate(); err != nil {
		return model.Workload{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	workload, err := m.Get(idOrName)
	if err != nil {
		return model.Workload{}, err
	}
	// Removing the schedule while a standby waits for replicas would strand
	// the standby at the checkpoint it already holds — the same silent-staleness
	// lie the spec-level cross-check refuses, enforced here because a policy
	// change on a live workload never re-runs spec validation.
	if policy == nil && workload.Spec.FailoverPolicy != nil {
		return model.Workload{}, errors.New("workload has a failover policy; remove it first (workload failover NAME --off) or keep a checkpoint schedule")
	}
	workload.Spec.CheckpointPolicy = policy
	workload.Spec.UpdatedAt = time.Now().UTC()
	return workload, m.records.Put(workload.Spec.ID, workload)
}

// SetFailoverPolicy attaches (or, with nil, removes) a workload's warm-standby
// designation. The policy itself is inert data here: the replication that
// follows it runs in the agent's policy loop, and this setter only persists
// the operator's intent where the spec already lives.
func (m *Manager) SetFailoverPolicy(idOrName string, policy *model.FailoverPolicySpec) (model.Workload, error) {
	if err := policy.Validate(); err != nil {
		return model.Workload{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	workload, err := m.Get(idOrName)
	if err != nil {
		return model.Workload{}, err
	}
	if policy != nil && workload.Spec.CheckpointPolicy == nil {
		return model.Workload{}, errors.New("failover policy requires a checkpoint policy: without periodic checkpoints nothing would replicate to the standby")
	}
	workload.Spec.FailoverPolicy = policy
	workload.Spec.UpdatedAt = time.Now().UTC()
	return workload, m.records.Put(workload.Spec.ID, workload)
}

func (m *Manager) Get(idOrName string) (model.Workload, error) {
	if workload, err := m.records.Get(idOrName); err == nil {
		return workload, nil
	}
	for _, workload := range m.records.List() {
		if workload.Spec.Name == idOrName {
			return workload, nil
		}
	}
	return model.Workload{}, persistence.ErrNotFound
}

func (m *Manager) List() []model.Workload {
	workloads := m.records.List()
	sort.Slice(workloads, func(i, j int) bool { return workloads[i].Spec.CreatedAt.Before(workloads[j].Spec.CreatedAt) })
	return workloads
}

func (m *Manager) Start(idOrName string) (model.Workload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workload, err := m.Get(idOrName)
	if err != nil {
		return model.Workload{}, err
	}
	if workload.Process != nil && linuxplatform.ProcessAlive(workload.Process.PID, workload.Process.ProcStartTicks) {
		return model.Workload{}, errors.New("workload is already running")
	}
	switch workload.Status {
	case model.WorkloadRegistered, model.WorkloadStopped, model.WorkloadCheckpointed, model.WorkloadFailed:
	default:
		return model.Workload{}, fmt.Errorf("cannot start workload in %s state", workload.Status)
	}
	executable, err := resolveExecutable(workload.Spec.Command[0])
	if err != nil {
		return model.Workload{}, err
	}
	logFile, err := m.openWorkloadLog(workload.Spec)
	if err != nil {
		return model.Workload{}, err
	}
	cgroup, err := m.cgroups.Open(workload)
	if err != nil {
		_ = logFile.Close()
		return model.Workload{}, fmt.Errorf("prepare workload cgroup: %w", err)
	}
	defer func() { _ = cgroup.Close() }()
	argv := append([]string{executable}, workload.Spec.Command[1:]...)
	privileged := os.Geteuid() == 0
	if privileged {
		// A privileged agent launches workloads through its own exec shim,
		// which installs the io_uring seccomp filter and then replaces
		// itself with the workload's command. Go's process spawning has no
		// seccomp hook, so the filter cannot be applied between the fork and
		// the exec; re-executing this binary — the same trick the
		// mount-namespace shim uses — is the one place it can go. The pid,
		// session, cgroup, and credential set below all survive the exec.
		self, selfErr := os.Executable()
		if selfErr != nil {
			_ = logFile.Close()
			return model.Workload{}, fmt.Errorf("resolve the agent binary to launch the workload: %w", selfErr)
		}
		argv = append([]string{self, WorkloadExecCommand, "--"}, argv...)
	}
	// The workload is not tied to any request context: it runs until the
	// operator or its own exit stops it, so its command carries a context
	// that is never canceled.
	command := exec.CommandContext(context.Background(), argv[0], argv[1:]...)
	command.Dir = workload.Spec.WorkingDir
	command.Env = mergedEnvironment(workload.Spec.Environment)
	command.Stdout = logFile
	command.Stderr = logFile
	// The workload leads its own session: setsid makes it the session and
	// process-group leader, so its sid, pgid, and pid all sit inside its PID
	// namespace. A workload that only left the agent's process group still
	// belongs to the agent's session, and CRIU refuses to dump a process
	// whose session leader lives outside its PID namespace — the namespace
	// init could never recreate that relationship on restore.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cgroup != nil {
		// CLONE_INTO_CGROUP: the workload and every process it ever forks
		// are born inside the workload's cgroup. Attaching after the spawn
		// instead would lose the race against a workload that forks workers
		// immediately — those children would stay in the agent's cgroup, and
		// a checkpoint would record two cgroups for one tree, one of which
		// CRIU refuses to restore.
		command.SysProcAttr.UseCgroupFD = true
		command.SysProcAttr.CgroupFD = int(cgroup.Fd())
	}
	if privileged {
		// A privileged agent gives every workload its own PID namespace.
		// The checkpoint images then carry that namespace, so a restore — on
		// another machine, on this one while the frozen source is still
		// preserved, or as a fork beside its running source — lands in a
		// fresh namespace instead of colliding with the pids the original
		// tree still holds (CRIU recreates the original pids, and without a
		// namespace two trees can never share them). Creating a PID namespace
		// needs CAP_SYS_ADMIN, which the agent holds exactly when it can also
		// run CRIU; an unprivileged agent cannot checkpoint anything, so its
		// workloads run in the host namespace.
		command.SysProcAttr.Cloneflags = syscall.CLONE_NEWPID
		command.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(workload.Spec.UID), Gid: uint32(workload.Spec.GID)}
	} else if workload.Spec.UID != os.Geteuid() || workload.Spec.GID != os.Getegid() {
		_ = logFile.Close()
		return model.Workload{}, errors.New("changing workload uid/gid requires a root agent")
	}
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return model.Workload{}, fmt.Errorf("start workload: %w", err)
	}
	pid := command.Process.Pid
	startTicks, err := waitForStartTicks(pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = logFile.Close()
		return model.Workload{}, err
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = logFile.Close()
		return model.Workload{}, err
	}
	workload.Status = model.WorkloadRunning
	workload.Process = &model.ProcessState{PID: pid, PGID: pgid, ProcStartTicks: startTicks, StartedAt: time.Now().UTC()}
	workload.Generation++
	workload.LastError = ""
	if err := m.records.Put(workload.Spec.ID, workload); err != nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = command.Wait()
		_ = logFile.Close()
		return model.Workload{}, err
	}
	m.commands[workload.Spec.ID] = command
	delete(m.stopping, workload.Spec.ID)
	go m.wait(workload.Spec.ID, startTicks, command, logFile)
	m.logger.Info("workload started", "workload_id", workload.Spec.ID, "pid", pid)
	return workload, nil
}

func (m *Manager) Pause(idOrName string) (model.Workload, error) {
	return m.signalAndUpdate(idOrName, syscall.SIGSTOP, model.WorkloadPaused)
}

func (m *Manager) Resume(idOrName string) (model.Workload, error) {
	return m.signalAndUpdate(idOrName, syscall.SIGCONT, model.WorkloadRunning)
}

func (m *Manager) signalAndUpdate(idOrName string, signal syscall.Signal, status model.WorkloadStatus) (model.Workload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workload, err := m.Get(idOrName)
	if err != nil {
		return model.Workload{}, err
	}
	if workload.Process == nil || !linuxplatform.ProcessAlive(workload.Process.PID, workload.Process.ProcStartTicks) {
		return model.Workload{}, errors.New("workload process is not alive")
	}
	if err := syscall.Kill(-workload.Process.PGID, signal); err != nil {
		return model.Workload{}, fmt.Errorf("signal workload: %w", err)
	}
	workload.Status = status
	workload.Generation++
	if err := m.records.Put(workload.Spec.ID, workload); err != nil {
		return model.Workload{}, err
	}
	return workload, nil
}

func (m *Manager) Stop(idOrName string, timeout time.Duration) (model.Workload, error) {
	m.mu.Lock()
	workload, err := m.Get(idOrName)
	if err != nil {
		m.mu.Unlock()
		return model.Workload{}, err
	}
	if workload.Process == nil || !linuxplatform.ProcessAlive(workload.Process.PID, workload.Process.ProcStartTicks) {
		workload.Status = model.WorkloadStopped
		workload.Process = nil
		workload.Generation++
		delete(m.stopping, workload.Spec.ID)
		err = m.records.Put(workload.Spec.ID, workload)
		m.mu.Unlock()
		return workload, err
	}
	process := *workload.Process
	m.stopping[workload.Spec.ID] = true
	m.mu.Unlock()
	// The stopping flag must survive until the waiter (or the adopted-process
	// watcher) records the exit as a stop, but it must not outlive an
	// abandoned stop: if the kill fails the agent is no longer ordering this
	// workload's end, and a later death of its own must be recorded as the
	// failure it is — a leaked flag would hide it as a clean stop.
	signaled := false
	defer func() {
		if !signaled {
			m.mu.Lock()
			delete(m.stopping, workload.Spec.ID)
			m.mu.Unlock()
		}
	}()
	// A checkpointed tree sits stopped where CRIU left it: SIGTERM only
	// queues behind the stop and burns the whole grace period, and resuming
	// it with SIGCONT first would let the frozen rollback copy execute. The
	// checkpoint's data is already durable in the chunk store, so it is
	// killed outright — SIGKILL is delivered to stopped processes directly.
	terminate := syscall.SIGTERM
	if workload.Status == model.WorkloadCheckpointed {
		terminate = syscall.SIGKILL
		// A tree CRIU left stopped may also sit in a frozen cgroup, where
		// even SIGKILL stays pending until the freezer opens. Thaw first
		// so the kill is delivered, not queued.
		_ = m.cgroups.Thaw(workload.Spec.ID)
	} else {
		_ = killProcessGroup(-process.PGID, syscall.SIGCONT)
	}
	if err := killProcessGroup(-process.PGID, terminate); err != nil && !errors.Is(err, syscall.ESRCH) {
		return model.Workload{}, err
	}
	signaled = true
	deadline := time.Now().Add(timeout)
	for linuxplatform.ProcessAlive(process.PID, process.ProcStartTicks) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if linuxplatform.ProcessAlive(process.PID, process.ProcStartTicks) {
		if err := killProcessGroup(-process.PGID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return model.Workload{}, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	workload, err = m.Get(workload.Spec.ID)
	if err != nil {
		return model.Workload{}, err
	}
	workload.Status = model.WorkloadStopped
	workload.Process = nil
	workload.Generation++
	if err := m.records.Put(workload.Spec.ID, workload); err != nil {
		return model.Workload{}, err
	}
	// The terminal state is recorded here; the waiter classifying the same
	// exit is now redundant, so the flag is cleared for this generation too.
	delete(m.stopping, workload.Spec.ID)
	m.removeCgroup(workload.Spec.ID)
	return workload, nil
}

// killProcessGroup signals a workload's process group. It is a variable so
// tests can exercise Stop's failure paths — a signal that fails against a
// live group cannot be arranged portably.
var killProcessGroup = syscall.Kill

// removeCgroup tears down the workload's cgroup directory and reports what
// happened: an occupied directory is another tenant's, not a failure.
func (m *Manager) removeCgroup(workloadID string) {
	if err := m.cgroups.Remove(workloadID); err != nil {
		if errors.Is(err, ErrCgroupOccupied) {
			m.logger.Debug("workload cgroup left in place — it still holds processes",
				"workload_id", workloadID)
			return
		}
		m.logger.Debug("remove workload cgroup", "workload_id", workloadID, "error", err)
	}
}

// Retire marks a workload's process as intentionally ended — the agent is
// about to kill it itself, as a committed restore does to the frozen source a
// checkpoint left behind. The exit the pending waiter then records is an
// ordinary stop, not a failure.
func (m *Manager) Retire(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopping[id] = true
}

func (m *Manager) AdoptRestored(id string, pid int) (model.Workload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workload, err := m.Get(id)
	if err != nil {
		return model.Workload{}, err
	}
	// The restored process's real credentials come from the checkpoint's
	// CRIU images, not from the manifest that admitted them. A hostile
	// checkpoint can declare uid 1000 to pass admission and carry images of
	// a root process — so the live process is read back from /proc and
	// reconciled with the manifest before adoption declares success. On
	// mismatch the process is killed here and the caller's rollback unwinds
	// the transaction; nothing else in the agent will ever run it.
	if err := verifyRestoredCredentials(pid, workload.Spec); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return model.Workload{}, err
	}
	startTicks, err := linuxplatform.ProcessStartTicks(pid)
	if err != nil {
		return model.Workload{}, err
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return model.Workload{}, err
	}
	workload.Process = &model.ProcessState{PID: pid, PGID: pgid, ProcStartTicks: startTicks, StartedAt: time.Now().UTC()}
	workload.Status = model.WorkloadRunning
	workload.Generation++
	delete(m.stopping, id)
	if err := m.cgroups.Attach(workload, pid); err != nil {
		return model.Workload{}, err
	}
	if err := m.records.Put(id, workload); err != nil {
		return model.Workload{}, err
	}
	go m.watchAdopted(id, startTicks, pid)
	return workload, nil
}

// adoptedReapInterval is how often a restored process's end is checked. The
// agent did not spawn these processes through os/exec, so there is no command
// to Wait on; the watcher polls and routes the death through the
// exit-recording path shared with spawned workloads.
const adoptedReapInterval = 500 * time.Millisecond

// describeWaitStatus renders a kernel exit status for a workload record.
func describeWaitStatus(status syscall.WaitStatus) string {
	switch {
	case status.Exited():
		return fmt.Sprintf("exit status %d", status.ExitStatus())
	case status.Signaled():
		return fmt.Sprintf("signal %s", status.Signal())
	default:
		return "unusual exit state"
	}
}

// watchAdopted supervises a restored process the manager adopted — the
// restore, fork, and clone paths all land here — so its death becomes record
// state immediately instead of at the next agent restart. It never restarts
// anything: a dead workload is recorded as failed or stopped and left there.
func (m *Manager) watchAdopted(id string, startTicks uint64, pid int) {
	for {
		time.Sleep(adoptedReapInterval)
		if exitErr, ended := adoptedProcessExit(pid, startTicks); ended {
			m.recordExit(id, startTicks, exitErr)
			return
		}
	}
}

// adoptedProcessExit reports whether a restored process has ended and what
// its exit is worth recording. A restored root that is the agent's child is
// reaped here — a zombie would outlive its death as far as /proc is
// concerned and leak a process slot until the agent exits — and its exit
// status is recorded the way a spawned workload's would be. A root CRIU
// parented elsewhere cannot be waited on: its death is detected through the
// same process identity Start recorded, and its exit status is unrecoverable.
func adoptedProcessExit(pid int, startTicks uint64) (error, bool) {
	var status syscall.WaitStatus
	reaped, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	if err == nil && reaped == pid {
		if status.Exited() && status.ExitStatus() == 0 {
			return nil, true
		}
		return fmt.Errorf("restored process exited: %s", describeWaitStatus(status)), true
	}
	if err == nil {
		return nil, false
	}
	if linuxplatform.ProcessAlive(pid, startTicks) {
		return nil, false
	}
	return errors.New("restored process exited"), true
}

// ParkFrozenSource moves a checkpoint's frozen source tree into a private
// cgroup while a restore recreates the workload in the cgroup its checkpoint
// recorded. Without the move both trees share one cgroup and the workload's
// own pids and memory limits count the rollback copy against the restore.
func (m *Manager) ParkFrozenSource(workload model.Workload, key string) error {
	return m.cgroups.ParkFrozenSource(workload, key)
}

// UnparkFrozenSource moves a parked frozen source tree back into its
// workload cgroup — the rollback path of a restore that did not commit.
func (m *Manager) UnparkFrozenSource(workloadID, key string) error {
	return m.cgroups.UnparkFrozenSource(workloadID, key)
}

// ReapFrozenSource kills a parked frozen source tree and removes its cgroup —
// the commit path, once the restored workload has proven itself.
func (m *Manager) ReapFrozenSource(key string) error {
	return m.cgroups.ReapFrozenSource(key)
}

func (m *Manager) PrepareRestore(spec model.WorkloadSpec) (model.Workload, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workload, err := m.Get(spec.ID)
	if err != nil {
		if !errors.Is(err, persistence.ErrNotFound) {
			return model.Workload{}, false, err
		}
		if err := spec.Normalize(); err != nil {
			return model.Workload{}, false, err
		}
		for _, existing := range m.records.List() {
			if strings.EqualFold(existing.Spec.Name, spec.Name) && existing.Spec.ID != spec.ID {
				return model.Workload{}, false, fmt.Errorf("workload name %q already exists", spec.Name)
			}
		}
		workload = model.Workload{Spec: spec, Status: model.WorkloadRestoring, Generation: 1}
		if err := m.records.Put(spec.ID, workload); err != nil {
			return model.Workload{}, false, err
		}
		return workload, true, nil
	}
	if workload.Spec.ID != spec.ID {
		return model.Workload{}, false, fmt.Errorf("restore spec id %q does not match the workload record it would replace (%s)", spec.ID, workload.Spec.ID)
	}
	if !strings.EqualFold(workload.Spec.Name, spec.Name) {
		return model.Workload{}, false, fmt.Errorf("restore cannot rename workload %q to %q", workload.Spec.Name, spec.Name)
	}
	// The replacement spec is checkpoint-manifest data and the manifest is a
	// trust boundary — a compromised source signs hostile manifests — so it
	// is validated exactly like a fresh spec instead of assigned raw. An
	// empty command would otherwise survive here and panic the next Start;
	// a root of "/" or a working directory outside the root would be
	// installed with no validation at all.
	if err := spec.Normalize(); err != nil {
		return model.Workload{}, false, err
	}
	if workload.Process != nil && linuxplatform.ProcessAlive(workload.Process.PID, workload.Process.ProcStartTicks) {
		if workload.Status != model.WorkloadCheckpointed {
			return model.Workload{}, false, errors.New("destination workload is already active")
		}
		// A checkpoint whose source is not left running freezes that source
		// on the machine as the rollback copy: the process sits SIGSTOP'd
		// with this record, and an agent restart recovers exactly this
		// state. Restoring that checkpoint supersedes the frozen tree — it
		// stays alive until the restore commits, then is reaped, and a
		// rollback resumes it — because the restore lands in a fresh PID
		// namespace and cannot collide with the pids it holds. A workload
		// that is running, or merely paused, is active and must not be
		// restored over.
	}
	workload.Spec = spec
	workload.Status = model.WorkloadRestoring
	workload.Process = nil
	workload.Generation++
	if err := m.records.Put(spec.ID, workload); err != nil {
		return model.Workload{}, false, err
	}
	return workload, false, nil
}

func (m *Manager) MarkRestoreFailed(id, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	workload, err := m.Get(id)
	if err != nil {
		return err
	}
	workload.Status = model.WorkloadFailed
	workload.Process = nil
	workload.LastError = reason
	workload.Generation++
	return m.records.Put(id, workload)
}

func (m *Manager) RestoreMetadata(workload model.Workload) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if workload.Spec.ID == "" {
		return errors.New("workload id is required")
	}
	if current, err := m.Get(workload.Spec.ID); err == nil && current.Process != nil && linuxplatform.ProcessAlive(current.Process.PID, current.Process.ProcStartTicks) {
		return errors.New("cannot replace metadata for an active workload")
	}
	return m.records.Put(workload.Spec.ID, workload)
}

func (m *Manager) MarkCheckpoint(id, checkpointID string, sourceStillRunning bool) (model.Workload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workload, err := m.Get(id)
	if err != nil {
		return model.Workload{}, err
	}
	workload.LatestCheckpointID = checkpointID
	if sourceStillRunning {
		workload.Status = model.WorkloadRunning
	} else {
		workload.Status = model.WorkloadCheckpointed
	}
	workload.Generation++
	return workload, m.records.Put(workload.Spec.ID, workload)
}

func (m *Manager) Delete(idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	workload, err := m.Get(idOrName)
	if err != nil {
		return err
	}
	if workload.Process != nil && linuxplatform.ProcessAlive(workload.Process.PID, workload.Process.ProcStartTicks) {
		return errors.New("stop the workload before deleting it")
	}
	if err := m.records.Delete(workload.Spec.ID); err != nil {
		return err
	}
	_ = m.cgroups.Remove(workload.Spec.ID)
	return nil
}

func (m *Manager) Reconcile() error {
	for _, workload := range m.records.List() {
		if workload.Process == nil {
			continue
		}
		if linuxplatform.ProcessAlive(workload.Process.PID, workload.Process.ProcStartTicks) {
			// The process survived the agent restart (workloads are
			// detached from it), so the same supervision a fresh adoption
			// gets is installed here: without it, a restored workload that
			// dies after a restart keeps its "running" record until the
			// next one.
			go m.watchAdopted(workload.Spec.ID, workload.Process.ProcStartTicks, workload.Process.PID)
			continue
		}
		workload.Process = nil
		if workload.Status == model.WorkloadRunning || workload.Status == model.WorkloadPaused || workload.Status == model.WorkloadStarting {
			workload.Status = model.WorkloadFailed
			workload.LastError = "recorded process is no longer running"
		} else {
			workload.Status = model.WorkloadStopped
		}
		workload.Generation++
		if err := m.records.Put(workload.Spec.ID, workload); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) LogReader(idOrName string, tailLines int) (io.ReadCloser, error) {
	workload, err := m.Get(idOrName)
	if err != nil {
		return nil, err
	}
	path := workloadLogPath(workload.Spec)
	if _, statErr := os.Stat(path); statErr != nil {
		// The root could not hold the log, so the workload appends to the
		// agent's state directory instead.
		path = m.stateLogPath(workload.Spec.ID)
	}
	if tailLines <= 0 {
		return os.Open(path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	lines := make([]string, 0, tailLines)
	for scanner.Scan() {
		if len(lines) == tailLines {
			copy(lines, lines[1:])
			lines[len(lines)-1] = scanner.Text()
		} else {
			lines = append(lines, scanner.Text())
		}
	}
	_ = file.Close()
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(strings.Join(lines, "\n") + "\n")), nil
}

func (m *Manager) wait(id string, startTicks uint64, command *exec.Cmd, logFile *os.File) {
	err := command.Wait()
	_ = logFile.Close()
	m.mu.Lock()
	delete(m.commands, id)
	m.mu.Unlock()
	m.recordExit(id, startTicks, err)
}

// recordExit is the one place a workload's death becomes record state. Both
// supervision paths end here — the waiter for processes the manager spawned
// and the adopted-process watcher for trees CRIU restored — so a restored
// workload's crash is recorded exactly like a spawned one's instead of
// lingering as "running" until the next agent restart.
func (m *Manager) recordExit(id string, startTicks uint64, exitErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workload, getErr := m.records.Get(id)
	if getErr != nil || workload.Process == nil || workload.Process.ProcStartTicks != startTicks {
		return
	}
	retired := m.stopping[id]
	delete(m.stopping, id)
	exitCode := 0
	if exitErr != nil {
		var exitError *exec.ExitError
		if errors.As(exitErr, &exitError) {
			exitCode = exitError.ExitCode()
		} else {
			exitCode = -1
		}
	}
	// A process that ended without the agent asking for it did not stop — it
	// died: an OOM kill inside its cgroup, a crash, a signal from outside.
	// Reporting that as "stopped" would hide the failure from the operator
	// exactly when the machine's own limits did their job; it is a failure,
	// with the kernel's reason as the record's error. A retirement the agent
	// itself ordered — the operator's stop, a superseded source a committed
	// restore reaps — is an ordinary stop, and a clean exit needs no error.
	workload.LastError = ""
	if exitErr != nil && !retired {
		workload.Status = model.WorkloadFailed
		workload.LastError = exitErr.Error()
	} else {
		workload.Status = model.WorkloadStopped
	}
	workload.Process = nil
	workload.Generation++
	if putErr := m.records.Put(id, workload); putErr != nil {
		m.logger.Error("persist workload exit", "workload_id", id, "error", putErr)
	}
	m.removeCgroup(id)
	m.logger.Info("workload exited", "workload_id", id, "exit_code", exitCode, "status", workload.Status)
}

// workloadLogPath is where a workload's stdout and stderr are appended: inside
// its own root, under .shift, so the descriptor's path is part of the
// filesystem that travels with the workload. A process restored on another
// machine reopens the log there — with the history written before the
// checkpoint still in it — instead of failing on a path that only exists on
// the machine that started it. The root is switched into place before CRIU
// restores the process, so by the time CRIU resolves the descriptor's path
// the file exists at the very location the checkpoint recorded.
func workloadLogPath(spec model.WorkloadSpec) string {
	return filepath.Join(spec.RootPath, ".shift", "logs", spec.ID+".log")
}

// stateLogPath is the fallback for a workload whose root cannot hold the log.
// An agent that cannot write into the root also cannot checkpoint it, so the
// descriptor's path never has to cross a machine boundary and the agent's
// state directory is enough.
func (m *Manager) stateLogPath(id string) string {
	return filepath.Join(m.stateDir, "logs", id+".log")
}

// openWorkloadLog opens the log a workload appends to, preferring the in-root
// location and falling back to the agent's state directory when the root
// cannot be written.
func (m *Manager) openWorkloadLog(spec model.WorkloadSpec) (*os.File, error) {
	directory := filepath.Join(spec.RootPath, ".shift", "logs")
	if err := os.MkdirAll(directory, 0o700); err == nil {
		if file, openErr := os.OpenFile(workloadLogPath(spec), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); openErr == nil {
			return file, nil
		}
	}
	if err := os.MkdirAll(filepath.Join(m.stateDir, "logs"), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(m.stateLogPath(spec.ID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

func resolveExecutable(command string) (string, error) {
	if filepath.IsAbs(command) {
		info, err := os.Stat(command)
		if err != nil {
			return "", err
		}
		if info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("command %s is not executable", command)
		}
		return command, nil
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("resolve command %q: %w", command, err)
	}
	return resolved, nil
}

// workloadBaseEnvironment lists the only variables a workload inherits from
// the agent's own environment. Everything else the agent runs with is
// withheld: the environment carries the machines-scope API key (ownership
// acquire and fence powers across the organization) and the storage
// credentials, and a workload that can read its own /proc/self/environ can
// exercise all of them. The spec's Environment is the explicitly scoped
// state that does move; nothing else does.
var workloadBaseEnvironment = []string{"PATH", "HOME", "TZ", "LANG"}

// defaultWorkloadPath is the PATH a workload gets when neither the agent nor
// the spec supplies one, so a minimal environment is still a runnable one.
const defaultWorkloadPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// isAgentCredentialEnv reports whether an environment variable carries an
// agent credential or agent configuration. Such variables are scrubbed from
// the workload environment even when a spec asks for them: the spec's
// environment crosses a trust boundary on restore (checkpoint manifests are
// hostile input), and no workload — however created — may end up holding an
// agent key. CRIU images capture the environment of the process they dump,
// so anything that leaked in here would be baked into every checkpoint and
// shipped to peer machines and object storage from there.
func isAgentCredentialEnv(key string) bool {
	upper := strings.ToUpper(key)
	if strings.HasPrefix(upper, "SHIFT_") {
		return true
	}
	switch upper {
	case "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_SECURITY_TOKEN",
		"S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY", "S3_SESSION_TOKEN":
		return true
	}
	return false
}

// mergedEnvironment builds the environment a workload is spawned with: the
// minimal inherited base, overlaid with the spec's explicitly declared
// variables, minus every agent credential. The agent's own environment is
// never inherited wholesale.
func mergedEnvironment(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, key := range workloadBaseEnvironment {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	if _, ok := values["PATH"]; !ok {
		values["PATH"] = defaultWorkloadPath
	}
	for key, value := range overrides {
		if strings.TrimSpace(key) == "" || isAgentCredentialEnv(key) {
			continue
		}
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func waitForStartTicks(pid int) (uint64, error) {
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		value, err := linuxplatform.ProcessStartTicks(pid)
		if err == nil {
			return value, nil
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	return 0, fmt.Errorf("read process identity for pid %s: %w", strconv.Itoa(pid), lastErr)
}

// verifyRestoredCredentials reconciles a restored process's real credentials
// — read back from the kernel, never from the checkpoint — with the identity
// the manifest declared and the destination policy admitted. The uid/gid
// boundary is the whole privilege story of a restore: a mismatch means the
// images carried a process of another identity than the manifest sold, and
// the restore must not stand.
func verifyRestoredCredentials(pid int, workload model.WorkloadSpec) error {
	content, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return fmt.Errorf("cannot verify the restored process's credentials: %w", err)
	}
	uid, gid, err := parseProcessCredentials(string(content))
	if err != nil {
		return fmt.Errorf("cannot verify the restored process's credentials: %w", err)
	}
	if uid != workload.UID || gid != workload.GID {
		return fmt.Errorf("restored process %d runs as uid %d gid %d but the checkpoint manifest declares uid %d gid %d; refusing to adopt it",
			pid, uid, gid, workload.UID, workload.GID)
	}
	return nil
}

// parseProcessCredentials extracts the real uid and gid from a
// /proc/<pid>/status document ("Uid:\treal\teffective\tsaved\tfs", likewise
// Gid).
func parseProcessCredentials(status string) (int, int, error) {
	uid, gid := -1, -1
	for _, line := range strings.Split(status, "\n") {
		key, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		switch key {
		case "Uid":
			uid, _ = strconv.Atoi(fields[0])
		case "Gid":
			gid, _ = strconv.Atoi(fields[0])
		}
	}
	if uid < 0 || gid < 0 {
		return 0, 0, errors.New("no Uid/Gid lines in process status")
	}
	return uid, gid, nil
}
