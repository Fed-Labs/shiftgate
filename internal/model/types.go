package model

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	StateFormatName    = "shift-state"
	StateFormatVersion = "1.0.0"
	APIVersion         = "v1"
)

// The version numbers a build advertises about itself. They are integers rather
// than semantic versions because they are compared, not displayed: an update is
// only applied when the release that would replace this build can still speak to
// the same control plane and still read the configuration and state this build
// has already written to disk.
const (
	// ProtocolVersion is the agent-to-control-plane and agent-to-agent wire
	// protocol this build speaks.
	ProtocolVersion = 1
	// MinimumProtocolVersion is the oldest protocol version this build still
	// understands from a peer or a control plane.
	MinimumProtocolVersion = 1
	// ConfigSchemaVersion is the on-disk configuration schema this build reads
	// and writes. It is the value of the "version" field in an agent
	// configuration file.
	ConfigSchemaVersion = 1
	// StateSchemaVersion is the on-disk state schema this build reads and
	// writes, covering workload records, checkpoints, and transfer sessions.
	StateSchemaVersion = 1
)

// ProtocolVersionHeader is the response header through which a control plane
// tells an agent which protocol version it speaks. An agent uses it to refuse an
// update that would leave it unable to report in.
const ProtocolVersionHeader = "Shift-Protocol-Version"

type StateClass string

const (
	StatePortable        StateClass = "portable"
	StateMachineSpecific StateClass = "machine_specific"
	StateExternal        StateClass = "external"
)

type WorkloadStatus string

const (
	WorkloadRegistered   WorkloadStatus = "registered"
	WorkloadStarting     WorkloadStatus = "starting"
	WorkloadRunning      WorkloadStatus = "running"
	WorkloadPaused       WorkloadStatus = "paused"
	WorkloadCheckpointed WorkloadStatus = "checkpointed"
	WorkloadRestoring    WorkloadStatus = "restoring"
	WorkloadStopped      WorkloadStatus = "stopped"
	WorkloadFailed       WorkloadStatus = "failed"
)

type WorkloadSpec struct {
	ID               string                `json:"id"`
	Name             string                `json:"name"`
	Command          []string              `json:"command"`
	RootPath         string                `json:"root_path"`
	WorkingDir       string                `json:"working_dir"`
	Environment      map[string]string     `json:"environment,omitempty"`
	UID              int                   `json:"uid"`
	GID              int                   `json:"gid"`
	Paths            []PathSpec            `json:"paths"`
	Ports            []PortSpec            `json:"ports,omitempty"`
	HealthCheck      *HealthCheckSpec      `json:"health_check,omitempty"`
	Resources        ResourceRequirements  `json:"resources"`
	DevicePolicy     DevicePolicy          `json:"device_policy"`
	NetworkPolicy    NetworkPolicy         `json:"network_policy"`
	CheckpointPolicy *CheckpointPolicySpec `json:"checkpoint_policy,omitempty"`
	FailoverPolicy   *FailoverPolicySpec   `json:"failover_policy,omitempty"`
	Lineage          *Lineage              `json:"lineage,omitempty"`
	CreatedAt        time.Time             `json:"created_at"`
	UpdatedAt        time.Time             `json:"updated_at"`
}

// CheckpointPolicySpec schedules agent-side periodic checkpoints for a
// workload: a full checkpoint every IntervalSeconds while it runs, and — when
// KeepLast is set — pruning of the workload's oldest checkpoints down to that
// count. A policy checkpoint is a full checkpoint on purpose: every retained
// snapshot is independently restorable, and the operator's incremental
// checkpoints keep working because a leave-running dump arms CRIU's memory
// tracker.
type CheckpointPolicySpec struct {
	// IntervalSeconds is the spacing between checkpoints, measured from the
	// last checkpoint (or the workload's creation when none exists yet). The
	// minimum is 10 seconds: every checkpoint briefly freezes the workload,
	// and a shorter interval would have it spend most of its time frozen.
	IntervalSeconds int `json:"interval_seconds"`
	// KeepLast caps how many of the workload's checkpoints to retain. Zero
	// keeps everything — pruning is deliberately opt-in. A checkpoint that a
	// retained checkpoint's lineage still needs is never pruned, even when
	// that leaves more than KeepLast behind.
	KeepLast int `json:"keep_last,omitempty"`
}

// Validate rejects a policy the scheduler could not honor. A nil policy — no
// scheduling — is valid.
func (p *CheckpointPolicySpec) Validate() error {
	if p == nil {
		return nil
	}
	if p.IntervalSeconds < 10 {
		return errors.New("checkpoint interval must be at least 10 seconds")
	}
	if p.KeepLast < 0 {
		return errors.New("checkpoint keep_last cannot be negative")
	}
	return nil
}

// FailoverPolicySpec designates a warm standby for a workload. The workload's
// newest root checkpoint — which the checkpoint policy this spec requires
// keeps arriving on schedule — is replicated to the standby agent over the
// mutually authenticated peer channel together with the workload key, so the
// standby holds a restorable copy it did not have to ask for. Failover never
// happens on the source's word alone: the standby restores only when the
// source's death is confirmed — its peer listener is unreachable AND the
// control plane reports it offline — or an operator explicitly commands the
// failover.
type FailoverPolicySpec struct {
	// AgentURL is the standby agent's peer listener — the same kind of https
	// URL a migration destination uses.
	AgentURL string `json:"agent_url"`
	// MachineID optionally pins the standby's identity. When set, replication
	// refuses to deposit state on a machine whose identity does not match,
	// so a mistyped address cannot silently land on the wrong host.
	MachineID string `json:"machine_id,omitempty"`
	// KeepLast caps how many of the workload's replicated checkpoints the
	// standby retains. Zero keeps everything there too.
	KeepLast int `json:"keep_last,omitempty"`
}

// Validate rejects a failover policy the replicator could not honor. A nil
// policy — no standby — is valid.
func (p *FailoverPolicySpec) Validate() error {
	if p == nil {
		return nil
	}
	parsed, err := url.Parse(p.AgentURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("failover standby agent URL must be a plain https URL")
	}
	if p.KeepLast < 0 {
		return errors.New("failover keep_last cannot be negative")
	}
	return nil
}

// Lineage records the exact state a workload was derived from. It is set when a
// workload is forked from another workload's checkpoint and is carried inside
// checkpoint manifests so that a future state-merge implementation can locate
// the common ancestor from recorded facts instead of inferring one. SHIFT does
// not implement state merging; this type only preserves the information a merge
// would require.
type Lineage struct {
	SourceWorkloadID   string    `json:"source_workload_id"`
	SourceCheckpointID string    `json:"source_checkpoint_id"`
	SourceRootPath     string    `json:"source_root_path,omitempty"`
	Generation         uint32    `json:"generation"`
	ForkedAt           time.Time `json:"forked_at"`
}

func (l *Lineage) Validate() error {
	if l == nil {
		return nil
	}
	if l.SourceWorkloadID == "" {
		return errors.New("lineage requires a source workload id")
	}
	if l.SourceCheckpointID == "" {
		return errors.New("lineage requires a source checkpoint id")
	}
	if l.ForkedAt.IsZero() {
		return errors.New("lineage requires a fork timestamp")
	}
	return nil
}

func (s *WorkloadSpec) Normalize() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("workload name is required")
	}
	if len(s.Command) == 0 || strings.TrimSpace(s.Command[0]) == "" {
		return errors.New("workload command is required")
	}
	if s.ID == "" {
		id, err := NewID()
		if err != nil {
			return err
		}
		s.ID = id
	}
	if s.RootPath == "" {
		return errors.New("root path is required; SHIFT will not infer an unrestricted home-directory capture")
	}
	absRoot, err := filepath.Abs(s.RootPath)
	if err != nil {
		return fmt.Errorf("resolve root path: %w", err)
	}
	s.RootPath = filepath.Clean(absRoot)
	if s.RootPath == string(filepath.Separator) {
		return errors.New("capturing the filesystem root is forbidden")
	}
	if s.WorkingDir == "" {
		s.WorkingDir = s.RootPath
	}
	absWorking, err := filepath.Abs(s.WorkingDir)
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	s.WorkingDir = filepath.Clean(absWorking)
	if !pathWithin(s.RootPath, s.WorkingDir) {
		return errors.New("working directory must be within the workload root")
	}
	if len(s.Paths) == 0 {
		s.Paths = []PathSpec{{Path: s.RootPath, Mode: PathReadWrite}}
	}
	for i := range s.Paths {
		if err := s.Paths[i].Normalize(s.RootPath); err != nil {
			return fmt.Errorf("path %d: %w", i, err)
		}
	}
	if s.Environment == nil {
		s.Environment = make(map[string]string)
	}
	if s.DevicePolicy == "" {
		s.DevicePolicy = DeviceRejectIncompatible
	}
	if s.NetworkPolicy == "" {
		s.NetworkPolicy = NetworkReconnect
	}
	if err := s.CheckpointPolicy.Validate(); err != nil {
		return fmt.Errorf("checkpoint policy: %w", err)
	}
	if err := s.FailoverPolicy.Validate(); err != nil {
		return fmt.Errorf("failover policy: %w", err)
	}
	// A failover policy without a checkpoint policy would replicate nothing:
	// the replicator follows the newest root checkpoint, and without a
	// schedule no new one arrives. Refusing here is the honest answer — a
	// workload whose standby silently held nothing would fail over into a
	// state far older than the operator believes exists.
	if s.FailoverPolicy != nil && s.CheckpointPolicy == nil {
		return errors.New("failover policy requires a checkpoint policy: without periodic checkpoints nothing would replicate to the standby")
	}
	if err := s.Lineage.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	return nil
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

type PathMode string

const (
	PathReadOnly  PathMode = "read_only"
	PathReadWrite PathMode = "read_write"
)

type PathSpec struct {
	Path       string   `json:"path"`
	Mode       PathMode `json:"mode"`
	OneFS      bool     `json:"one_filesystem"`
	Exclusions []string `json:"exclusions,omitempty"`
}

func (p *PathSpec) Normalize(root string) error {
	if p.Path == "" {
		return errors.New("path is required")
	}
	abs, err := filepath.Abs(p.Path)
	if err != nil {
		return err
	}
	p.Path = filepath.Clean(abs)
	if !pathWithin(root, p.Path) {
		return fmt.Errorf("%q is outside workload root %q", p.Path, root)
	}
	if p.Mode == "" {
		p.Mode = PathReadWrite
	}
	if p.Mode != PathReadOnly && p.Mode != PathReadWrite {
		return fmt.Errorf("unsupported path mode %q", p.Mode)
	}
	for _, pattern := range p.Exclusions {
		if filepath.IsAbs(pattern) || strings.Contains(pattern, "..") {
			return fmt.Errorf("unsafe exclusion %q", pattern)
		}
	}
	return nil
}

type PortSpec struct {
	Protocol      string `json:"protocol"`
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port,omitempty"`
}

type HealthCheckSpec struct {
	Type       string        `json:"type"`
	Address    string        `json:"address,omitempty"`
	Command    []string      `json:"command,omitempty"`
	Interval   time.Duration `json:"interval"`
	Timeout    time.Duration `json:"timeout"`
	Retries    int           `json:"retries"`
	StartDelay time.Duration `json:"start_delay"`
}

type ResourceRequirements struct {
	CPUCount     float64     `json:"cpu_count,omitempty"`
	MemoryBytes  uint64      `json:"memory_bytes,omitempty"`
	StorageBytes uint64      `json:"storage_bytes,omitempty"`
	PIDs         uint64      `json:"pids,omitempty"`
	GPUs         []GPUDevice `json:"gpus,omitempty"`
}

type DevicePolicy string

const (
	DeviceRejectIncompatible DevicePolicy = "reject_incompatible"
	DeviceWarnIncompatible   DevicePolicy = "warn_incompatible"
)

type NetworkPolicy string

const (
	NetworkPreserve  NetworkPolicy = "preserve"
	NetworkReconnect NetworkPolicy = "reconnect"
	NetworkDrain     NetworkPolicy = "drain"
)

// VirtualIdentity is the address a workload keeps while it moves. It is
// derived from the workload id, not allocated from a registry, so every
// machine restores the same workload to the same address without a
// coordinator. The address lives in the RFC 6598 carrier-grade NAT range: it
// is an identifier, never a route, so it cannot collide with a machine's
// real networks.
type VirtualIdentity struct {
	IP           string `json:"ip"`
	PrefixLength int    `json:"prefix_length"`
}

// PortMapping is one resolved NAT rule: traffic arriving on the host port is
// delivered to the workload's listener on the container port. When the two are
// equal the restored process binds the port itself and no forwarder runs; the
// mapping still reserves the port against other workloads.
type PortMapping struct {
	Protocol      string `json:"protocol"`
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port"`
}

// SocketDisposition states what actually happened to one listener's sockets.
type SocketDisposition string

const (
	// SocketPreserved means the socket state itself traveled with the
	// checkpoint and the destination restored it. Only a validated restore
	// on a compatible kernel earns this value.
	SocketPreserved SocketDisposition = "preserved"
	// SocketRecreated means the listener was re-established at the
	// destination and the connections it held at checkpoint time were
	// dropped; peers must reconnect.
	SocketRecreated SocketDisposition = "recreated"
)

// NetworkPortStatus is the plan's verdict for one declared port.
type NetworkPortStatus struct {
	Mapping     PortMapping       `json:"mapping"`
	Disposition SocketDisposition `json:"disposition"`
	// Mechanism says how the listener comes back: the restored process
	// binds it itself ("direct") or a SHIFT forwarder carries traffic to it
	// ("forwarded").
	Mechanism string `json:"mechanism"`
}

// NetworkPlan is what SHIFT intends to do with a workload's network across a
// migration, computed before anything moves. It is carried on the migration
// record so the events, the CLI, and the status document a restored
// application reads all tell the same story.
type NetworkPlan struct {
	Policy   NetworkPolicy       `json:"policy"`
	Identity VirtualIdentity     `json:"identity"`
	Ports    []NetworkPortStatus `json:"ports"`
	// SocketsCarried reports that the checkpoint will include TCP socket
	// state (policy preserve). Carrying it is an attempt; the status
	// document written after restore records whether it held.
	SocketsCarried bool `json:"sockets_carried"`
	// ConnectionsDropped reports that live connections will not survive
	// and peers must reconnect.
	ConnectionsDropped bool `json:"connections_dropped"`
	// DrainBeforeCheckpoint reports that established connections at the
	// source are to be drained before the checkpoint is taken.
	DrainBeforeCheckpoint bool   `json:"drain_before_checkpoint"`
	Summary               string `json:"summary"`
}

// virtualNetworkRange is the RFC 6598 carrier-grade NAT space virtual
// identities are drawn from. It is parsed once at package initialization and
// never mutated.
var virtualNetworkRange = func() *net.IPNet {
	_, network, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		panic("model: " + err.Error())
	}
	return network
}()

// Validate checks that a virtual identity read from a manifest or migration
// record is well-formed and inside the carrier-grade NAT range identities are
// drawn from.
func (identity VirtualIdentity) Validate() error {
	ip := net.ParseIP(identity.IP)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("virtual network identity %q is not an IPv4 address", identity.IP)
	}
	if !virtualNetworkRange.Contains(ip) {
		return fmt.Errorf("virtual network identity %q is outside the %s range", identity.IP, virtualNetworkRange)
	}
	if identity.PrefixLength != 10 {
		return fmt.Errorf("virtual network identity prefix length must be 10, got %d", identity.PrefixLength)
	}
	return nil
}

// Direct reports whether the workload listens on the host port itself. A
// process restored into the host network namespace rebinds its own port, so a
// mapping onto the same port needs no forwarder — only the reservation.
func (mapping PortMapping) Direct() bool {
	return mapping.HostPort == mapping.ContainerPort
}

// String renders the mapping the way an operator would read it.
func (mapping PortMapping) String() string {
	if mapping.Direct() {
		return fmt.Sprintf("%s %d", mapping.Protocol, mapping.HostPort)
	}
	return fmt.Sprintf("%s %d→%d", mapping.Protocol, mapping.HostPort, mapping.ContainerPort)
}

type ProcessState struct {
	PID            int       `json:"pid"`
	PGID           int       `json:"pgid"`
	ProcStartTicks uint64    `json:"proc_start_ticks"`
	StartedAt      time.Time `json:"started_at"`
	ExitCode       *int      `json:"exit_code,omitempty"`
}

type Workload struct {
	Spec               WorkloadSpec   `json:"spec"`
	Status             WorkloadStatus `json:"status"`
	Process            *ProcessState  `json:"process,omitempty"`
	LatestCheckpointID string         `json:"latest_checkpoint_id,omitempty"`
	Generation         uint64         `json:"generation"`
	LastError          string         `json:"last_error,omitempty"`
}

type MachineIdentity struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	PublicKeyPEM string `json:"public_key_pem"`
}

type MachineCapabilities struct {
	MachineID    string           `json:"machine_id"`
	Hostname     string           `json:"hostname"`
	OS           string           `json:"os"`
	Distribution string           `json:"distribution"`
	Kernel       string           `json:"kernel"`
	Architecture string           `json:"architecture"`
	CPUs         int              `json:"cpus"`
	MemoryBytes  uint64           `json:"memory_bytes"`
	Storage      []StorageDevice  `json:"storage"`
	GPUs         []GPUDevice      `json:"gpus"`
	GPURuntimes  []GPURuntime     `json:"gpu_runtimes,omitempty"`
	CRIU         CRIUCapabilities `json:"criu"`
	Features     map[string]bool  `json:"features"`
	AgentVersion string           `json:"agent_version"`
	ObservedAt   time.Time        `json:"observed_at"`
}

func CurrentPlatformSupported() bool {
	return runtime.GOOS == "linux" && runtime.GOARCH == "amd64"
}

type StorageDevice struct {
	Mountpoint     string `json:"mountpoint"`
	Filesystem     string `json:"filesystem"`
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

// GPUDevice is one accelerator a machine advertises or a workload requires.
// A requirement is matched by vendor, compute capability, and memory — a
// stricter model match would make placement impossible on heterogeneous
// fleets, so a model difference is surfaced as a compatibility warning by the
// checker instead of a hard failure here.
type GPUDevice struct {
	Vendor            string   `json:"vendor"`
	Model             string   `json:"model"`
	DriverVersion     string   `json:"driver_version,omitempty"`
	MemoryBytes       uint64   `json:"memory_bytes,omitempty"`
	ComputeCapability string   `json:"compute_capability,omitempty"`
	Runtime           string   `json:"runtime,omitempty"`
	CheckpointRestore bool     `json:"checkpoint_restore"`
	DeviceFiles       []string `json:"device_files,omitempty"`
}

// GPURuntime is a GPU software environment installed on a machine: the CUDA
// toolkit or a ROCm installation. A workload's process usually carries its own
// runtime libraries inside its root; what the machine must provide is the
// driver, which GPUDevice already records. The runtime entry exists so
// destinations can be compared and operators can see what is installed.
type GPURuntime struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	ToolkitPath string `json:"toolkit_path,omitempty"`
	Installed   bool   `json:"installed"`
}

type CRIUCapabilities struct {
	Installed bool     `json:"installed"`
	Version   string   `json:"version,omitempty"`
	Healthy   bool     `json:"healthy"`
	Features  []string `json:"features,omitempty"`
	Errors    []string `json:"errors,omitempty"`
}

type StateItem struct {
	Name        string     `json:"name"`
	Class       StateClass `json:"class"`
	Adapter     string     `json:"adapter,omitempty"`
	Required    bool       `json:"required"`
	Description string     `json:"description,omitempty"`
}

// FileDependency is one file outside the process image that a workload's
// command needs in order to run: its binary, its ELF interpreter, or a shared
// library. Dependencies inside the workload root travel with the checkpoint;
// dependencies outside it do not, and a destination without them cannot run
// the workload — which is exactly why they are recorded rather than guessed.
type FileDependency struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	InsideRoot bool   `json:"inside_root"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
}

// FilesystemCapture records how a checkpoint's filesystem asset was taken:
// what filesystem the root lives on, and whether the bytes came from a
// point-in-time snapshot or from the root itself while the workload was
// frozen. It exists so the manifest never implies a stronger consistency
// guarantee than the machine actually provided.
type FilesystemCapture struct {
	Filesystem string `json:"filesystem"`
	Snapshot   string `json:"snapshot,omitempty"`
	// FrozenCapture is true when the archive was read straight from the
	// workload root while the process was stopped — SHIFT's guarantee on
	// filesystems without snapshots — rather than from a snapshot.
	FrozenCapture bool `json:"frozen_capture"`
	// ChangedFiles, AddedFiles, and DeletedFiles count the file-level
	// difference from the workload's previous checkpoint, when one exists.
	ChangedFiles int `json:"changed_files,omitempty"`
	AddedFiles   int `json:"added_files,omitempty"`
	DeletedFiles int `json:"deleted_files,omitempty"`
}

type CheckpointKind string

const (
	CheckpointFull        CheckpointKind = "full"
	CheckpointIncremental CheckpointKind = "incremental"
)

type ChunkRef struct {
	Address      string `json:"address"`
	KeyVersion   uint32 `json:"key_version"`
	PlainSize    int64  `json:"plain_size"`
	StoredSize   int64  `json:"stored_size"`
	CipherSHA256 string `json:"cipher_sha256"`
	PlainSHA256  string `json:"plain_sha256"`
	Sequence     int    `json:"sequence"`
	Compression  string `json:"compression"`
	Encryption   string `json:"encryption"`
}

type AssetManifest struct {
	Name         string     `json:"name"`
	MediaType    string     `json:"media_type"`
	PlainSize    int64      `json:"plain_size"`
	StoredSize   int64      `json:"stored_size"`
	SHA256       string     `json:"sha256"`
	Chunks       []ChunkRef `json:"chunks"`
	Required     bool       `json:"required"`
	RestoreOrder int        `json:"restore_order"`
}

type CheckpointEngineInfo struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	LeaveRunning bool   `json:"leave_running"`
	ParentImages bool   `json:"parent_images"`
	PreCopy      bool   `json:"pre_copy,omitempty"`
	// PreCopyPasses is how many pre-dump iterations actually ran while the
	// workload kept running — the honest count, not the requested cap.
	PreCopyPasses int  `json:"pre_copy_passes,omitempty"`
	TCPState      bool `json:"tcp_state"`
	ShellJob      bool `json:"shell_job"`
	FileLocks     bool `json:"file_locks"`
}

type SecurityEnvelope struct {
	KeyVersion        uint32 `json:"key_version"`
	ManifestSignature string `json:"manifest_signature"`
	SignerMachineID   string `json:"signer_machine_id"`
	Algorithm         string `json:"algorithm"`
}

type CheckpointMetrics struct {
	StartedAt time.Time `json:"started_at"`
	// FreezeStartedAt is the instant the workload actually stopped
	// executing — the pause before the final dump. It is zero when nothing
	// was frozen (the workload was already stopped or paused). Downtime is
	// measured from here rather than from StartedAt: pre-copy passes and
	// changed-file accounting run while the workload is still live.
	FreezeStartedAt   time.Time     `json:"freeze_started_at,omitzero"`
	CompletedAt       time.Time     `json:"completed_at"`
	Duration          time.Duration `json:"duration"`
	PlainBytes        int64         `json:"plain_bytes"`
	StoredBytes       int64         `json:"stored_bytes"`
	DeduplicatedBytes int64         `json:"deduplicated_bytes"`
	ChunkCount        int           `json:"chunk_count"`
}

type CheckpointManifest struct {
	Format          string               `json:"format"`
	FormatVersion   string               `json:"format_version"`
	ID              string               `json:"id"`
	ParentID        string               `json:"parent_id,omitempty"`
	Kind            CheckpointKind       `json:"kind"`
	Workload        WorkloadSpec         `json:"workload"`
	SourceIdentity  MachineIdentity      `json:"source_identity"`
	SourceMachine   MachineCapabilities  `json:"source_machine"`
	CreatedAt       time.Time            `json:"created_at"`
	Engine          CheckpointEngineInfo `json:"engine"`
	Assets          []AssetManifest      `json:"assets"`
	StateInventory  []StateItem          `json:"state_inventory"`
	Filesystem      FilesystemCapture    `json:"filesystem_capture"`
	Dependencies    []FileDependency     `json:"dependencies,omitempty"`
	DeviceNeeds     []GPUDevice          `json:"device_requirements,omitempty"`
	RequiredBytes   int64                `json:"required_bytes"`
	Metrics         CheckpointMetrics    `json:"metrics"`
	Security        SecurityEnvelope     `json:"security"`
	ManifestSHA256  string               `json:"manifest_sha256"`
	CompatibilityID string               `json:"compatibility_id"`
}

type CompatibilityIssue struct {
	Code        string `json:"code"`
	Severity    string `json:"severity"`
	Resource    string `json:"resource"`
	Description string `json:"description"`
	Adaptation  string `json:"adaptation,omitempty"`
}

type CompatibilityReport struct {
	Compatible bool                 `json:"compatible"`
	Issues     []CompatibilityIssue `json:"issues"`
	CheckedAt  time.Time            `json:"checked_at"`
}

type MigrationStage string

const (
	MigrationCreated      MigrationStage = "CREATED"
	MigrationDiscover     MigrationStage = "DISCOVER"
	MigrationValidate     MigrationStage = "VALIDATE"
	MigrationSnapshot     MigrationStage = "SNAPSHOT"
	MigrationPrepare      MigrationStage = "PREPARE"
	MigrationTransfer     MigrationStage = "TRANSFER"
	MigrationVerify       MigrationStage = "VERIFY"
	MigrationRestore      MigrationStage = "RESTORE"
	MigrationPostValidate MigrationStage = "POST_VALIDATE"
	MigrationSwitch       MigrationStage = "SWITCH"
	MigrationCommit       MigrationStage = "COMMIT"
	MigrationCleanup      MigrationStage = "CLEANUP"
	MigrationCompleted    MigrationStage = "COMPLETED"
	MigrationFailed       MigrationStage = "FAILED"
	MigrationRollingBack  MigrationStage = "ROLLING_BACK"
	MigrationRolledBack   MigrationStage = "ROLLED_BACK"
	// The two-L spelling is the persisted stage value in existing migration
	// rows and the dashboard's type union — it cannot be "corrected".
	MigrationCancelled MigrationStage = "CANCELLED" //nolint:misspell // persisted stage value; the dashboard type union names it
)

// A failure at any pre-commit stage can enter ROLLING_BACK directly: a
// migration whose source is still being preserved must never be observable
// in FAILED, which every observer treats as terminal. FAILED is reserved
// for terminally failed migrations — a rollback that itself needs an
// operator, or a stage that cannot roll back.
var migrationTransitions = map[MigrationStage]map[MigrationStage]bool{
	MigrationCreated:  {MigrationDiscover: true, MigrationFailed: true, MigrationCancelled: true},
	MigrationDiscover: {MigrationValidate: true, MigrationRollingBack: true, MigrationFailed: true, MigrationCancelled: true},
	MigrationValidate: {MigrationSnapshot: true, MigrationRollingBack: true, MigrationFailed: true, MigrationCancelled: true},
	// A live migration opens the destination session inside its snapshot
	// stage — each pre-copy pass's images transfer as the pass completes — so
	// it moves straight from SNAPSHOT to TRANSFER; a cold migration creates
	// its whole checkpoint first and reserves between the two.
	MigrationSnapshot:     {MigrationPrepare: true, MigrationTransfer: true, MigrationRollingBack: true, MigrationFailed: true, MigrationCancelled: true},
	MigrationPrepare:      {MigrationTransfer: true, MigrationRollingBack: true, MigrationFailed: true, MigrationCancelled: true},
	MigrationTransfer:     {MigrationVerify: true, MigrationRollingBack: true, MigrationFailed: true, MigrationCancelled: true},
	MigrationVerify:       {MigrationRestore: true, MigrationRollingBack: true, MigrationFailed: true, MigrationCancelled: true},
	MigrationRestore:      {MigrationPostValidate: true, MigrationFailed: true, MigrationRollingBack: true, MigrationCancelled: true},
	MigrationPostValidate: {MigrationSwitch: true, MigrationRollingBack: true, MigrationFailed: true, MigrationCancelled: true},
	MigrationSwitch:       {MigrationCommit: true, MigrationRollingBack: true, MigrationFailed: true, MigrationCancelled: true},
	MigrationCommit:       {MigrationCleanup: true, MigrationRollingBack: true, MigrationFailed: true, MigrationCancelled: true},
	MigrationCleanup:      {MigrationCompleted: true, MigrationFailed: true, MigrationCancelled: true},
	MigrationFailed:       {MigrationRollingBack: true},
	MigrationRollingBack:  {MigrationRolledBack: true, MigrationFailed: true, MigrationCancelled: true},
}

func CanTransition(from, to MigrationStage) bool {
	return migrationTransitions[from][to]
}

type MigrationMode string

const (
	MigrationCold MigrationMode = "cold"
	MigrationLive MigrationMode = "live"
)

type Destination struct {
	MachineID  string `json:"machine_id"`
	AgentURL   string `json:"agent_url"`
	ServerName string `json:"server_name,omitempty"`
}

type MigrationMetrics struct {
	TotalStateBytes   int64 `json:"total_state_bytes"`
	TransferredBytes  int64 `json:"transferred_bytes"`
	DeduplicatedBytes int64 `json:"deduplicated_bytes"`
	// PreCopyTransferredBytes is the part of TransferredBytes a live
	// migration moved during its pre-copy passes — while the workload kept
	// running. The difference between the two fields is what had to travel
	// inside the frozen window.
	PreCopyTransferredBytes int64         `json:"pre_copy_transferred_bytes,omitempty"`
	CheckpointDuration      time.Duration `json:"checkpoint_duration"`
	RestoreDuration         time.Duration `json:"restore_duration"`
	Downtime                time.Duration `json:"downtime"`
	TransferDuration        time.Duration `json:"transfer_duration"`
	TransferBytesPerSec     float64       `json:"transfer_bytes_per_second"`
}

type MigrationEvent struct {
	Sequence   uint64         `json:"sequence"`
	Stage      MigrationStage `json:"stage"`
	Message    string         `json:"message"`
	Timestamp  time.Time      `json:"timestamp"`
	Progress   float64        `json:"progress"`
	BytesDone  int64          `json:"bytes_done,omitempty"`
	BytesTotal int64          `json:"bytes_total,omitempty"`
}

type Migration struct {
	ID              string        `json:"id"`
	WorkloadID      string        `json:"workload_id"`
	CheckpointID    string        `json:"checkpoint_id,omitempty"`
	SourceMachineID string        `json:"source_machine_id"`
	Destination     Destination   `json:"destination"`
	Mode            MigrationMode `json:"mode"`
	// PreCopyPasses caps the pre-dump iterations a live migration runs
	// before freezing the source (0 = the agent's default policy). The
	// checkpoint manifest records how many actually ran.
	PreCopyPasses   int                 `json:"pre_copy_passes,omitempty"`
	Stage           MigrationStage      `json:"stage"`
	Compatibility   CompatibilityReport `json:"compatibility"`
	Network         NetworkPlan         `json:"network"`
	Metrics         MigrationMetrics    `json:"metrics"`
	Events          []MigrationEvent    `json:"events"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
	CompletedAt     *time.Time          `json:"completed_at,omitempty"`
	FailureCode     string              `json:"failure_code,omitempty"`
	FailureReason   string              `json:"failure_reason,omitempty"`
	SourcePreserved bool                `json:"source_preserved"`
	Revision        uint64              `json:"revision"`
}

func (m *Migration) Transition(to MigrationStage, message string, progress float64) error {
	if !CanTransition(m.Stage, to) {
		return fmt.Errorf("invalid migration transition %s -> %s", m.Stage, to)
	}
	m.Stage = to
	m.UpdatedAt = time.Now().UTC()
	m.Revision++
	m.Events = append(m.Events, MigrationEvent{
		Sequence:  uint64(len(m.Events) + 1),
		Stage:     to,
		Message:   message,
		Timestamp: m.UpdatedAt,
		Progress:  progress,
	})
	if to == MigrationCompleted || to == MigrationRolledBack || to == MigrationCancelled {
		completed := m.UpdatedAt
		m.CompletedAt = &completed
	}
	return nil
}

// Standby duty states. Armed means the standby holds state and watches the
// source; failed_over means the workload was restored there and the duty is
// the failover's history.
const (
	StandbyArmed      = "armed"
	StandbyFailedOver = "failed_over"
)

// StandbyDuty is the standby agent's record of one workload it protects:
// what state it holds, from which source, and — after a failover — from what
// checkpoint the workload was restored there. The duty, not the transfer
// session that delivered the state, is the operational truth: sessions
// expire on their own clock, duties persist until withdrawn or failed over.
type StandbyDuty struct {
	WorkloadID       string    `json:"workload_id"`
	WorkloadName     string    `json:"workload_name,omitempty"`
	WorkloadUID      int       `json:"workload_uid,omitempty"`
	SourceMachineID  string    `json:"source_machine_id"`
	SourceAgentURL   string    `json:"source_agent_url,omitempty"`
	KeepLast         int       `json:"keep_last,omitempty"`
	LastCheckpointID string    `json:"last_checkpoint_id"`
	LastCheckpointAt time.Time `json:"last_checkpoint_at"`
	HeldAt           time.Time `json:"held_at"`
	UpdatedAt        time.Time `json:"updated_at"`

	// State is "armed" while the standby holds state and watches the source,
	// "failed_over" once the workload was restored here — after which the
	// duty is the failover's history, not an active watch.
	State             string    `json:"state"`
	FailoverAt        time.Time `json:"failover_at,omitempty"`
	FailoverRestoreID string    `json:"failover_restore_id,omitempty"`
	FailoverReason    string    `json:"failover_reason,omitempty"`

	LastFailoverError string    `json:"last_failover_error,omitempty"`
	NextAttemptAt     time.Time `json:"next_attempt_at,omitempty"`
}

// ReplicationEntry is the source agent's ledger record for one workload it
// replicates to a standby: what was pushed, to whom, and how the last
// attempt went. It is the source-side counterpart of the standby's duty.
type ReplicationEntry struct {
	WorkloadID       string    `json:"workload_id"`
	WorkloadName     string    `json:"workload_name,omitempty"`
	StandbyURL       string    `json:"standby_url"`
	StandbyMachineID string    `json:"standby_machine_id,omitempty"`
	KeepLast         int       `json:"keep_last,omitempty"`
	LastCheckpointID string    `json:"last_checkpoint_id,omitempty"`
	LastPushAt       time.Time `json:"last_push_at,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
	LastErrorAt      time.Time `json:"last_error_at,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type ErrorResponse struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
	Details   any    `json:"details,omitempty"`
}
