package checkpoint

import (
	"fmt"
	"os"
	"testing"

	shiftruntime "shift.dev/shift/internal/runtime"
)

// TestMain gives the test binary the workload-exec re-exec verb the
// shift-agent binary dispatches in main: the fixtures start real workloads
// through the runtime manager, and a privileged manager launches every
// workload through that verb — so run under root, the helper it re-executes
// is this binary. The shim replaces the process on success; a return is a
// failure.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == shiftruntime.WorkloadExecCommand {
		if err := shiftruntime.RunWorkloadExec(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "workload exec shim:", err)
		}
		os.Exit(1)
	}
	os.Exit(m.Run())
}
