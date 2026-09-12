package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// MountNamespaceCommand is the argv[1] the agent binary recognizes as the
// private-mount-namespace shim. The agent re-executes itself with this verb so
// that bind mounts required by a restore are visible only to the restored
// process tree; nothing is mounted in the host namespace.
const MountNamespaceCommand = "mount-namespace"

// BindMount describes one bind mount applied inside the restore namespace.
// Forked workloads use it to present their own filesystem copy at the absolute
// path recorded in the checkpointed process images.
type BindMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

func (b BindMount) validate() error {
	if b.Source == "" || b.Target == "" {
		return errors.New("bind mount requires a source and a target")
	}
	if !strings.HasPrefix(b.Source, "/") || !strings.HasPrefix(b.Target, "/") {
		return errors.New("bind mount paths must be absolute")
	}
	if b.Target == "/" {
		return errors.New("refusing to bind mount over the filesystem root")
	}
	return nil
}

// namespaceCommand builds the argv that re-executes helper as the mount
// namespace shim. Arguments are passed as a flat argv, never through a shell,
// so paths cannot inject additional commands.
func namespaceCommand(helper string, mounts []BindMount, program string, programArgs []string) ([]string, error) {
	if helper == "" {
		return nil, errors.New("mount namespace helper binary is unknown")
	}
	arguments := []string{MountNamespaceCommand}
	for _, mount := range mounts {
		if err := mount.validate(); err != nil {
			return nil, err
		}
		mode := "rw"
		if mount.ReadOnly {
			mode = "ro"
		}
		arguments = append(arguments, "--bind", mount.Source, mount.Target, mode)
	}
	arguments = append(arguments, "--", program)
	return append(arguments, programArgs...), nil
}

// RunMountNamespace is the entry point of the shim. It runs inside a freshly
// unshared mount namespace whose propagation has already been set to private by
// the parent, applies the requested bind mounts, and then replaces itself with
// the requested program so the restored process tree inherits the namespace.
func RunMountNamespace(arguments []string) error {
	mounts, program, programArgs, err := parseMountNamespaceArgs(arguments)
	if err != nil {
		return err
	}
	for _, mount := range mounts {
		if err := applyBindMount(mount); err != nil {
			return err
		}
	}
	resolved, err := exec.LookPath(program)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", program, err)
	}
	return syscall.Exec(resolved, append([]string{resolved}, programArgs...), os.Environ())
}

func parseMountNamespaceArgs(arguments []string) (mounts []BindMount, program string, programArgs []string, err error) {
	index := 0
	for index < len(arguments) {
		switch arguments[index] {
		case "--bind":
			if index+3 >= len(arguments) {
				return nil, "", nil, errors.New("--bind requires a source, a target, and a mode")
			}
			mount := BindMount{Source: arguments[index+1], Target: arguments[index+2], ReadOnly: arguments[index+3] == "ro"}
			if err := mount.validate(); err != nil {
				return nil, "", nil, err
			}
			mounts = append(mounts, mount)
			index += 4
		case "--":
			remainder := arguments[index+1:]
			if len(remainder) == 0 {
				return nil, "", nil, errors.New("mount namespace shim requires a program to execute")
			}
			return mounts, remainder[0], remainder[1:], nil
		default:
			return nil, "", nil, fmt.Errorf("unexpected mount namespace argument %q", arguments[index])
		}
	}
	return nil, "", nil, errors.New("mount namespace shim requires a program to execute")
}

func applyBindMount(mount BindMount) error {
	if _, err := os.Stat(mount.Source); err != nil {
		return fmt.Errorf("bind mount source %s: %w", mount.Source, err)
	}
	if err := os.MkdirAll(mount.Target, 0o755); err != nil {
		return fmt.Errorf("bind mount target %s: %w", mount.Target, err)
	}
	flags := uintptr(syscall.MS_BIND | syscall.MS_REC)
	if err := syscall.Mount(mount.Source, mount.Target, "", flags, ""); err != nil {
		return fmt.Errorf("bind %s onto %s: %w", mount.Source, mount.Target, err)
	}
	if !mount.ReadOnly {
		return nil
	}
	remount := uintptr(syscall.MS_BIND | syscall.MS_REC | syscall.MS_REMOUNT | syscall.MS_RDONLY)
	if err := syscall.Mount("", mount.Target, "", remount, ""); err != nil {
		return fmt.Errorf("remount %s read-only: %w", mount.Target, err)
	}
	return nil
}
