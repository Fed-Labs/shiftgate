package integration

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"shift.dev/shift/internal/checkpoint"
	shiftruntime "shift.dev/shift/internal/runtime"
)

// laneOutcomes records what actually happened to every test the runner
// selected, so TestMain can enforce the lane promise after m.Run(): a
// SHIFT_TEST_E2E / SHIFT_TEST_STRESS lane where every test skipped silently
// is not coverage, and must fail instead of exiting green.
var laneOutcomes struct {
	sync.Mutex
	ran     []string
	skipped []string
}

// recordLaneOutcome registers t's final outcome (one per test, at the top of
// its gate, before any skip can hide it). A skipped test is recorded with
// its name so the stress lane can refuse a run that silently dropped one of
// its scenarios.
func recordLaneOutcome(t testing.TB) {
	t.Helper()
	name := t.Name()
	t.Cleanup(func() {
		outcome, _ := t.(interface{ Skipped() bool })
		laneOutcomes.Lock()
		defer laneOutcomes.Unlock()
		if outcome != nil && outcome.Skipped() {
			laneOutcomes.skipped = append(laneOutcomes.skipped, name)
			return
		}
		laneOutcomes.ran = append(laneOutcomes.ran, name)
	})
}

// checkLaneGuard fails the binary when a requested lane proved nothing:
// zero tests ran (everything skipped — unprivileged, no CRIU, no flags), or
// the stress lane skipped one of its TestStress* scenarios and would
// otherwise pass having run only part of the promised set. It strengthens
// the same guard the CI lanes apply to their logs, so `make test-e2e` and
// `make test-stress` fail the same way the workflow does.
func checkLaneGuard(code int) int {
	e2eLane := os.Getenv("SHIFT_TEST_E2E") == "1"
	stressLane := os.Getenv("SHIFT_TEST_STRESS") == "1"
	laneOutcomes.Lock()
	defer laneOutcomes.Unlock()
	if (e2eLane || stressLane) && len(laneOutcomes.ran) == 0 {
		fmt.Fprintf(os.Stderr, "\nlane guard: SHIFT_TEST_E2E/SHIFT_TEST_STRESS requested but no test ran (selected %d, all skipped); refusing to pass a lane with zero coverage\n",
			len(laneOutcomes.skipped))
		if code == 0 {
			code = 1
		}
	}
	if stressLane {
		for _, name := range laneOutcomes.skipped {
			if strings.HasPrefix(name, "TestStress") {
				fmt.Fprintf(os.Stderr, "\nlane guard: stress scenario %s was skipped; the stress lane must run every scenario it selects (see its --- SKIP line for the reason)\n", name)
				if code == 0 {
					code = 1
				}
			}
		}
	}
	return code
}

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
	os.Exit(checkLaneGuard(m.Run()))
}
