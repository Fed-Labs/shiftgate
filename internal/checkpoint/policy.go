package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"shift.dev/shift/internal/model"
)

// RestorePolicy is the destination-side admission policy for everything that
// materializes checkpoint content onto this machine's filesystem: restore,
// fork, and clone targets. A checkpoint is untrusted input — its manifest is
// signed by the source, and a compromised source signs hostile manifests
// legitimately — so every rule here is enforced on the destination and a zero
// policy fails closed: no target outside a workload root already registered on
// this machine is ever replaced.
//
// The operator pre-registers additional targets through
// <state-dir>/metadata/restore-policy.json (see LoadRestorePolicy).
type RestorePolicy struct {
	// AllowedRootPrefixes pre-registers the directory prefixes checkpoint
	// targets may live under when they are not already the registered root of
	// a workload on this machine. An empty list admits no such targets.
	AllowedRootPrefixes []string `json:"allowed_root_prefixes,omitempty"`
	// AllowRootWorkload authorizes restoring workloads whose manifest
	// declares UID 0. Without it a root-uid manifest is refused outright:
	// the restored process would otherwise run the manifest's command with
	// full privileges.
	AllowRootWorkload bool `json:"allow_root_workload,omitempty"`
	// AllowDeviceNodes keeps device nodes a checkpoint archive contains.
	// Without it they are refused during extraction.
	AllowDeviceNodes bool `json:"allow_device_nodes,omitempty"`
	// AllowSetuid keeps setuid/setgid bits a checkpoint archive carries.
	// Without it the bits are stripped during extraction.
	AllowSetuid bool `json:"allow_setuid,omitempty"`
	// AllowFileCaps keeps file capabilities (security.capability xattrs) a
	// checkpoint archive carries. Without it they are stripped during
	// extraction: a planted cap_setuid/cap_dac_override binary executed by
	// the restored workload would otherwise gain host-root power in the init
	// user namespace, the modern equivalent of the setuid escape.
	AllowFileCaps bool `json:"allow_file_caps,omitempty"`
	// AllowSystemTreeTargets is the explicit operator override for the
	// absolute system-tree refusal: a restore target under one of these
	// prefixes is admitted even though it sits inside a denied system or
	// user tree. Empty — the default — keeps the refusal absolute; the
	// override exists for the rare machine whose workload roots genuinely
	// live under /opt or /home and is set only by the operator's own policy
	// file, never by a manifest.
	AllowSystemTreeTargets []string `json:"allow_system_tree_targets,omitempty"`
}

// deniedTargetPrefixes are host system and user trees a checkpoint target may
// never replace, whatever any allowlist says. Replacing one of these as root
// is total compromise of the destination (or of user data), so the refusal is
// absolute unless the operator's own policy explicitly overrides it for
// concrete prefixes (RestorePolicy.AllowSystemTreeTargets).
var deniedTargetPrefixes = []string{
	"/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib32", "/lib64", "/libx32",
	"/media", "/mnt", "/opt", "/proc", "/root", "/run", "/sbin", "/snap", "/srv",
	"/sys", "/tmp", "/usr", "/var",
}

const maxPolicyBytes = int64(1 << 20)

// ErrRestoreTargetDenied marks every target-admission refusal so callers can
// treat them uniformly: no staging, no rollback, nothing touched.
var ErrRestoreTargetDenied = errors.New("restore target denied by destination policy")

// LoadRestorePolicy reads the operator's restore policy from
// <stateDir>/metadata/restore-policy.json. A missing file means the fail-
// closed zero policy. The file is operator authority, so it must be owned by
// root or the agent user and must not be group- or world-writable; anything
// else is refused rather than trusted. The open is O_NOFOLLOW and every
// validation runs against the opened descriptor — stat-then-read would leave
// a swap window where a permissive file passes validation while the bytes
// read belong to another.
func LoadRestorePolicy(stateDir string) (RestorePolicy, error) {
	path := filepath.Join(stateDir, "metadata", "restore-policy.json")
	// O_NONBLOCK keeps a FIFO planted at the path from wedging the open.
	descriptor, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ENOENT) {
		return RestorePolicy{}, nil
	}
	if errors.Is(err, syscall.ELOOP) {
		return RestorePolicy{}, fmt.Errorf("%w: restore policy must be a regular file, not a symlink", ErrRestoreTargetDenied)
	}
	if err != nil {
		return RestorePolicy{}, err
	}
	file := os.NewFile(uintptr(descriptor), path)
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return RestorePolicy{}, err
	}
	if !info.Mode().IsRegular() {
		return RestorePolicy{}, fmt.Errorf("%w: restore policy must be a regular file", ErrRestoreTargetDenied)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return RestorePolicy{}, fmt.Errorf("%w: restore policy file must not be group- or world-writable", ErrRestoreTargetDenied)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if euid := os.Geteuid(); stat.Uid != uint32(euid) && stat.Uid != 0 {
			return RestorePolicy{}, fmt.Errorf("%w: restore policy file must be owned by root or the agent user", ErrRestoreTargetDenied)
		}
	}
	encoded, err := io.ReadAll(io.LimitReader(file, maxPolicyBytes+1))
	if err != nil {
		return RestorePolicy{}, err
	}
	if int64(len(encoded)) > maxPolicyBytes {
		return RestorePolicy{}, fmt.Errorf("%w: restore policy file exceeds size limit", ErrRestoreTargetDenied)
	}
	var policy RestorePolicy
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return RestorePolicy{}, fmt.Errorf("decode restore policy: %w", err)
	}
	// Trailing bytes after the first JSON value are a corrupt or
	// partially-overwritten policy file; misreading it as valid would fail
	// open instead of closed, so they are refused like the repository's own
	// decodeJSON refuses them.
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return RestorePolicy{}, fmt.Errorf("%w: restore policy contains trailing JSON data", ErrRestoreTargetDenied)
		}
		return RestorePolicy{}, fmt.Errorf("decode restore policy: %w", err)
	}
	if err := policy.normalize(); err != nil {
		return RestorePolicy{}, err
	}
	return policy, nil
}

func (p *RestorePolicy) normalize() error {
	overrides := make([]string, 0, len(p.AllowSystemTreeTargets))
	for _, prefix := range p.AllowSystemTreeTargets {
		prefix = filepath.Clean(prefix)
		if !filepath.IsAbs(prefix) || prefix == string(filepath.Separator) {
			return fmt.Errorf("%w: system-tree override %q must be an absolute path other than the filesystem root", ErrRestoreTargetDenied, prefix)
		}
		overrides = append(overrides, prefix)
	}
	p.AllowSystemTreeTargets = overrides
	cleaned := make([]string, 0, len(p.AllowedRootPrefixes))
	for _, prefix := range p.AllowedRootPrefixes {
		prefix = filepath.Clean(prefix)
		if !filepath.IsAbs(prefix) || prefix == string(filepath.Separator) {
			return fmt.Errorf("%w: allowed root prefix %q must be an absolute path other than the filesystem root", ErrRestoreTargetDenied, prefix)
		}
		if p.denied(prefix) {
			return fmt.Errorf("%w: allowed root prefix %q is inside a system directory", ErrRestoreTargetDenied, prefix)
		}
		cleaned = append(cleaned, prefix)
	}
	p.AllowedRootPrefixes = cleaned
	return nil
}

// SetRestorePolicy overrides the loaded policy. Tests use it to authorize
// their temporary roots; production callers prefer the policy file.
func (s *Service) SetRestorePolicy(policy RestorePolicy) error {
	if err := policy.normalize(); err != nil {
		return err
	}
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	copy := policy
	s.restorePolicy = &copy
	return nil
}

func (s *Service) activeRestorePolicy() (RestorePolicy, error) {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	if s.restorePolicy != nil {
		return *s.restorePolicy, nil
	}
	policy, err := LoadRestorePolicy(s.stateDir)
	if err != nil {
		return RestorePolicy{}, err
	}
	s.restorePolicy = &policy
	return policy, nil
}

// registeredRoot returns the workload root this machine already knows the
// workload by, or "" when the workload is not registered here. Registration
// is operator action (workload creation), so a registered root is an
// operator-approved target without any extra configuration.
func (s *Service) registeredRoot(workloadID string) string {
	if workload, err := s.runtime.Get(workloadID); err == nil {
		return filepath.Clean(workload.Spec.RootPath)
	}
	return ""
}

// CheckRestoreTarget admits a restore target: the manifest root must be
// absolute and outside every system tree, it must either equal the root the
// workload is registered with on this machine or sit under an operator-
// registered prefix, and the on-disk directory it replaces must be a real
// directory owned by the workload's uid. A fresh target must sit in an
// existing real directory owned by the workload's uid or by root.
//
// The path is resolved without following a single symlink — a target like
// <allowed>/lnk/newdir with lnk -> /etc is refused where a lexical check
// would happily admit it — and the checks run on the resolved identity of
// the parent and the final component, not on the string the manifest names.
func (p RestorePolicy) CheckRestoreTarget(targetRoot string, workload model.WorkloadSpec, registeredRoot string) error {
	if err := p.CheckWorkload(workload); err != nil {
		return err
	}
	target := filepath.Clean(targetRoot)
	if err := p.checkTargetPath(target); err != nil {
		return err
	}
	dir, stats, err := inspectTarget(target)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if registeredRoot != "" {
		if target != registeredRoot {
			return fmt.Errorf("%w: checkpoint targets %s but the workload is registered on this destination with root %s", ErrRestoreTargetDenied, target, registeredRoot)
		}
	} else if !p.rootPrefixAllowed(target) {
		return fmt.Errorf("%w: %s is not the registered root of a workload on this machine and is not under an operator-registered restore root", ErrRestoreTargetDenied, target)
	}
	return p.checkTargetOwnership(stats, workload.UID)
}

// CheckNewRoot admits a brand-new root beside a known-good source root (fork
// and clone targets): outside every system tree, under an operator-registered
// prefix or beside the source root, with an existing parent owned by the
// workload's uid or by root, and not already present. The same symlink-free
// resolution as CheckRestoreTarget applies — a new root landing through a
// planted parent symlink is refused, not materialized.
func (p RestorePolicy) CheckNewRoot(targetRoot, sourceRoot string, workload model.WorkloadSpec) error {
	if err := p.CheckWorkload(workload); err != nil {
		return err
	}
	target := filepath.Clean(targetRoot)
	if err := p.checkTargetPath(target); err != nil {
		return err
	}
	source := filepath.Clean(sourceRoot)
	if err := p.checkTargetPath(source); err != nil {
		return err
	}
	dir, stats, err := inspectTarget(target)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if filepath.Dir(target) != filepath.Dir(source) && !p.rootPrefixAllowed(target) {
		return fmt.Errorf("%w: %s is neither beside the source workload root nor under an operator-registered restore root", ErrRestoreTargetDenied, target)
	}
	if stats.targetExists {
		return fmt.Errorf("%w: %s already exists", ErrRestoreTargetDenied, target)
	}
	return p.checkTargetOwnership(stats, workload.UID)
}

// CheckWorkload validates the identity a manifest declares for the restored
// process against destination policy: sane uid/gid, a non-empty command with
// no NUL bytes, and — unless the operator explicitly authorized root restores
// — a non-root uid. The uid here is what the process is started and health-
// checked under, so it is exactly the privilege boundary.
func (p RestorePolicy) CheckWorkload(workload model.WorkloadSpec) error {
	if workload.UID < 0 || workload.GID < 0 {
		return fmt.Errorf("%w: workload uid/gid must not be negative", ErrRestoreTargetDenied)
	}
	if workload.UID == 0 && !p.AllowRootWorkload {
		return fmt.Errorf("%w: the checkpoint declares a root (uid 0) workload and this destination has not authorized root restores (allow_root_workload in the restore policy)", ErrRestoreTargetDenied)
	}
	if len(workload.Command) == 0 || strings.TrimSpace(workload.Command[0]) == "" {
		return fmt.Errorf("%w: workload command is empty", ErrRestoreTargetDenied)
	}
	total := 0
	for _, arg := range workload.Command {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("%w: workload command contains a NUL byte", ErrRestoreTargetDenied)
		}
		total += len(arg)
	}
	if total > 128<<10 {
		return fmt.Errorf("%w: workload command is unreasonably long", ErrRestoreTargetDenied)
	}
	return nil
}

func (p RestorePolicy) rootPrefixAllowed(target string) bool {
	for _, prefix := range p.AllowedRootPrefixes {
		if target == prefix || pathContains(prefix, target) {
			return true
		}
	}
	return false
}

// denied reports whether a path sits inside a refused system tree that this
// policy has not explicitly overridden.
func (p RestorePolicy) denied(path string) bool {
	if !underDeniedTargetPrefix(path) {
		return false
	}
	for _, override := range p.AllowSystemTreeTargets {
		if path == override || pathContains(override, path) {
			return false
		}
	}
	return true
}

func (p RestorePolicy) checkTargetPath(target string) error {
	if target == "" || target == string(filepath.Separator) || !filepath.IsAbs(target) {
		return fmt.Errorf("%w: %q is not a usable absolute workload root", ErrRestoreTargetDenied, target)
	}
	if p.denied(target) {
		return fmt.Errorf("%w: %s is a system directory and may never be replaced by checkpoint content", ErrRestoreTargetDenied, target)
	}
	return nil
}

func underDeniedTargetPrefix(path string) bool {
	for _, prefix := range deniedTargetPrefixes {
		if path == prefix || pathContains(prefix, path) {
			return true
		}
	}
	return false
}

// checkTargetOwnership pins the target to the workload's identity on disk. An
// existing target must already be that workload's real directory; a fresh one
// must sit inside an existing real directory owned by the workload uid or by
// root (an operator-created parent). Symlinked parents and targets are
// refused outright — the stats come from the symlink-free resolution in
// inspectTarget, so no hop along the path can be a link.
func (p RestorePolicy) checkTargetOwnership(stats *targetStats, workloadUID int) error {
	parent := filepath.Dir(stats.path)
	if err := requireOwnedBy(stats.parentStat, parent, workloadUID, true); err != nil {
		return err
	}
	if !stats.targetExists {
		return nil
	}
	if stats.targetStat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return fmt.Errorf("%w: %s must be a real directory, not a symlink", ErrRestoreTargetDenied, stats.path)
	}
	return requireOwnedBy(stats.targetStat, stats.path, workloadUID, false)
}

func requireOwnedBy(stat syscall.Stat_t, path string, workloadUID int, allowRoot bool) error {
	if int(stat.Uid) == workloadUID {
		return nil
	}
	if allowRoot && stat.Uid == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s is not owned by workload uid %d", ErrRestoreTargetDenied, path, workloadUID)
}

// RecheckPinnedTarget re-runs the admission-time ownership verdict against
// the exact handle the filesystem switch uses. Admission
// (CheckRestoreTarget) pins and closes its handle at the start of Prepare;
// during the extraction window that follows, a parent owned by the workload
// uid — which policy permits — can have its entries renamed behind the path.
// Re-running the check on the switch pin's stats immediately before the
// backup rename restores the ownership guarantee without holding the
// admission handle open across the whole transaction.
func (p RestorePolicy) RecheckPinnedTarget(target *targetDir, workloadUID int) error {
	if target == nil {
		return fmt.Errorf("%w: no pinned target to re-check", ErrRestoreTargetDenied)
	}
	var parentStat syscall.Stat_t
	if err := syscall.Fstat(target.fd, &parentStat); err != nil {
		return fmt.Errorf("%w: target parent %s is no longer reachable: %v", ErrRestoreTargetDenied, target.path, err)
	}
	targetStat, exists, err := target.lstatTarget()
	if err != nil {
		return err
	}
	stats := &targetStats{
		path:         filepath.Join(target.path, target.name),
		parentStat:   parentStat,
		targetExists: exists,
		targetStat:   targetStat,
	}
	return p.checkTargetOwnership(stats, workloadUID)
}

// SanitizeExtractedEntry applies the destination restore policy to one entry
// of a freshly extracted checkpoint tree. The archive is untrusted input
// produced by the source machine: extraction as root reproduces whatever it
// carries, so anything that turns a workload directory into local privilege
// escalation is neutralized here — device nodes, FIFOs, sockets, and other
// special files are refused unless explicitly allowed (a planted FIFO blocks
// any later open forever; a device node is direct hardware access), the
// setuid/setgid bits are stripped unless the policy keeps them, and
// privilege-bearing extended attributes are stripped unless the policy keeps
// file capabilities. The extraction walk calls this for every entry;
// symlink containment is enforced separately.
func SanitizeExtractedEntry(path string, mode os.FileMode, policy RestorePolicy) error {
	if mode&(os.ModeDevice|os.ModeCharDevice) != 0 && !policy.AllowDeviceNodes {
		return fmt.Errorf("checkpoint archive contains device node %s; refusing extraction (allow_device_nodes is not set)", path)
	}
	if mode&(os.ModeNamedPipe|os.ModeSocket|os.ModeIrregular) != 0 && !policy.AllowDeviceNodes {
		return fmt.Errorf("checkpoint archive contains FIFO, socket, or special file %s; refusing extraction (allow_device_nodes is not set)", path)
	}
	if mode&os.ModeSymlink == 0 && mode&(os.ModeSetuid|os.ModeSetgid) != 0 && !policy.AllowSetuid {
		if err := os.Chmod(path, mode&^(os.ModeSetuid|os.ModeSetgid)); err != nil {
			return fmt.Errorf("strip setuid/setgid from %s: %w", path, err)
		}
	}
	return policy.stripEntryXattrs(path)
}

// xattrStrippedByPolicy reports whether a named extended attribute is
// privilege-bearing for a restored workload and must be removed by
// stripEntryXattrs. Kept pure so the admission decision is exhaustively
// testable without CAP_SETFCAP.
func xattrStrippedByPolicy(name string, policy RestorePolicy) bool {
	if !strings.HasPrefix(name, "security.") && !strings.HasPrefix(name, "trusted.") {
		return false
	}
	return !(name == "security.capability" && policy.AllowFileCaps)
}

// stripEntryXattrs removes extended attributes in the security and trusted
// namespaces from an extracted entry. security.capability is the modern
// setuid: a planted cap_setuid/cap_dac_override=ep binary hands the restored
// workload host-root power in the init user namespace, and extraction as root
// with --xattrs restores it verbatim. Other security.* / trusted.* labels
// (SELinux/Smack/AppArmor types, trusted.overlay, …) are host authority a
// checkpoint has no business setting and are stripped as well; only an
// explicit allow_file_caps keeps security.capability. POSIX ACLs (system.*)
// are not privilege-bearing in this model and are left intact.
func (p RestorePolicy) stripEntryXattrs(path string) error {
	size, err := unix.Llistxattr(path, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil
		}
		return fmt.Errorf("list extended attributes of %s: %w", path, err)
	}
	if size <= 0 {
		return nil
	}
	buffer := make([]byte, size)
	read, err := unix.Llistxattr(path, buffer)
	if err != nil {
		return fmt.Errorf("list extended attributes of %s: %w", path, err)
	}
	for _, name := range strings.Split(string(buffer[:read]), "\x00") {
		if name == "" || !xattrStrippedByPolicy(name, p) {
			continue
		}
		if err := unix.Lremovexattr(path, name); err != nil && !errors.Is(err, unix.ENODATA) {
			return fmt.Errorf("strip extended attribute %s from %s: %w", name, path, err)
		}
	}
	return nil
}
