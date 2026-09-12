package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// WorkloadExecCommand is the argv[1] the agent binary recognizes as the
// workload-exec shim. A privileged agent launches every workload by
// re-executing itself with this verb: the shim installs the seccomp filter
// below and then replaces itself with the workload's command, so the workload
// process itself — not a parent that could exit — carries the filter. Like the
// mount-namespace verb, it is not a user-facing command.
const WorkloadExecCommand = "workload-exec"

// The io_uring syscalls occupy the architecture-independent tail of the Linux
// syscall table: 425-427 name io_uring_setup, io_uring_enter, and
// io_uring_register on every architecture, 32-bit or 64-bit, so the filter can
// compare raw numbers without first proving the caller's architecture.
const (
	sysIoUringSetup    = 425
	sysIoUringEnter    = 426
	sysIoUringRegister = 427
)

// Classic BPF instruction encodings and return actions, and the prctl(2)
// selectors the shim needs. The syscall package defines the BPF structs but
// none of these. bpfLoadWordAbsolute is BPF_LD|BPF_W|BPF_ABS, bpfJumpEqual is
// BPF_JMP|BPF_JEQ|BPF_K, bpfReturn is BPF_RET|BPF_K.
const (
	bpfLoadWordAbsolute = 0x20
	bpfJumpEqual        = 0x15
	bpfReturn           = 0x06
	seccompRetAllow     = 0x7fff0000
	seccompRetErrno     = 0x00050000
	prSetNoNewPrivs     = 38
	prSetSeccomp        = 22
	seccompModeFilter   = 2
)

// ioUringFilter returns the seccomp program that fails the three io_uring
// syscalls with EPERM and allows everything else. Runtimes that use io_uring —
// libuv above all, which Node links — take the failure as "no io_uring here"
// and fall back to plain syscalls, so nothing breaks; but no process under the
// filter can ever hold an io_uring instance, whose anon_inode:[io_uring]
// mappings CRIU cannot dump. Workloads a privileged agent launches are
// therefore checkpointable by construction, not by the cooperation of whatever
// runtime they happen to run. Denying these three syscalls is also what a
// container runtime's default seccomp profile does.
func ioUringFilter() []syscall.SockFilter {
	return []syscall.SockFilter{
		// Load the syscall number into the accumulator.
		{Code: bpfLoadWordAbsolute, K: 0},
		{Code: bpfJumpEqual, Jt: 3, K: sysIoUringSetup},
		{Code: bpfJumpEqual, Jt: 2, K: sysIoUringEnter},
		{Code: bpfJumpEqual, Jt: 1, K: sysIoUringRegister},
		{Code: bpfReturn, K: seccompRetAllow},
		// Fail the io_uring syscalls with EPERM.
		{Code: bpfReturn, K: seccompRetErrno | 1},
	}
}

// RunWorkloadExec is the entry point of the workload-exec shim. The parent has
// already placed this process in its PID namespace, cgroup, session, and
// credential; execve preserves all of that, and a filter installed before the
// exec survives it, so the workload runs under the filter from its first
// instruction.
func RunWorkloadExec(arguments []string) error {
	if len(arguments) < 2 || arguments[0] != "--" {
		return errors.New("workload exec shim requires -- followed by a program to execute")
	}
	program, programArgs := arguments[1], arguments[2:]
	resolved, err := exec.LookPath(program)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", program, err)
	}
	if err := installIOUringFilter(); err != nil {
		return err
	}
	return syscall.Exec(resolved, append([]string{resolved}, programArgs...), os.Environ())
}

// installIOUringFilter applies the io_uring filter to the current process. A
// process without CAP_SYS_ADMIN may install a filter only after promising
// never to gain privileges again, so the no-new-privileges flag goes first.
// That promise is what makes the filter safe to install for a workload running
// as an unprivileged uid — and it means a workload cannot exec a setuid binary
// to root, the same trade every container runtime makes for this protection.
func installIOUringFilter() error {
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("set no-new-privileges: %w", errno)
	}
	program := ioUringFilter()
	fprog := syscall.SockFprog{Len: uint16(len(program)), Filter: &program[0]}
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetSeccomp, seccompModeFilter, uintptr(unsafe.Pointer(&fprog)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("install io_uring seccomp filter: %w", errno)
	}
	return nil
}
