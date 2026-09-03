package integration

import (
	"fmt"
	"os"
	"testing"

	"shift.dev/shift/internal/checkpoint"
	shiftruntime "shift.dev/shift/internal/runtime"
)

// TestMain gives the test binary the re-exec verbs the shift-agent binary
// dispatches in main. The e2e suite runs the agent as a library inside this
// binary, so when the runtime manager launches a workload through the exec
// shim, or a restore through the mount-namespace shim, the helper it
// re-executes is this binary with the verb as argv[1] — exactly how the real
// agent re-executes itself. Both shims replace the process on success; a
// return is a failure.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case checkpoint.MountNamespaceCommand:
			if err := checkpoint.RunMountNamespace(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "mount namespace shim:", err)
			}
			os.Exit(1)
		case shiftruntime.WorkloadExecCommand:
			if err := shiftruntime.RunWorkloadExec(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "workload exec shim:", err)
			}
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}
