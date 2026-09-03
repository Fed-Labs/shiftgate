package runtime

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives the test binary the workload-exec re-exec verb the
// shift-agent binary dispatches in main. Manager.Start launches workloads
// through that verb whenever it runs privileged, and these tests share the
// manager with the agent — so run under root, the helper the manager
// re-executes is this binary. The shim replaces the process on success; a
// return is a failure.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == WorkloadExecCommand {
		if err := RunWorkloadExec(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "workload exec shim:", err)
		}
		os.Exit(1)
	}
	os.Exit(m.Run())
}
