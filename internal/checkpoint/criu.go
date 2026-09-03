package checkpoint

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type CRIU struct {
	binary string
	helper string
}

type DumpOptions struct {
	PID             int
	ImagesDirectory string
	ParentImages    string
	TCPState        bool
	ShellJob        bool
	FileLocks       bool
	ExternalUNIX    bool
	LeaveStopped    bool
	LeaveRunning    bool
	ManageCgroups   string
}

type RestoreOptions struct {
	ImagesDirectory string
	TCPState        bool
	ShellJob        bool
	FileLocks       bool
	ExternalUNIX    bool
	ManageCgroups   string
	// BindMounts are applied inside a private mount namespace before CRIU
	// runs. A forked workload uses this so its own filesystem copy appears at
	// the absolute path recorded in the checkpointed process images, which is
	// what lets a fork and its source run at the same time on one machine.
	BindMounts []BindMount
}

func NewCRIU(binary string) (*CRIU, error) {
	if binary == "" {
		resolved, err := exec.LookPath("criu")
		if err != nil {
			return nil, errors.New("CRIU is not installed")
		}
		binary = resolved
	}
	helper, err := os.Executable()
	if err != nil {
		helper = ""
	}
	return &CRIU{binary: binary, helper: helper}, nil
}

// SetMountNamespaceHelper overrides the binary re-executed as the private mount
// namespace shim. Only the agent binary implements the shim verb, so a process
// that embeds the engine without it must point this at the agent.
func (c *CRIU) SetMountNamespaceHelper(path string) {
	c.helper = path
}

func (c *CRIU) Version(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, c.binary, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("criu version: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func (c *CRIU) Check(ctx context.Context) error {
	output, err := exec.CommandContext(ctx, c.binary, "check").CombinedOutput()
	if err != nil {
		return fmt.Errorf("criu kernel check failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (c *CRIU) PreDump(ctx context.Context, options DumpOptions) error {
	if err := validateDumpOptions(options); err != nil {
		return err
	}
	args, err := c.dumpArgs("pre-dump", options)
	if err != nil {
		return err
	}
	args = append(args, "--track-mem")
	return c.run(ctx, options.ImagesDirectory, args)
}

func (c *CRIU) Dump(ctx context.Context, options DumpOptions) error {
	if err := validateDumpOptions(options); err != nil {
		return err
	}
	args, err := c.dumpArgs("dump", options)
	if err != nil {
		return err
	}
	return c.run(ctx, options.ImagesDirectory, args)
}

func (c *CRIU) Restore(ctx context.Context, options RestoreOptions) (int, error) {
	if options.ImagesDirectory == "" {
		return 0, errors.New("images directory is required")
	}
	workDirectory := filepath.Join(options.ImagesDirectory, "work")
	if err := os.MkdirAll(workDirectory, 0o700); err != nil {
		return 0, err
	}
	pidFile := filepath.Join(workDirectory, "restore.pid")
	args := []string{
		"restore",
		"--images-dir", options.ImagesDirectory,
		"--work-dir", workDirectory,
		"--log-file", "restore.log",
		// CRIU spells verbosity -vNUM (its long form is --verbosity=NUM);
		// --log-level is not an option it recognizes.
		"-v4",
		"--restore-detached",
		"--pidfile", pidFile,
	}
	args = appendFeatureArgs(args, options.TCPState, options.ShellJob, options.FileLocks, options.ExternalUNIX, options.ManageCgroups)
	if err := c.runInNamespace(ctx, options.ImagesDirectory, args, options.BindMounts); err != nil {
		return 0, err
	}
	content, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, fmt.Errorf("read restored pid: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil || pid <= 0 {
		return 0, errors.New("CRIU returned an invalid restored pid")
	}
	return pid, nil
}

func validateDumpOptions(options DumpOptions) error {
	if options.PID <= 1 {
		return errors.New("refusing to checkpoint pid 1 or an invalid pid")
	}
	if options.ImagesDirectory == "" {
		return errors.New("images directory is required")
	}
	if options.LeaveRunning && options.LeaveStopped {
		return errors.New("leave-running and leave-stopped are mutually exclusive")
	}
	return nil
}

func (c *CRIU) dumpArgs(action string, options DumpOptions) ([]string, error) {
	workDirectory := filepath.Join(options.ImagesDirectory, "work")
	if err := os.MkdirAll(workDirectory, 0o700); err != nil {
		return nil, err
	}
	args := []string{
		action,
		"--tree", strconv.Itoa(options.PID),
		"--images-dir", options.ImagesDirectory,
		"--work-dir", workDirectory,
		"--log-file", action + ".log",
		// CRIU spells verbosity -vNUM (its long form is --verbosity=NUM);
		// --log-level is not an option it recognizes.
		"-v4",
	}
	args = appendFeatureArgs(args, options.TCPState, options.ShellJob, options.FileLocks, options.ExternalUNIX, options.ManageCgroups)
	if options.ParentImages != "" {
		relative, err := filepath.Rel(options.ImagesDirectory, options.ParentImages)
		if err != nil {
			return nil, err
		}
		args = append(args, "--prev-images-dir", relative, "--track-mem")
	}
	if options.LeaveRunning {
		args = append(args, "--leave-running")
	}
	if options.LeaveStopped {
		args = append(args, "--leave-stopped")
	}
	return args, nil
}

func appendFeatureArgs(args []string, tcpState, shellJob, fileLocks, externalUNIX bool, manageCgroups string) []string {
	if tcpState {
		args = append(args, "--tcp-established")
	}
	if shellJob {
		args = append(args, "--shell-job")
	}
	if fileLocks {
		args = append(args, "--file-locks")
	}
	if externalUNIX {
		args = append(args, "--ext-unix-sk")
	}
	if manageCgroups != "" {
		// --manage-cgroups takes an OPTIONAL argument, and getopt accepts an
		// optional value only in the attached --flag=value form. As a separate
		// word the value survives as a stray positional and CRIU aborts with
		// "excessive parameter".
		args = append(args, "--manage-cgroups="+manageCgroups)
	}
	return args
}

func (c *CRIU) run(ctx context.Context, imagesDirectory string, args []string) error {
	return c.runInNamespace(ctx, imagesDirectory, args, nil)
}

// runInNamespace executes CRIU directly when no bind mounts are requested. With
// bind mounts it re-executes the agent's namespace shim under CLONE_NEWNS, which
// Go additionally marks MS_PRIVATE recursively, so the mounts stay invisible to
// the rest of the machine and are released when the restored tree exits.
func (c *CRIU) runInNamespace(ctx context.Context, imagesDirectory string, args []string, mounts []BindMount) error {
	command := exec.CommandContext(ctx, c.binary, args...)
	if len(mounts) > 0 {
		shimArgs, err := namespaceCommand(c.helper, mounts, c.binary, args)
		if err != nil {
			return err
		}
		command = exec.CommandContext(ctx, c.helper, shimArgs...)
		command.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
	}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		logTail := tailFile(filepath.Join(imagesDirectory, "work", args[0]+".log"), 40)
		return fmt.Errorf("criu %s failed: %w: %s%s", args[0], err, strings.TrimSpace(output.String()), logTail)
	}
	return nil
}

func tailFile(path string, lines int) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	values := make([]string, 0, lines)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if len(values) == lines {
			copy(values, values[1:])
			values[len(values)-1] = scanner.Text()
		} else {
			values = append(values, scanner.Text())
		}
	}
	if len(values) == 0 {
		return ""
	}
	return "\nCRIU log:\n" + strings.Join(values, "\n")
}

func commandTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return context.WithTimeout(parent, timeout)
}
