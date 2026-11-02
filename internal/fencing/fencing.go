// Package fencing decides whether an unattended failover may proceed. The
// contract is deliberately asymmetric: automatic failover is admitted only
// after a provider positively confirms the source is fenced — ResultFenced or
// ResultAlreadyFenced with a nil error, as AllowsAutomatic states — and every
// other outcome fails closed with ErrAutomaticFailoverBlocked while the
// standby keeps waiting. The operator's explicit failover trigger is unfenced
// by design: the command is the confirmation. The default provider is
// Disabled, which never authorizes anything.
package fencing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Result is deliberately explicit: only Fenced and AlreadyFenced permit an
// unattended failover. Every other result is inconclusive and must fail closed.
type Result string

const (
	ResultFenced        Result = "fenced"
	ResultAlreadyFenced Result = "already_fenced"
	ResultDisabled      Result = "not_configured"
	ResultRefused       Result = "refused"
	ResultTimedOut      Result = "timed_out"
	ResultInconclusive  Result = "inconclusive"
)

type Request struct {
	WorkloadID      string
	SourceMachineID string
	Epoch           uint64
}

type Provider interface {
	Name() string
	Fence(context.Context, Request) (Result, error)
}

var ErrAutomaticFailoverBlocked = errors.New("automatic failover is blocked because fencing was not positively confirmed")

type Disabled struct{}

func (Disabled) Name() string { return "disabled" }
func (Disabled) Fence(context.Context, Request) (Result, error) {
	return ResultDisabled, ErrAutomaticFailoverBlocked
}

// Self is an explicit declaration that the source is expected to stop itself
// after losing ownership. It is not hard fencing: the result is always
// inconclusive until a real watchdog implementation is attached.
type Self struct{}

func (Self) Name() string { return "self" }
func (Self) Fence(context.Context, Request) (Result, error) {
	return ResultInconclusive, fmt.Errorf("self fencing requires a configured source watchdog: %w", ErrAutomaticFailoverBlocked)
}

// Command invokes a narrowly configured external fencing integration. The
// command receives only fixed positional facts, never workload environment or
// secrets. Exit 0 means fenced; exit 2 means already fenced; all other exits
// are refused/inconclusive. Both binaries must be absolute paths inside one
// of AllowedDirs — the integration runs as the agent's user (root on a
// privileged agent), so a free-form path is an arbitrary-root-execution
// primitive, not configuration. A symlink in the allow-listed directory is
// the same primitive wearing a costume: the resolved file must itself sit
// inside the allow-listed directory, be a root- (or agent-) owned regular
// file that is not group- or world-writable, and still be that same file
// (device and inode) when the exec happens.
type Command struct {
	Path       string
	StatusPath string
	Timeout    time.Duration
	// AllowedDirs bounds where the fencing binaries may live. Empty means
	// DefaultFencingDirs.
	AllowedDirs []string
}

// DefaultFencingDirs is the allowlist a command fencing integration must
// live under when the caller configures no AllowedDirs: operator-managed
// directories, not whatever path a config file happens to name.
var DefaultFencingDirs = []string{"/etc/shift/fencing.d", "/usr/local/libexec/shift/fencing.d"}

func (c Command) Name() string { return "command" }

// fencingBinary is a validated fencing executable: the resolved path plus the
// device and inode it had when validation ran, so the exec can re-prove it is
// still the same file.
type fencingBinary struct {
	resolved string
	dev      uint64
	ino      uint64
}

func (c Command) validate() (fencingBinary, fencingBinary, error) {
	dirs := c.AllowedDirs
	if len(dirs) == 0 {
		dirs = DefaultFencingDirs
	}
	resolvedDirs := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		resolved, err := filepath.EvalSymlinks(filepath.Clean(dir))
		if err != nil {
			continue
		}
		resolvedDirs = append(resolvedDirs, resolved)
	}
	if len(resolvedDirs) == 0 {
		return fencingBinary{}, fencingBinary{}, fmt.Errorf("none of the allow-listed fencing directories %v exist", dirs)
	}
	fence, err := validateBinary(c.Path, resolvedDirs)
	if err != nil {
		return fencingBinary{}, fencingBinary{}, err
	}
	status, err := validateBinary(c.StatusPath, resolvedDirs)
	if err != nil {
		return fencingBinary{}, fencingBinary{}, err
	}
	return fence, status, nil
}

func validateBinary(candidate string, resolvedDirs []string) (fencingBinary, error) {
	if strings.TrimSpace(candidate) == "" {
		return fencingBinary{}, errors.New("fencing command and post-fence status command are required")
	}
	clean := filepath.Clean(candidate)
	if !filepath.IsAbs(clean) {
		return fencingBinary{}, fmt.Errorf("fencing command %q must be an absolute path", candidate)
	}
	// The candidate itself must be a regular file, not a symlink: Lstat on
	// the exact path, no following.
	info, err := os.Lstat(clean)
	if err != nil {
		return fencingBinary{}, fmt.Errorf("fencing command %q: %w", candidate, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fencingBinary{}, fmt.Errorf("fencing command %q must be a regular file, not a symlink or other special file", candidate)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fencingBinary{}, fmt.Errorf("fencing command %q: cannot determine ownership", candidate)
	}
	if euid := os.Geteuid(); stat.Uid != 0 && stat.Uid != uint32(euid) {
		return fencingBinary{}, fmt.Errorf("fencing command %q must be owned by root or the agent user", candidate)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fencingBinary{}, fmt.Errorf("fencing command %q must not be group- or world-writable", candidate)
	}
	// Containment is checked on the fully resolved path as well: a symlink
	// anywhere in the chain (a directory swap inside an allow-listed dir)
	// must not smuggle in /tmp/evil or /bin/sh.
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return fencingBinary{}, fmt.Errorf("fencing command %q: %w", candidate, err)
	}
	allowed := false
	for _, dir := range resolvedDirs {
		relative, relErr := filepath.Rel(dir, resolved)
		if relErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fencingBinary{}, fmt.Errorf("fencing command %q resolves to %s, outside the allow-listed fencing directories", candidate, resolved)
	}
	return fencingBinary{resolved: resolved, dev: uint64(stat.Dev), ino: stat.Ino}, nil
}

// stillSame re-proves, immediately before an exec, that the binary is still
// the file validation approved.
func (b fencingBinary) stillSame() error {
	info, err := os.Lstat(b.resolved)
	if err != nil {
		return fmt.Errorf("fencing command %s disappeared after validation: %w", b.resolved, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Dev) != b.dev || stat.Ino != b.ino {
		return fmt.Errorf("fencing command %s was replaced after validation", b.resolved)
	}
	return nil
}

func (c Command) Fence(ctx context.Context, request Request) (Result, error) {
	fenceBinary, statusBinary, err := c.validate()
	if err != nil {
		return ResultDisabled, fmt.Errorf("%w: %w", err, ErrAutomaticFailoverBlocked)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := fenceBinary.stillSame(); err != nil {
		return ResultInconclusive, fmt.Errorf("%w: %w", err, ErrAutomaticFailoverBlocked)
	}
	command := exec.CommandContext(callCtx, fenceBinary.resolved, request.WorkloadID, request.SourceMachineID, fmt.Sprint(request.Epoch))
	err = command.Run()
	if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		return ResultTimedOut, fmt.Errorf("%w: %w", callCtx.Err(), ErrAutomaticFailoverBlocked)
	}
	if err == nil {
		// The status check gets its own time budget: a slow fence that
		// correctly reported success must not leave the confirmation with
		// no budget and block the failover it just earned.
		statusCtx, statusCancel := context.WithTimeout(ctx, timeout)
		defer statusCancel()
		if statusErr := statusBinary.stillSame(); statusErr != nil {
			return ResultInconclusive, fmt.Errorf("%w: %w", statusErr, ErrAutomaticFailoverBlocked)
		}
		status := exec.CommandContext(statusCtx, statusBinary.resolved, request.WorkloadID, request.SourceMachineID, fmt.Sprint(request.Epoch))
		statusErr := status.Run()
		if statusErr == nil {
			return ResultFenced, nil
		}
		if errors.Is(statusCtx.Err(), context.DeadlineExceeded) {
			return ResultTimedOut, fmt.Errorf("%w: %w", statusCtx.Err(), ErrAutomaticFailoverBlocked)
		}
		return ResultInconclusive, fmt.Errorf("post-fence status did not confirm source stopped: %w: %w", statusErr, ErrAutomaticFailoverBlocked)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 2 {
		return ResultAlreadyFenced, nil
	}
	return ResultRefused, fmt.Errorf("%w: %w", err, ErrAutomaticFailoverBlocked)
}

func AllowsAutomatic(result Result) bool {
	return result == ResultFenced || result == ResultAlreadyFenced
}
