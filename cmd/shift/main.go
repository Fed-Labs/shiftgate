package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/controlclient"
	"shift.dev/shift/internal/migration"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/termio"
)

type options struct {
	agentEndpoint  string
	controlURL     string
	tokenStorePath string
	jsonOutput     bool
	timeout        time.Duration
	stdout         io.Writer
	stderr         io.Writer
}

type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "shiftgate:", err)
		var exit exitError
		if errors.As(err, &exit) {
			os.Exit(exit.code)
		}
		os.Exit(1)
	}
}

func run(arguments []string, stdout, stderr io.Writer) error {
	if len(arguments) == 1 && arguments[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, "SHIFT", config.Version)
		return nil
	}
	settings := options{
		stdout: stdout, stderr: stderr,
		agentEndpoint:  defaultAgentEndpoint(),
		controlURL:     os.Getenv("SHIFT_CONTROL_URL"),
		tokenStorePath: controlclient.DefaultTokenStorePath(),
		timeout:        30 * time.Second,
	}
	if topic, asked := helpRequest(arguments); asked {
		return printCommandHelp(stdout, topic)
	}
	remaining, err := extractGlobalFlags(arguments, &settings)
	if err != nil {
		return exitError{code: 2, err: err}
	}
	if len(remaining) == 0 {
		printUsage(stdout)
		return exitError{code: 2, err: errors.New("a command is required")}
	}
	command := remaining[0]
	commandArgs := remaining[1:]
	switch command {
	case "version":
		if err := rejectArgs("version", "shiftgate version", commandArgs); err != nil {
			return err
		}
		if settings.jsonOutput {
			return writeJSON(stdout, map[string]string{"version": config.Version})
		}
		_, _ = fmt.Fprintln(stdout, "SHIFT", config.Version)
		return nil
	case "completion":
		return completion(stdout, commandArgs)
	case "connections":
		return runConnections(settings, commandArgs)
	case "disconnect":
		return runDisconnect(settings, commandArgs)
	case "connect":
		return runConnect(context.Background(), settings, commandArgs)
	}
	ctx := context.Background()
	// Control-plane commands share the global flags but talk to the control
	// plane, not the agent — building an agent client for them would both
	// fail pointlessly when no agent runs and imply the wrong target.
	if isControlCommand(command) {
		return runControl(ctx, command, commandArgs, settings)
	}
	client, err := agentclient.New(settings.agentEndpoint, settings.timeout)
	if err != nil {
		return exitError{code: 2, err: err}
	}
	switch command {
	case "doctor":
		return runDoctor(ctx, client, settings, commandArgs)
	case "gc":
		return runGC(ctx, client, settings, commandArgs)
	case "machines", "machine":
		// Dual-mode by configuration: with a control plane configured the
		// fleet view is the more useful answer, and a lone agent still has
		// its local answer when none is. Registration and capability queries
		// only ever make sense against a control plane, so they always go
		// there — on the default (platform) endpoint that means a login.
		if len(commandArgs) > 0 {
			switch commandArgs[0] {
			case "register", "capability":
				return runFleetMachines(ctx, settings, commandArgs)
			case "list":
				// `machines` and `machines list` are one view (help documents
				// both forms); consume the synonym so the local mode never
				// rejects the first form in its own usage block. Anything
				// after it is still junk and is rejected below.
				commandArgs = commandArgs[1:]
			default:
				if settings.controlURL != "" {
					return runFleetMachines(ctx, settings, commandArgs)
				}
				return usageError(fmt.Sprintf("unknown machines subcommand %q; the local machine view takes no subcommands — 'shiftgate machines register|capability' manage the control-plane registry%s",
					commandArgs[0], suggestCommand(commandArgs[0])))
			}
		}
		if settings.controlURL != "" {
			return runFleetMachines(ctx, settings, commandArgs)
		}
		return runMachine(ctx, client, settings, commandArgs)
	case "workloads":
		if settings.controlURL != "" {
			return runFleetWorkloads(ctx, settings, commandArgs)
		}
		if err := rejectArgs("workloads", "shiftgate workloads", commandArgs); err != nil {
			return err
		}
		return runWorkload(ctx, client, settings, []string{"list"})
	case "workload":
		return runWorkload(ctx, client, settings, commandArgs)
	case "checkpoint":
		return runCheckpoint(ctx, client, settings, commandArgs)
	case "restore":
		return runRestore(ctx, client, settings, commandArgs)
	case "fork":
		return runFork(ctx, client, settings, commandArgs)
	case "clone":
		return runClone(ctx, client, settings, commandArgs)
	case "migrate":
		return runMigrate(ctx, client, settings, commandArgs)
	case "failover":
		return runFailover(ctx, client, settings, commandArgs)
	case "standby":
		return runStandby(ctx, client, settings, commandArgs)
	case "status":
		return runStatus(ctx, client, settings, commandArgs)
	case "update":
		return runUpdate(ctx, client, settings, commandArgs)
	case "logs":
		return runWorkload(ctx, client, settings, append([]string{"logs"}, commandArgs...))
	default:
		printUsage(stderr)
		if strings.HasPrefix(command, "-") {
			return exitError{code: 2, err: fmt.Errorf("unknown flag %q; see the global flags in 'shiftgate help' (a mistyped global flag is not a command)", command)}
		}
		return exitError{code: 2, err: fmt.Errorf("unknown command %q%s", command, suggestCommand(command))}
	}
}

// isControlCommand reports whether a command targets the control plane rather
// than the local agent.
func isControlCommand(command string) bool {
	switch command {
	case "login", "logout", "whoami", "plans", "storage", "fleet", "marketplace", "market", "trial", "refer", "referral":
		return true
	}
	return false
}

// runGC fronts the chunk garbage collection story. Online sweeping under the
// running agent is the shipped path: as root, `shiftgate gc` plans and
// (--dry-run=false) deletes unreferenced chunks while the agent serves, under
// the collector's two-phase grace and write guard. The offline 'shift-gc'
// sweep is the fallback when the agent cannot take a live sweep at all.
func runGC(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	flags := flagNew("gc")
	grace := durationFlag(flags, "grace", 24*time.Hour, "minimum age for orphaned chunks")
	dryRun := flags.Bool("dry-run", true, "plan only; never delete chunks")
	positional, err := parseCommandFlags(flags, arguments)
	if err != nil {
		return usageError(err.Error())
	}
	if err := rejectArgs("gc", "shiftgate gc [--grace DURATION] [--dry-run]", positional); err != nil {
		return err
	}
	result, err := client.GarbageCollect(ctx, agentclient.GCRequest{GraceSeconds: int(grace.Seconds()), DryRun: *dryRun})
	if err != nil {
		var api *agentclient.APIError
		if errors.As(err, &api) && api.Code == "GC_ROOT_REQUIRED" {
			printOfflineGC(settings, *grace, *dryRun, api.Code)
			return exitError{code: 3, err: errors.New("garbage collection needs root; rerun as root (sudo shiftgate gc), or run the offline sweep printed above")}
		}
		// A version-skewed or unreachable agent must not dead-end here
		// either: the offline sweep is the fallback in every failure mode.
		printOfflineGC(settings, *grace, *dryRun, "")
		return operationError(err)
	}
	return writeJSON(settings.stdout, result)
}

// printOfflineGC names the remedy and the offline tool's exact invocation: an
// operator staring at a full disk gets commands to copy, not a pointer at an
// undocumented workflow. Everything goes to stderr — next to the error that
// prompted it, and clear of --json stdout.
func printOfflineGC(settings options, grace time.Duration, dryRun bool, code string) {
	out := settings.stderr
	switch code {
	case "GC_ROOT_REQUIRED":
		_, _ = fmt.Fprintln(out, "Garbage collection needs root (it rewrites the agent state directory): sudo shiftgate gc")
	default:
		_, _ = fmt.Fprintln(out, "The agent did not complete a live sweep; the offline sweep below reclaims the same chunks.")
	}
	stateDir := "/var/lib/shift"
	_, _ = fmt.Fprintf(out, "Offline fallback (shift-gc), when the agent cannot take a live sweep:\n"+
		"  sudo systemctl stop shift-agent\n"+
		"  sudo shift-gc --state-dir %s --grace %s --dry-run        # plan only\n"+
		"  sudo shift-gc --state-dir %s --grace %s --dry-run=false  # delete orphans past the grace period\n"+
		"  sudo systemctl start shift-agent\n"+
		"(adjust --state-dir when the agent config names another state directory)\n",
		stateDir, grace, stateDir, grace)
	if !dryRun {
		_, _ = fmt.Fprintln(out, "This run requested deletion; rerun with root, or use the offline --dry-run=false invocation above.")
	}
}

func runDoctor(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if err := rejectArgs("doctor", "shiftgate doctor", arguments); err != nil {
		return err
	}
	result, err := client.Doctor(ctx)
	if err != nil {
		return operationError(err)
	}
	report := buildDoctorReport(ctx, settings, result)
	if settings.jsonOutput {
		if err := writeJSON(settings.stdout, report); err != nil {
			return err
		}
		// The exit-3 contract is not a human-renderer detail: CI running
		// --json must see the same failure digit as an operator does.
		if !report.Healthy {
			return exitError{code: 3, err: errors.New("one or more required checks failed")}
		}
		return nil
	}
	return writeDoctorHuman(settings, report)
}

func runMachine(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if err := rejectArgs("machines", "shiftgate machines", arguments); err != nil {
		return err
	}
	machine, err := client.Machine(ctx)
	if err != nil {
		return operationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, []model.MachineCapabilities{machine})
	}
	// The full machine id, not a truncation: it is the value another
	// machine's `migrate --machine-id` pin must match exactly.
	_, _ = fmt.Fprintf(settings.stdout, "MACHINE %s\n", machine.MachineID)
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "NAME\tOS\tARCH\tCPU\tMEMORY\tGPU\tRUNTIMES\tCRIU")
	gpu := "-"
	if len(machine.GPUs) > 0 {
		gpu = machine.GPUs[0].Vendor + " " + machine.GPUs[0].Model
	}
	runtimes := "-"
	if len(machine.GPURuntimes) > 0 {
		names := make([]string, 0, len(machine.GPURuntimes))
		for _, runtime := range machine.GPURuntimes {
			if runtime.Version != "" {
				names = append(names, runtime.Name+" "+runtime.Version)
			} else {
				names = append(names, runtime.Name)
			}
		}
		runtimes = strings.Join(names, ", ")
	}
	criu := "unavailable"
	if machine.CRIU.Healthy {
		criu = machine.CRIU.Version
	}
	_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", machine.Hostname, machine.Distribution, machine.Architecture, machine.CPUs, humanBytes(int64(machine.MemoryBytes)), gpu, runtimes, criu)
	return writer.Flush()
}

var workloadSubcommands = []string{"create", "list", "inspect", "policy", "failover", "start", "pause", "resume", "stop", "delete", "logs"}

func runWorkload(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if len(arguments) == 0 {
		return usageError("workload subcommand is required; valid: " + strings.Join(workloadSubcommands, ", "))
	}
	subcommand := arguments[0]
	args := arguments[1:]
	switch subcommand {
	case "list":
		if err := rejectArgs("workload list", "shiftgate workload list", args); err != nil {
			return err
		}
		values, err := client.Workloads(ctx)
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, values)
		}
		writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "NAME\tSTATUS\tPID\tROOT\tLATEST CHECKPOINT")
		for _, workload := range values {
			pid := "-"
			if workload.Process != nil {
				pid = strconv.Itoa(workload.Process.PID)
			}
			checkpointID := shortID(workload.LatestCheckpointID)
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", workload.Spec.Name, workload.Status, pid, workload.Spec.RootPath, checkpointID)
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		for _, workload := range values {
			if workload.LastError != "" {
				_, _ = fmt.Fprintf(settings.stdout, "%s: %s (see 'shiftgate workload inspect %s')\n", workload.Spec.Name, workload.LastError, workload.Spec.ID)
			}
		}
		return nil
	case "create":
		return workloadCreate(ctx, client, settings, args)
	case "inspect":
		flags := flagNew("workload inspect")
		positional, err := parseCommandFlags(flags, args)
		if err != nil {
			return usageError(err.Error())
		}
		if len(positional) != 1 {
			return usageError("usage: shiftgate workload inspect ID")
		}
		value, err := client.Workload(ctx, positional[0])
		if err != nil {
			return operationError(err)
		}
		return writeJSON(settings.stdout, value)
	case "policy":
		flags := flagNew("workload policy")
		every := flags.String("every", "", "checkpoint interval, for example 10m (minimum 10s)")
		keep := flags.Int("keep", 0, "retain at most this many of the workload's checkpoints (0 keeps all)")
		off := flags.Bool("off", false, "clear the periodic checkpoint policy")
		positional, err := parseCommandFlags(flags, args)
		if err != nil {
			return usageError(err.Error())
		}
		if len(positional) != 1 {
			return usageError("usage: shiftgate workload policy NAME (--every 10m [--keep 5] | --off)")
		}
		intervalSeconds := 0
		if !*off {
			// Periodic checkpoint policies are a paid capability (plan 2
			// BR-07): free orgs get 402 PLAN_REQUIRED_PERIODIC_POLICY. Clear
			// a policy (--off) is never gated.
			if err := checkPlanFeatureEntitled(ctx, settings, billing.FeaturePeriodicPolicies, billing.PeriodicPolicyPlanRequiredCode, billing.PeriodicPolicyPlanRequiredMessage); err != nil {
				return err
			}
			if *every == "" {
				return usageError("either --every DURATION or --off is required")
			}
			interval, err := time.ParseDuration(*every)
			if err != nil {
				return usageError("--every must be a duration, for example 10m")
			}
			policy := &model.CheckpointPolicySpec{IntervalSeconds: int(interval.Seconds()), KeepLast: *keep}
			if err := policy.Validate(); err != nil {
				return usageError(err.Error())
			}
			intervalSeconds = policy.IntervalSeconds
		} else if *every != "" || *keep != 0 {
			return usageError("--off cannot be combined with --every or --keep")
		}
		value, err := client.SetCheckpointPolicy(ctx, positional[0], intervalSeconds, *keep)
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, value)
		}
		if err := printWorkload(settings, value); err != nil {
			return err
		}
		if policy := value.Spec.CheckpointPolicy; policy != nil {
			keepNote := "all checkpoints"
			if policy.KeepLast > 0 {
				keepNote = fmt.Sprintf("the last %d checkpoints", policy.KeepLast)
			}
			_, _ = fmt.Fprintf(settings.stdout, "checkpoint policy: every %s, retaining %s\n",
				(time.Duration(policy.IntervalSeconds) * time.Second).String(), keepNote)
		} else {
			_, _ = fmt.Fprintln(settings.stdout, "checkpoint policy: disabled")
		}
		return nil
	case "failover":
		flags := flagNew("workload failover")
		to := flags.String("to", "", "standby agent's peer URL (https://HOST:PORT)")
		machineID := flags.String("machine-id", "", "expected standby machine id; refuse a host that answers as another")
		keep := flags.Int("keep", 0, "standby retains at most this many of the workload's checkpoints (0 keeps all)")
		off := flags.Bool("off", false, "clear the failover policy; the standby duty is withdrawn")
		positional, err := parseCommandFlags(flags, args)
		if err != nil {
			return usageError(err.Error())
		}
		if len(positional) != 1 {
			return usageError("usage: shiftgate workload failover NAME (--to URL [--machine-id ID] [--keep N] | --off)")
		}
		var policy *model.FailoverPolicySpec
		if !*off {
			if *to == "" {
				return usageError("either --to URL or --off is required")
			}
			policy = &model.FailoverPolicySpec{AgentURL: *to, MachineID: *machineID, KeepLast: *keep}
			if err := policy.Validate(); err != nil {
				return usageError(err.Error())
			}
		} else if *to != "" || *machineID != "" || *keep != 0 {
			return usageError("--off cannot be combined with --to, --machine-id, or --keep")
		}
		agentURL, pin, keepLast := "", "", 0
		if policy != nil {
			// Arming warm-standby replication is a paid entitlement
			// (plan 2 BR-08): HA Pack on Pro, included on Business. Clearing
			// a policy is never gated.
			if err := checkPlanFeatureEntitled(ctx, settings, billing.FeatureFailover, billing.FailoverPlanRequiredCode, billing.FailoverPlanRequiredMessage); err != nil {
				return err
			}
			agentURL, pin, keepLast = policy.AgentURL, policy.MachineID, policy.KeepLast
		}
		value, err := client.SetFailoverPolicy(ctx, positional[0], agentURL, pin, keepLast)
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, value)
		}
		if err := printWorkload(settings, value); err != nil {
			return err
		}
		if installed := value.Spec.FailoverPolicy; installed != nil {
			keepNote := "all checkpoints"
			if installed.KeepLast > 0 {
				keepNote = fmt.Sprintf("the last %d checkpoints", installed.KeepLast)
			}
			_, _ = fmt.Fprintf(settings.stdout, "failover policy: replicate to %s, standby retains %s\n", installed.AgentURL, keepNote)
			_, _ = fmt.Fprintln(settings.stdout, "the newest checkpoint is pushed on the next scheduler pass (a few seconds)")
		} else {
			_, _ = fmt.Fprintln(settings.stdout, "failover policy: disabled; the standby duty is withdrawn on the next pass")
		}
		return nil
	case "start", "pause", "resume":
		flags := flagNew("workload " + subcommand)
		positional, err := parseCommandFlags(flags, args)
		if err != nil {
			return usageError(err.Error())
		}
		if len(positional) != 1 {
			return usageError("usage: shiftgate workload " + subcommand + " ID")
		}
		value, err := client.WorkloadAction(ctx, positional[0], subcommand, struct{}{})
		if err != nil {
			return operationError(err)
		}
		return printWorkload(settings, value)
	case "stop":
		flags := flagNew("workload stop")
		timeout := durationFlag(flags, "timeout", 10*time.Second, "graceful stop timeout (accepts seconds or durations)")
		positional, err := parseCommandFlags(flags, args)
		if err != nil {
			return usageError(err.Error())
		}
		if len(positional) != 1 {
			return usageError("usage: shiftgate workload stop ID [--timeout DURATION]")
		}
		// The stop grace is clamped to the agent's 60s limit; say so instead
		// of letting the number shrink silently.
		effective := *timeout
		if effective > time.Minute {
			_, _ = fmt.Fprintf(settings.stderr, "note: --timeout %s exceeds the agent's 60s stop limit and is clamped to 1m\n", *timeout)
			effective = time.Minute
		}
		value, err := opClient(client, settings, effective).WorkloadAction(ctx, positional[0], "stop", map[string]int{"timeout_seconds": int(effective.Seconds())})
		if err != nil {
			return operationError(err)
		}
		return printWorkload(settings, value)
	case "delete":
		flags := flagNew("workload delete")
		yes := flags.Bool("yes", false, "skip the confirmation prompt")
		positional, err := parseCommandFlags(flags, args)
		if err != nil {
			return usageError(err.Error())
		}
		if len(positional) != 1 {
			return usageError("usage: shiftgate workload delete ID [--yes]")
		}
		if err := confirmDestructive(settings, *yes, fmt.Sprintf("delete workload %s and its network presence", positional[0])); err != nil {
			return err
		}
		if err := client.DeleteWorkload(ctx, positional[0]); err != nil {
			return operationError(err)
		}
		if !settings.jsonOutput {
			_, _ = fmt.Fprintln(settings.stdout, "Workload deleted.")
		}
		return nil
	case "logs":
		flags := flagNew("workload logs")
		tail := flags.Int("tail", 200, "number of trailing lines")
		positional, err := parseCommandFlags(flags, args)
		if err != nil {
			return usageError(err.Error())
		}
		if len(positional) != 1 {
			return usageError("usage: shiftgate workload logs ID [--tail LINES]")
		}
		if *tail < 0 {
			return usageError("--tail must not be negative")
		}
		content, err := client.Logs(ctx, positional[0], *tail)
		if err != nil {
			return operationError(err)
		}
		_, _ = fmt.Fprint(settings.stdout, content)
		return nil
	default:
		return unknownSubcommandError("workload", subcommand, workloadSubcommands)
	}
}

func workloadCreate(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	flags := flagNew("workload create")
	root := flags.String("path", "", "explicit workload root")
	working := flags.String("workdir", "", "working directory")
	start := flags.Bool("start", false, "start immediately")
	network := flags.String("network", string(model.NetworkReconnect), "preserve, reconnect, or drain")
	devicePolicy := flags.String("device-policy", string(model.DeviceRejectIncompatible), "reject_incompatible or warn_incompatible")
	cpu := flags.Float64("cpu", 0, "CPU limit in cores")
	memory := flags.String("memory", "", "memory limit, for example 2GiB")
	checkpointEvery := flags.String("checkpoint-every", "", "take a checkpoint this often while the workload runs, for example 10m")
	checkpointKeep := flags.Int("checkpoint-keep", 0, "with --checkpoint-every: retain at most this many of the workload's checkpoints (0 keeps all)")
	var environment stringValues
	var gpus gpuValues
	flags.Var(&environment, "env", "environment variable KEY=VALUE; repeatable")
	flags.Var(&gpus, "gpu", "required GPU, fields vendor=,model=,memory=,compute=,runtime=,restore=; repeatable")
	positional, err := parseCommandFlags(flags, arguments)
	if err != nil {
		return usageError(err.Error())
	}
	if len(positional) == 0 {
		return usageError("usage: shiftgate workload create NAME --path PATH -- COMMAND [ARGS...]")
	}
	name := positional[0]
	command := positional[1:]
	if *root == "" || len(command) == 0 {
		return usageError("an explicit --path and command after -- are required")
	}
	if *cpu < 0 || math.IsNaN(*cpu) || math.IsInf(*cpu, 0) {
		return usageError("--cpu must be a non-negative number of cores")
	}
	switch model.DevicePolicy(*devicePolicy) {
	case model.DeviceRejectIncompatible, model.DeviceWarnIncompatible:
	default:
		return usageError("device policy must be reject_incompatible or warn_incompatible")
	}
	switch model.NetworkPolicy(*network) {
	case model.NetworkPreserve, model.NetworkReconnect, model.NetworkDrain:
	default:
		return usageError("network policy must be preserve, reconnect, or drain")
	}
	absRoot, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	if *working == "" {
		*working = absRoot
	} else {
		*working, err = filepath.Abs(*working)
		if err != nil {
			return err
		}
	}
	memoryBytes, err := parseBytes(*memory)
	if err != nil {
		return usageError(err.Error())
	}
	var checkpointPolicy *model.CheckpointPolicySpec
	if *checkpointEvery != "" {
		interval, err := time.ParseDuration(*checkpointEvery)
		if err != nil {
			return usageError("--checkpoint-every must be a duration, for example 10m")
		}
		policy := &model.CheckpointPolicySpec{IntervalSeconds: int(interval.Seconds()), KeepLast: *checkpointKeep}
		if err := policy.Validate(); err != nil {
			return usageError(err.Error())
		}
		checkpointPolicy = policy
	} else if *checkpointKeep != 0 {
		return usageError("--checkpoint-keep requires --checkpoint-every")
	}
	envMap := make(map[string]string)
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return usageError("environment values must use KEY=VALUE")
		}
		envMap[key] = value
	}
	spec := model.WorkloadSpec{
		Name: name, Command: command, RootPath: absRoot, WorkingDir: *working,
		Environment: envMap, UID: os.Geteuid(), GID: os.Getegid(),
		NetworkPolicy:    model.NetworkPolicy(*network),
		DevicePolicy:     model.DevicePolicy(*devicePolicy),
		Resources:        model.ResourceRequirements{CPUCount: *cpu, MemoryBytes: uint64(memoryBytes), GPUs: gpus},
		CheckpointPolicy: checkpointPolicy,
	}
	created, err := client.CreateWorkload(ctx, spec)
	if err != nil {
		return operationError(err)
	}
	if *start {
		created, err = client.WorkloadAction(ctx, created.Spec.ID, "start", struct{}{})
		if err != nil {
			return operationError(err)
		}
	}
	return printWorkload(settings, created)
}

var checkpointSubcommands = []string{"create", "list", "inspect", "mirror"}

func runCheckpoint(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if len(arguments) == 0 {
		return usageError("checkpoint subcommand is required; valid: " + strings.Join(checkpointSubcommands, ", "))
	}
	switch arguments[0] {
	case "list":
		flags := flagNew("checkpoint list")
		workload := flags.String("workload", "", "filter by workload id or name")
		positional, err := parseCommandFlags(flags, arguments[1:])
		if err != nil {
			return usageError(err.Error())
		}
		if err := rejectArgs("checkpoint list", "shiftgate checkpoint list [--workload ID|NAME]", positional); err != nil {
			return err
		}
		values, err := client.Checkpoints(ctx, "")
		if err != nil {
			return operationError(err)
		}
		if *workload != "" {
			values = filterCheckpoints(values, *workload)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, values)
		}
		writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "ID\tWORKLOAD\tKIND\tCREATED\tSIZE\tSTORED")
		for _, value := range values {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", shortID(value.ID), value.WorkloadName, value.Kind, value.CreatedAt.Local().Format(time.DateTime), humanBytes(value.PlainBytes), humanBytes(value.StoredBytes))
		}
		return writer.Flush()
	case "create":
		flags := flagNew("checkpoint create")
		leaveRunning := flags.Bool("leave-running", true, "resume the workload after the checkpoint (default)")
		stopWhenDone := flags.Bool("stop-when-done", false, "leave the workload stopped after the checkpoint (same as --leave-running=false)")
		tcpState := flags.Bool("tcp-state", false, "request CRIU TCP repair state")
		parent := flags.String("parent", "", "parent checkpoint for incremental mode")
		timeout := durationFlag(flags, "timeout", 1800*time.Second, "checkpoint timeout (accepts seconds or durations)")
		positional, err := parseCommandFlags(flags, arguments[1:])
		if err != nil {
			return usageError(err.Error())
		}
		if len(positional) != 1 {
			return usageError("usage: shiftgate checkpoint create WORKLOAD [options]")
		}
		leaveRunningSet := false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "leave-running" {
				leaveRunningSet = true
			}
		})
		if *stopWhenDone {
			if leaveRunningSet && *leaveRunning {
				return usageError("--leave-running and --stop-when-done are mutually exclusive")
			}
			*leaveRunning = false
		}
		kind := model.CheckpointFull
		if *parent != "" {
			kind = model.CheckpointIncremental
		}
		manifest, err := opClient(client, settings, *timeout).CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
			WorkloadID: positional[0], Kind: kind, ParentID: *parent,
			LeaveRunning: leaveRunning, TCPState: *tcpState, TimeoutSeconds: int(timeout.Seconds()),
		})
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, manifest)
		}
		_, _ = fmt.Fprintf(settings.stdout, "Checkpoint %s created. %s -> %s, deduplicated %s, duration %s.\n", shortID(manifest.ID), humanBytes(manifest.Metrics.PlainBytes), humanBytes(manifest.Metrics.StoredBytes), humanBytes(manifest.Metrics.DeduplicatedBytes), manifest.Metrics.Duration.Round(time.Millisecond))
		if capture := manifest.Filesystem; capture.Filesystem != "" {
			source := "the frozen workload root"
			if capture.Snapshot != "" {
				source = "a " + capture.Snapshot + " snapshot"
			}
			changes := ""
			if capture.ChangedFiles+capture.AddedFiles+capture.DeletedFiles > 0 {
				changes = fmt.Sprintf("; %d changed, %d added, %d deleted since the previous checkpoint", capture.ChangedFiles, capture.AddedFiles, capture.DeletedFiles)
			}
			_, _ = fmt.Fprintf(settings.stdout, "Filesystem: %s, captured from %s%s.\n", capture.Filesystem, source, changes)
		}
		if len(manifest.Dependencies) > 0 {
			outside := 0
			for _, dependency := range manifest.Dependencies {
				if !dependency.InsideRoot {
					outside++
				}
			}
			_, _ = fmt.Fprintf(settings.stdout, "Dependencies: %d of %d resolved files live outside the workload root and do not travel with the checkpoint.\n", outside, len(manifest.Dependencies))
		}
		return nil
	case "mirror":
		flags := flagNew("checkpoint mirror")
		positional, err := parseCommandFlags(flags, arguments[1:])
		if err != nil {
			return usageError(err.Error())
		}
		if len(positional) != 1 {
			return usageError("usage: shiftgate checkpoint mirror ID")
		}
		result, err := client.MirrorCheckpoint(ctx, positional[0])
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, result)
		}
		_, _ = fmt.Fprintf(settings.stdout, "Checkpoint %s mirrored: %d objects, %s.\n", shortID(result.CheckpointID), result.Objects, humanBytes(result.Bytes))
		return nil
	case "inspect":
		flags := flagNew("checkpoint inspect")
		positional, err := parseCommandFlags(flags, arguments[1:])
		if err != nil {
			return usageError(err.Error())
		}
		if len(positional) != 1 {
			return usageError("usage: shiftgate checkpoint inspect ID")
		}
		manifest, err := client.Checkpoint(ctx, positional[0])
		if err != nil {
			return operationError(err)
		}
		return writeJSON(settings.stdout, manifest)
	default:
		return unknownSubcommandError("checkpoint", arguments[0], checkpointSubcommands)
	}
}

// filterCheckpoints keeps the checkpoints of one workload, addressed by id or
// by the workload name as printed in the WORKLOAD column. The agent's query
// filter matches workload ids only, so a name filter (the form the docs and
// demos use) is resolved here against the summaries instead of silently
// printing an empty table.
func filterCheckpoints(values []checkpoint.Summary, workload string) []checkpoint.Summary {
	filtered := make([]checkpoint.Summary, 0, len(values))
	for _, value := range values {
		if value.WorkloadID == workload || value.WorkloadName == workload {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func runRestore(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	flags := flagNew("restore")
	timeout := durationFlag(flags, "timeout", 1800*time.Second, "restore timeout (accepts seconds or durations)")
	lazy := flags.Bool("lazy", false, "start the process before its memory is fully loaded; pages stream in on demand (needs userfaultfd)")
	positional, err := parseCommandFlags(flags, arguments)
	if err != nil {
		return usageError(err.Error())
	}
	if len(positional) != 1 {
		return usageError("usage: shiftgate restore CHECKPOINT [--lazy] [--timeout DURATION]")
	}
	record, err := opClient(client, settings, *timeout).Restore(ctx, positional[0], agentclient.RestoreRequest{TimeoutSeconds: int(timeout.Seconds()), Lazy: *lazy})
	if err != nil {
		return operationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, record)
	}
	_, _ = fmt.Fprintf(settings.stdout, "Checkpoint restored and committed. Process PID: %d\n", record.PID)
	// The same clock measures both paths, so the two numbers can be compared:
	// eager pays the full memory load before the process exists, lazy pays it
	// as the workload runs.
	_, _ = fmt.Fprintf(settings.stdout, "time to first execution: %d ms%s\n", record.TimeToFirstExecutionMS,
		map[bool]string{true: " (lazy: memory pages are still streaming in on demand)", false: ""}[record.Lazy])
	return nil
}

func runMigrate(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	flags := flagNew("migrate")
	destination := flags.String("to", "", "destination peer agent URL")
	machineID := flags.String("machine-id", "", "expected destination machine id")
	serverName := flags.String("server-name", "", "TLS server name")
	mode := flags.String("mode", string(model.MigrationCold), "cold or live")
	preCopyPasses := flags.Int("pre-copy-passes", 0, "live mode: cap on pre-dump passes before the freeze (0 = single pass)")
	dryRun := flags.Bool("dry-run", false, "check whether the migration would be admitted and exit; nothing is moved")
	preflight := flags.Bool("preflight", false, "alias for --dry-run")
	timeout := durationFlag(flags, "timeout", 7200*time.Second, "migration timeout (accepts seconds or durations)")
	wait := flags.Bool("wait", true, "wait for completion")
	positional, err := parseCommandFlags(flags, arguments)
	if err != nil {
		return usageError(err.Error())
	}
	if len(positional) != 1 {
		return usageError("usage: shiftgate migrate WORKLOAD --to https://HOST:PORT [options]")
	}
	workload := positional[0]
	if *destination == "" {
		return usageError("--to is required")
	}
	if *preCopyPasses < 0 || *preCopyPasses > 16 {
		return usageError("--pre-copy-passes must be between 0 and 16")
	}
	switch model.MigrationMode(*mode) {
	case model.MigrationCold, model.MigrationLive:
	default:
		return usageError("--mode must be cold or live")
	}
	// Migrations run cross-machine for up to the operation timeout; the
	// shared request timeout must not cut the synchronous pieces (preflight,
	// the create call) short.
	client = opClient(client, settings, *timeout)
	resolvedDestination, err := resolveDestination(*destination, *machineID, *serverName)
	if err != nil {
		return usageError(err.Error())
	}
	input := agentclient.MigrationCreateRequest{
		WorkloadID:  workload,
		Destination: resolvedDestination,
		Mode:        model.MigrationMode(*mode), PreCopyPasses: *preCopyPasses, TimeoutSeconds: int(timeout.Seconds()),
	}
	if *dryRun || *preflight {
		return runMigrateDryRun(ctx, client, settings, input)
	}
	// Live mode is a paid entitlement; refuse before anything freezes. The
	// agent-side gate is the final authority (it also covers requests that
	// never see this CLI).
	if input.Mode == model.MigrationLive {
		if err := checkLiveMigrationEntitled(ctx, settings); err != nil {
			return err
		}
	}
	record, err := client.CreateMigration(ctx, input)
	if err != nil {
		return operationError(err)
	}
	if !*wait {
		if settings.jsonOutput {
			return writeJSON(settings.stdout, record)
		}
		_, _ = fmt.Fprintln(settings.stdout, record.ID)
		return nil
	}
	if err := waitForMigration(ctx, client, settings, record.ID); err != nil {
		return err
	}
	// Post-success referral nudge after the third move in 30 days (plan 1
	// BR-11): stderr only, suppressed by --json and SHIFT_NO_HINTS=1.
	maybeReferralHint(ctx, settings)
	return nil
}

// runMigrateDryRun is `migrate --dry-run`: the agent reaches the destination
// over the peer channel, runs the same compatibility check a real migration
// would run on the same inputs, and reports what it found. Nothing is
// frozen, moved, or recorded. Exit 0 means the migration would be admitted
// (warnings allowed); exit 3 means the destination is incompatible and the
// migration would be rejected.
func runMigrateDryRun(ctx context.Context, client *agentclient.Client, settings options, input agentclient.MigrationCreateRequest) error {
	result, err := client.PreflightMigration(ctx, input)
	if err != nil {
		return operationError(err)
	}
	if settings.jsonOutput {
		if err := writeJSON(settings.stdout, result); err != nil {
			return err
		}
	} else {
		printPreflight(settings.stdout, result)
	}
	if !result.Report.Compatible {
		return exitError{code: 3, err: errors.New("destination is incompatible: the migration would be rejected")}
	}
	return nil
}

// printPreflight renders the compatibility report the way an operator reads
// it: the two machines, the network plan the migration would apply, the
// verdict, then every rejection and warning with its remedy.
func printPreflight(output io.Writer, result migration.PreflightResult) {
	mode := string(result.Mode)
	if mode == "" {
		mode = string(model.MigrationCold)
	}
	_, _ = fmt.Fprintf(output, "Preflight for workload %s (mode %s)\n", result.Workload.Name, mode)
	_, _ = fmt.Fprintf(output, "destination: %s on %s — %s/%s, kernel %s, CRIU %s, %s memory\n",
		shortID(result.Destination.MachineID), result.Destination.Hostname,
		result.Destination.OS, result.Destination.Architecture, result.Destination.Kernel,
		criuVersion(result.Destination.CRIU), humanBytes(int64(result.Destination.MemoryBytes)))
	_, _ = fmt.Fprintf(output, "source:      %s on %s — %s/%s, kernel %s\n",
		shortID(result.SourceMachine.MachineID), result.SourceMachine.Hostname,
		result.SourceMachine.OS, result.SourceMachine.Architecture, result.SourceMachine.Kernel)
	_, _ = fmt.Fprintf(output, "network:     %s\n", result.Network.Summary)
	verdict := "yes — the migration would be admitted"
	if !result.Report.Compatible {
		verdict = "no — the migration would be rejected"
	}
	_, _ = fmt.Fprintf(output, "compatible:  %s\n", verdict)
	var rejections, warnings []model.CompatibilityIssue
	for _, issue := range result.Report.Issues {
		if issue.Severity == "error" {
			rejections = append(rejections, issue)
		} else {
			warnings = append(warnings, issue)
		}
	}
	if len(rejections) > 0 {
		_, _ = fmt.Fprintf(output, "\nrejections (%d) — each of these fails the migration:\n", len(rejections))
		for _, issue := range rejections {
			_, _ = fmt.Fprintf(output, "  %s [%s] %s — %s\n", issue.Code, issue.Resource, issue.Description, issue.Adaptation)
		}
	}
	if len(warnings) > 0 {
		_, _ = fmt.Fprintf(output, "\nwarnings (%d) — the migration proceeds, but check these:\n", len(warnings))
		for _, issue := range warnings {
			_, _ = fmt.Fprintf(output, "  %s [%s] %s — %s\n", issue.Code, issue.Resource, issue.Description, issue.Adaptation)
		}
	}
	if len(rejections) == 0 && len(warnings) == 0 {
		_, _ = fmt.Fprintln(output, "\nno compatibility issues found")
	}
}

// runFailover is the source-side view of warm-standby replication: for every
// workload with a failover policy, what was pushed to which standby and how
// the last attempt went. `shiftgate standby` on the standby machine is the
// other half of the picture.
func runFailover(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	flags := flagNew("failover")
	positional, err := parseCommandFlags(flags, arguments)
	if err != nil {
		return usageError(err.Error())
	}
	if len(positional) > 1 || (len(positional) == 1 && positional[0] != "status") {
		return usageError("usage: shiftgate failover [status]")
	}
	entries, err := client.ReplicationEntries(ctx)
	if err != nil {
		return operationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, entries)
	}
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(settings.stdout, "No failover policies are active. Install one with: shiftgate workload failover NAME --to URL")
		return nil
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "WORKLOAD\tSTANDBY\tLAST CHECKPOINT\tPUSHED\tKEEP\tERROR")
	for _, entry := range entries {
		pushed := "-"
		if !entry.LastPushAt.IsZero() {
			pushed = entry.LastPushAt.Local().Format(time.DateTime)
		}
		keep := "all"
		if entry.KeepLast > 0 {
			keep = strconv.Itoa(entry.KeepLast)
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
			workloadLabel(entry.WorkloadName, entry.WorkloadID), entry.StandbyURL,
			shortID(entry.LastCheckpointID), pushed, keep, entry.LastError)
	}
	return writer.Flush()
}

// workloadLabel names a workload the way an operator knows it: the name when
// the record carries one, the id otherwise.
func workloadLabel(name, id string) string {
	if name != "" {
		return name
	}
	return shortID(id)
}

// criuVersion renders the destination's CRIU state honestly: a missing
// install, an unknown version, and a failed health check are all said out
// loud rather than papered over.
func criuVersion(caps model.CRIUCapabilities) string {
	if !caps.Installed {
		return "not installed"
	}
	version := caps.Version
	if version == "" {
		version = "unknown version"
	}
	if !caps.Healthy {
		version += " (failed its capability check)"
	}
	return version
}

var standbySubcommands = []string{"list", "trigger"}

// runStandby is the standby-side view of warm-standby failover: the duties
// this machine holds, and the explicit trigger that restores one here.
func runStandby(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if len(arguments) == 0 {
		return usageError("usage: shiftgate standby list\n       shiftgate standby trigger WORKLOAD [--checkpoint ID] [--timeout DURATION]")
	}
	switch arguments[0] {
	case "list":
		return standbyList(ctx, client, settings, arguments[1:])
	case "trigger":
		return standbyTrigger(ctx, settings, arguments[1:])
	default:
		return unknownSubcommandError("standby", arguments[0], standbySubcommands)
	}
}

// standbyList prints the duty table, then the facts a table cannot hold:
// whether the death watch is armed at all, and what happened on the duties
// that already failed over or keep failing to.
func standbyList(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if err := rejectArgs("standby list", "shiftgate standby list", arguments); err != nil {
		return err
	}
	result, err := client.StandbyStatus(ctx)
	if err != nil {
		return operationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, result)
	}
	if len(result.Duties) == 0 {
		_, _ = fmt.Fprintln(settings.stdout, "This machine stands warm standby for no workloads.")
		return nil
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "WORKLOAD\tSTATE\tSOURCE\tCHECKPOINT\tHELD\tKEEP")
	for _, duty := range result.Duties {
		keep := "all"
		if duty.KeepLast > 0 {
			keep = strconv.Itoa(duty.KeepLast)
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
			workloadLabel(duty.WorkloadName, duty.WorkloadID), duty.State,
			shortID(duty.SourceMachineID), shortID(duty.LastCheckpointID),
			duty.HeldAt.Local().Format(time.DateTime), keep)
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	for _, duty := range result.Duties {
		if duty.State == model.StandbyFailedOver {
			_, _ = fmt.Fprintf(settings.stdout, "%s failed over %s from checkpoint %s: %s\n",
				workloadLabel(duty.WorkloadName, duty.WorkloadID),
				duty.FailoverAt.Local().Format(time.DateTime), shortID(duty.LastCheckpointID), duty.FailoverReason)
		}
		if duty.LastFailoverError != "" {
			_, _ = fmt.Fprintf(settings.stdout, "%s: last failover attempt failed: %s (next attempt %s)\n",
				workloadLabel(duty.WorkloadName, duty.WorkloadID), duty.LastFailoverError,
				duty.NextAttemptAt.Local().Format(time.DateTime))
		}
	}
	if !result.AutomaticFailover {
		_, _ = fmt.Fprintln(settings.stdout, "automatic failover: disabled — no control plane is configured, so this agent cannot confirm a source's death; only 'standby trigger' can act")
	}
	return nil
}

// standbyTrigger commands an explicit failover: the newest replicated
// checkpoint (or --checkpoint) is restored here, now, with no death
// confirmation — the command is the confirmation. The agent probes the
// source first and reports a live one, because that means two live copies.
func standbyTrigger(ctx context.Context, settings options, arguments []string) error {
	flags := flagNew("standby trigger")
	checkpointID := flags.String("checkpoint", "", "restore this replicated checkpoint instead of the duty's newest")
	lazy := flags.Bool("lazy", false, "start the failed-over process before its memory is fully loaded; pages stream in on demand (needs userfaultfd)")
	timeout := durationFlag(flags, "timeout", 15*time.Minute, "failover timeout; a restore can take minutes, so this overrides the global --timeout")
	positional, err := parseCommandFlags(flags, arguments)
	if err != nil {
		return usageError(err.Error())
	}
	if len(positional) != 1 {
		return usageError("usage: shiftgate standby trigger WORKLOAD_ID [--checkpoint ID] [--lazy] [--timeout DURATION]")
	}
	// The trigger runs a full restore synchronously; the shared client's
	// 30-second default would cut it off long before the agent finished.
	triggerClient, err := agentclient.New(settings.agentEndpoint, *timeout)
	if err != nil {
		return exitError{code: 2, err: err}
	}
	result, err := triggerClient.TriggerStandbyFailover(ctx, positional[0], *checkpointID, *lazy)
	if err != nil {
		return operationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, result)
	}
	if result.Warning != "" {
		_, _ = fmt.Fprintln(settings.stderr, "warning: "+result.Warning)
	}
	duty := result.Duty
	_, _ = fmt.Fprintf(settings.stdout, "Failover complete: workload %s restored from checkpoint %s (restore %s).\n",
		workloadLabel(duty.WorkloadName, duty.WorkloadID), shortID(duty.LastCheckpointID), shortID(duty.FailoverRestoreID))
	if duty.FailoverReason != "" {
		_, _ = fmt.Fprintf(settings.stdout, "reason: %s\n", duty.FailoverReason)
	}
	return nil
}

func runStatus(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	flags := flagNew("status")
	positional, err := parseCommandFlags(flags, arguments)
	if err != nil {
		return usageError(err.Error())
	}
	if len(positional) > 1 {
		return usageError("usage: shiftgate status [MIGRATION]")
	}
	if len(positional) == 1 {
		record, err := client.Migration(ctx, positional[0])
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, record)
		}
		printMigration(settings.stdout, record)
		return nil
	}
	values, err := client.Migrations(ctx)
	if err != nil {
		return operationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, values)
	}
	if len(values) == 0 {
		_, _ = fmt.Fprintln(settings.stdout, "No migrations yet. Start one with: shiftgate migrate WORKLOAD --to ALIAS|https://HOST:PORT")
		return nil
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tWORKLOAD\tSTAGE\tPROGRESS\tDESTINATION\tUPDATED")
	for _, value := range values {
		progress := 0.0
		if len(value.Events) > 0 {
			progress = value.Events[len(value.Events)-1].Progress * 100
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%.0f%%\t%s\t%s\n", shortID(value.ID), shortID(value.WorkloadID), value.Stage, progress, shortID(value.Destination.MachineID), value.UpdatedAt.Local().Format(time.DateTime))
	}
	return writer.Flush()
}

// waitForMigration polls until the migration reaches a terminal stage. Progress
// is rendered from the agent's real event stream, never estimated: on a
// terminal the bar redraws in place; anywhere else each event prints one
// greppable line.
func waitForMigration(ctx context.Context, client *agentclient.Client, settings options, id string) error {
	lastSequence := uint64(0)
	printer := newProgressPrinter(settings.stdout)
	for {
		record, err := client.Migration(ctx, id)
		if err != nil {
			return operationError(err)
		}
		terminal := record.Stage == model.MigrationCompleted || record.Stage == model.MigrationRolledBack ||
			record.Stage == model.MigrationFailed || record.Stage == model.MigrationCancelled
		if settings.jsonOutput {
			// Machine-readable mode writes the terminal record exactly once,
			// then falls through to the same exit-code contract human mode
			// enforces: only a clean COMPLETED may exit 0. Scripts branch on
			// that digit, so the failure path must never report success here.
			if terminal {
				if err := writeJSON(settings.stdout, record); err != nil {
					return err
				}
			}
		} else if len(record.Events) > 0 {
			event := record.Events[len(record.Events)-1]
			if event.Sequence != lastSequence {
				lastSequence = event.Sequence
				printer.event(record.Stage, event)
			}
		}
		switch record.Stage {
		case model.MigrationCompleted:
			printer.end()
			// A completed stage that still carries a failure code is not a
			// success: report it as the failure it is, never "successful".
			if record.FailureCode != "" {
				if !settings.jsonOutput {
					_, _ = fmt.Fprintf(settings.stdout, "Migration finished with %s: %s\n", record.FailureCode, record.FailureReason)
				}
				return exitError{code: 5, err: errors.New(record.FailureReason)}
			}
			if !settings.jsonOutput {
				_, _ = fmt.Fprintf(settings.stdout, "Migration successful. Downtime: %s, transferred: %s, deduplicated: %s.\n", record.Metrics.Downtime.Round(time.Millisecond), humanBytes(record.Metrics.TransferredBytes), humanBytes(record.Metrics.DeduplicatedBytes))
				if record.Network.Identity.IP != "" {
					sockets := "listeners re-established; connections must reconnect"
					if record.Network.SocketsCarried {
						sockets = "TCP state carried with the checkpoint"
					}
					_, _ = fmt.Fprintf(settings.stdout, "Network: virtual address %s, %s.\n", record.Network.Identity.IP, sockets)
				}
			}
			return nil
		case model.MigrationRolledBack, model.MigrationFailed, model.MigrationCancelled:
			printer.end()
			return exitError{code: 5, err: fmt.Errorf("migration %s: %s", record.Stage, record.FailureReason)}
		}
		select {
		case <-ctx.Done():
			printer.end()
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// progressPrinter renders migration progress from the events the agent
// actually emitted. On a terminal it redraws a single line in place — a bar
// that fills as real progress arrives; on any other writer (a pipe, a file,
// JSON-disabled logs) it prints one line per event so output stays greppable.
type progressPrinter struct {
	writer   io.Writer
	terminal bool
}

func newProgressPrinter(writer io.Writer) *progressPrinter {
	printer := &progressPrinter{writer: writer}
	if file, ok := writer.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			printer.terminal = true
		}
	}
	return printer
}

// event draws one progress step.
func (printer *progressPrinter) event(stage model.MigrationStage, event model.MigrationEvent) {
	if !printer.terminal {
		_, _ = fmt.Fprintf(printer.writer, "[%s] %3.0f%% %s", stage, event.Progress*100, event.Message)
		if event.BytesTotal > 0 {
			_, _ = fmt.Fprintf(printer.writer, " (%s / %s)", humanBytes(event.BytesDone), humanBytes(event.BytesTotal))
		}
		_, _ = fmt.Fprintln(printer.writer)
		return
	}
	const width = 30
	filled := int(event.Progress*float64(width) + 0.5)
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	_, _ = fmt.Fprintf(printer.writer, "\r[%s%s] %3.0f%% %s %s", strings.Repeat("=", filled), strings.Repeat(" ", width-filled), event.Progress*100, stage, event.Message)
	if event.BytesTotal > 0 {
		_, _ = fmt.Fprintf(printer.writer, " (%s / %s)", humanBytes(event.BytesDone), humanBytes(event.BytesTotal))
	}
}

// end closes an in-place bar so whatever follows starts on a fresh line.
func (printer *progressPrinter) end() {
	if printer.terminal {
		_, _ = fmt.Fprintln(printer.writer)
	}
}

func printWorkload(settings options, workload model.Workload) error {
	if settings.jsonOutput {
		return writeJSON(settings.stdout, workload)
	}
	pid := "-"
	if workload.Process != nil {
		pid = strconv.Itoa(workload.Process.PID)
	}
	_, _ = fmt.Fprintf(settings.stdout, "%s: %s (PID %s)\n", workload.Spec.Name, workload.Status, pid)
	return nil
}

func printMigration(writer io.Writer, record model.Migration) {
	progress := 0.0
	message := ""
	if len(record.Events) > 0 {
		event := record.Events[len(record.Events)-1]
		progress = event.Progress * 100
		message = event.Message
	}
	_, _ = fmt.Fprintf(writer, "%s %s %.0f%% %s\n", record.ID, record.Stage, progress, message)
}

var forkSubcommands = []string{"list", "inspect"}

func runFork(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if len(arguments) == 0 {
		return usageError("usage: shiftgate fork WORKLOAD [--name NAME] [--root PATH] [--checkpoint ID] [--activate]\n       shiftgate fork list\n       shiftgate fork inspect FORK")
	}
	switch arguments[0] {
	case "list":
		if err := rejectArgs("fork list", "shiftgate fork list", arguments[1:]); err != nil {
			return err
		}
		records, err := client.Forks(ctx)
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, records)
		}
		writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "FORK\tSTATE\tSOURCE\tWORKLOAD\tROOT\tGEN\tACTIVE")
		for _, record := range records {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%d\t%t\n", record.ID, record.State,
				record.SourceWorkloadID, record.ForkWorkloadID, record.RootPath, record.Generation, record.Activated)
		}
		return writer.Flush()
	case "inspect":
		flags := flagNew("fork inspect")
		positional, err := parseCommandFlags(flags, arguments[1:])
		if err != nil {
			return usageError(err.Error())
		}
		if len(positional) != 1 {
			return usageError("usage: shiftgate fork inspect FORK")
		}
		record, err := client.Fork(ctx, positional[0])
		if err != nil {
			return operationError(err)
		}
		return writeJSON(settings.stdout, record)
	}
	flags := flagNew("fork")
	name := flags.String("name", "", "name for the forked workload")
	root := flags.String("root", "", "root directory for the forked workload")
	checkpointID := flags.String("checkpoint", "", "fork from this checkpoint instead of the current state")
	activate := flags.Bool("activate", false, "also restore the forked process tree so both workloads run")
	timeout := durationFlag(flags, "timeout", 1800*time.Second, "fork timeout (accepts seconds or durations)")
	positional, err := parseCommandFlags(flags, arguments)
	if err != nil {
		return usageError(err.Error())
	}
	if len(positional) != 1 {
		return usageError("usage: shiftgate fork WORKLOAD [--name NAME] [--root PATH] [--checkpoint ID] [--activate]")
	}
	input := agentclient.ForkRequest{Name: *name, RootPath: *root, CheckpointID: *checkpointID, Activate: *activate, TimeoutSeconds: int(timeout.Seconds())}
	record, err := opClient(client, settings, *timeout).ForkWorkload(ctx, positional[0], input)
	if err != nil {
		return operationError(withSubcommandTypoHint(err, positional[0], forkSubcommands))
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, record)
	}
	_, _ = fmt.Fprintf(settings.stdout, "Forked workload %s from checkpoint %s\n", record.ForkWorkloadID, record.SourceCheckpointID)
	_, _ = fmt.Fprintf(settings.stdout, "  fork id:     %s\n  root:        %s\n  checkpoint:  %s\n  generation:  %d\n",
		record.ID, record.RootPath, record.ForkCheckpointID, record.Generation)
	if record.Activated {
		_, _ = fmt.Fprintf(settings.stdout, "  process:     running as PID %d\n", record.PID)
	} else {
		_, _ = fmt.Fprintln(settings.stdout, "  process:     not started; run shiftgate restore with the fork checkpoint or shiftgate fork --activate")
	}
	return nil
}

var cloneSubcommands = []string{"list", "inspect", "rollback"}

func runClone(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if len(arguments) == 0 {
		return usageError("usage: shiftgate clone CHECKPOINT [--count N] [--prefix NAME] [--parallel N] [--timeout DURATION] [--wait-ready] [--wait-timeout DURATION]\n       shiftgate clone list\n       shiftgate clone inspect CLONE\n       shiftgate clone rollback CLONE")
	}
	switch arguments[0] {
	case "list":
		if err := rejectArgs("clone list", "shiftgate clone list", arguments[1:]); err != nil {
			return err
		}
		records, err := client.Clones(ctx)
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, records)
		}
		writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "CLONE\tSTATE\tCOUNT\tSOURCE WORKLOAD\tRUNNING\tDURATION\tCLONED FILES")
		for _, record := range records {
			running := 0
			for _, member := range record.Members {
				if member.PID != 0 {
					running++
				}
			}
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%d\t%s\t%d/%d\t%dms\t%d\n", record.ID, record.State,
				record.Count, record.SourceWorkloadID, running, record.Count, record.DurationMS, record.ClonedFiles)
		}
		return writer.Flush()
	case "inspect":
		flags := flagNew("clone inspect")
		positional, err := parseCommandFlags(flags, arguments[1:])
		if err != nil {
			return usageError(err.Error())
		}
		if len(positional) != 1 {
			return usageError("usage: shiftgate clone inspect CLONE")
		}
		record, err := client.Clone(ctx, positional[0])
		if err != nil {
			return operationError(err)
		}
		return writeJSON(settings.stdout, record)
	case "rollback":
		flags := flagNew("clone rollback")
		yes := flags.Bool("yes", false, "skip the confirmation prompt")
		positional, err := parseCommandFlags(flags, arguments[1:])
		if err != nil {
			return usageError(err.Error())
		}
		if len(positional) != 1 {
			return usageError("usage: shiftgate clone rollback CLONE [--yes]")
		}
		if err := confirmDestructive(settings, *yes, fmt.Sprintf("roll back clone set %s and remove its member workloads", positional[0])); err != nil {
			return err
		}
		record, err := client.CloneRollback(ctx, positional[0])
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, record)
		}
		_, _ = fmt.Fprintf(settings.stdout, "Clone set %s rolled back: %d workload(s) removed\n", record.ID, record.Count)
		return nil
	}
	flags := flagNew("clone")
	count := flags.Int("count", 1, "how many clones to derive from the checkpoint (1-128)")
	prefix := flags.String("prefix", "", "name prefix for the clones (default: derived from the source workload)")
	parallel := flags.Int("parallel", 4, "how many clones to restore at once (1-16)")
	timeout := durationFlag(flags, "timeout", 3600*time.Second, "clone timeout (accepts seconds or durations)")
	waitReady := flags.Bool("wait-ready", false, "block until every spawned workload passes its health check (or is running), then print per-worker ready latency")
	waitTimeout := durationFlag(flags, "wait-timeout", 120*time.Second, "how long --wait-ready may block before failing loudly (accepts seconds or durations)")
	positional, err := parseCommandFlags(flags, arguments)
	if err != nil {
		return usageError(err.Error())
	}
	if len(positional) != 1 {
		return usageError("usage: shiftgate clone CHECKPOINT [--count N] [--prefix NAME] [--parallel N] [--wait-ready] [--wait-timeout DURATION]")
	}
	if *count < 1 || *count > 128 {
		return usageError("--count must be between 1 and 128")
	}
	if *parallel < 1 || *parallel > 16 {
		return usageError("--parallel must be between 1 and 16")
	}
	input := agentclient.CloneRequest{Count: *count, NamePrefix: *prefix, Parallel: *parallel, TimeoutSeconds: int(timeout.Seconds())}
	record, err := opClient(client, settings, *timeout).CloneCheckpoint(ctx, positional[0], input)
	if err != nil {
		return operationError(withSubcommandTypoHint(err, positional[0], cloneSubcommands))
	}
	if *waitReady {
		statuses, waitErr := waitCloneReady(ctx, client, record, *waitTimeout)
		if settings.jsonOutput {
			payload := map[string]any{"clone": record, "workers": statuses}
			if waitErr != nil {
				payload["error"] = waitErr.Error()
			}
			_ = writeJSON(settings.stdout, payload)
		} else {
			for _, status := range statuses {
				mark := "ready"
				if !status.Ready {
					mark = "NOT READY"
				}
				_, _ = fmt.Fprintf(settings.stdout, "  %-28s %-9s %6dms  %s\n", status.Name, mark, status.LatencyMS, status.Detail)
			}
		}
		if waitErr != nil {
			// Loudly: the clone set stands (originals untouched); the exit
			// names the workers that never came ready.
			failed := make([]string, 0)
			for _, status := range statuses {
				if !status.Ready {
					failed = append(failed, status.Name)
				}
			}
			return exitError{code: 1, err: fmt.Errorf("wait-ready: %v (not ready: %s)", waitErr, strings.Join(failed, ", "))}
		}
		return nil
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, record)
	}
	_, _ = fmt.Fprintf(settings.stdout, "Cloned %d workload(s) from checkpoint %s\n", record.Count, record.CheckpointID)
	_, _ = fmt.Fprintf(settings.stdout, "  clone set:   %s\n  duration:    %dms\n  cloned files (copy-on-write): %d, copied: %d\n",
		record.ID, record.DurationMS, record.ClonedFiles, record.CopiedFiles)
	for _, member := range record.Members {
		_, _ = fmt.Fprintf(settings.stdout, "  %-28s pid %-6d root %s\n", member.Name, member.PID, member.RootPath)
	}
	_, _ = fmt.Fprintln(settings.stdout, "  state is stored once for the whole set; each workload owns its root and every checkpoint it takes afterwards")
	return nil
}

// opClient gives one synchronous long operation its own request deadline.
// The shared client's default (30s) exists for quick commands; a checkpoint,
// restore, fork, clone, or migration runs for the minutes-to-hours the
// operation budget allows and must never be cut off client-side while the
// agent is still working. The client gets the operation budget plus slack so
// the agent's own timeout fires first.
func opClient(client *agentclient.Client, settings options, operationTimeout time.Duration) *agentclient.Client {
	deadline := operationTimeout + 30*time.Second
	if settings.timeout > deadline {
		deadline = settings.timeout
	}
	return client.WithTimeout(deadline)
}

// withSubcommandTypoHint turns a not-found failure into an actionable one
// without ever blocking a real name: `shiftgate fork best` forks a workload
// named "best", and only a genuine not-found on "lst" gets the note that
// "list" exists. The old pre-dispatch refusal treated every near-miss as a
// typo and made real workloads unforkable.
func withSubcommandTypoHint(err error, argument string, valid []string) error {
	if err == nil {
		return nil
	}
	var api *agentclient.APIError
	if errors.As(err, &api) && (api.Status == http.StatusNotFound || strings.Contains(api.Code, "NOT_FOUND")) {
		if hint := subcommandTypo(argument, valid); hint != "" {
			return fmt.Errorf("%w; if %q was meant as a subcommand: %s", err, argument, strings.Join(valid, ", "))
		}
	}
	return err
}

// subcommandTypo notices an argument that looks like a misspelled subcommand
// (fork lst, clone roolback) so a not-found error can suggest the fix.
func subcommandTypo(argument string, valid []string) string {
	lowered := strings.ToLower(argument)
	for _, candidate := range valid {
		if lowered != candidate && editDistance(lowered, candidate) <= 2 {
			return fmt.Sprintf("%q looks like a misspelling of the %q subcommand; valid: %s", argument, candidate, strings.Join(valid, ", "))
		}
	}
	return ""
}

// confirmDestructive gates a destructive operation: a terminal gets a y/N
// prompt, and scripts must say --yes explicitly — a pipeline that deletes
// workloads should never be one typo away from doing it silently.
func confirmDestructive(settings options, yes bool, action string) error {
	if yes {
		return nil
	}
	if !termio.IsTerminal(os.Stdin) {
		return usageError(fmt.Sprintf("refusing to %s without confirmation; pass --yes to confirm in scripts", action))
	}
	_, _ = fmt.Fprintf(settings.stderr, "Really %s? [y/N] ", action)
	answer, err := termio.ReadLine(os.Stdin)
	if err != nil {
		return exitError{code: 3, err: fmt.Errorf("read confirmation: %w", err)}
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	// Declining the prompt is the operator saying "this invocation was not
	// what I meant" — a usage-level outcome, exit 2, not an environment
	// failure.
	return usageError("aborted")
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func operationError(err error) error {
	var api *agentclient.APIError
	if errors.As(err, &api) {
		code := 1
		if api.Status == 404 {
			code = 4
		}
		return exitError{code: code, err: err}
	}
	return exitError{code: 3, err: err}
}

func usageError(message string) error {
	return exitError{code: 2, err: errors.New(message)}
}

func defaultAgentEndpoint() string {
	if value := os.Getenv("SHIFT_AGENT_ENDPOINT"); value != "" {
		return value
	}
	if value := os.Getenv("SHIFT_AGENT"); value != "" {
		return value
	}
	return "unix:///run/shift/agent.sock"
}

func shortID(value string) string {
	if value == "" {
		return "-"
	}
	if len(value) > 8 {
		return value[:8]
	}
	return value
}

func humanBytes(value int64) string {
	if value < 0 {
		return "-"
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	number := float64(value)
	unit := 0
	for number >= 1024 && unit < len(units)-1 {
		number /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", value, units[unit])
	}
	return fmt.Sprintf("%.1f %s", number, units[unit])
}

func parseBytes(value string) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	normalized := strings.ToUpper(strings.TrimSpace(value))
	multipliers := []struct {
		suffix string
		value  int64
	}{
		{"TIB", 1 << 40}, {"TB", 1_000_000_000_000}, {"GIB", 1 << 30}, {"GB", 1_000_000_000},
		{"MIB", 1 << 20}, {"MB", 1_000_000}, {"KIB", 1 << 10}, {"KB", 1_000}, {"B", 1},
	}
	for _, multiplier := range multipliers {
		if strings.HasSuffix(normalized, multiplier.suffix) {
			number := strings.TrimSpace(strings.TrimSuffix(normalized, multiplier.suffix))
			parsed, err := strconv.ParseFloat(number, 64)
			if err != nil || parsed < 0 {
				return 0, fmt.Errorf("invalid byte value %q", value)
			}
			// The byte count must be a whole number of bytes inside int64:
			// `1.5B` is a typo, and an overflowing product would wrap into a
			// negative size — both are usage errors, never garbage.
			product := parsed * float64(multiplier.value)
			if product != math.Trunc(product) || product >= math.Exp2(63) {
				return 0, fmt.Errorf("invalid byte value %q", value)
			}
			return int64(product), nil
		}
	}
	parsed, err := strconv.ParseInt(normalized, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("invalid byte value %q", value)
	}
	return parsed, nil
}

type stringValues []string

func (v *stringValues) String() string { return strings.Join(*v, ",") }
func (v *stringValues) Set(value string) error {
	*v = append(*v, value)
	return nil
}

// gpuValues collects repeatable --gpu flags. Each value is a comma-separated
// list of vendor=, model=, memory=, compute=, runtime=, and restore= fields;
// vendor is required and every other field is optional.
type gpuValues []model.GPUDevice

func (v *gpuValues) String() string {
	parts := make([]string, 0, len(*v))
	for _, gpu := range *v {
		parts = append(parts, gpu.Vendor+" "+gpu.Model)
	}
	return strings.Join(parts, "; ")
}

func (v *gpuValues) Set(value string) error {
	gpu := model.GPUDevice{}
	for _, field := range strings.Split(value, ",") {
		key, fieldValue, found := strings.Cut(field, "=")
		if !found {
			return fmt.Errorf("gpu fields must use KEY=VALUE, got %q", field)
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "vendor":
			gpu.Vendor = strings.TrimSpace(fieldValue)
		case "model":
			gpu.Model = strings.TrimSpace(fieldValue)
		case "memory":
			bytes, err := parseBytes(strings.TrimSpace(fieldValue))
			if err != nil {
				return fmt.Errorf("gpu memory: %w", err)
			}
			gpu.MemoryBytes = uint64(bytes)
		case "compute":
			gpu.ComputeCapability = strings.TrimSpace(fieldValue)
		case "runtime":
			gpu.Runtime = strings.TrimSpace(fieldValue)
		case "restore":
			gpu.CheckpointRestore = strings.TrimSpace(strings.ToLower(fieldValue)) == "true"
		default:
			return fmt.Errorf("unknown gpu field %q", key)
		}
	}
	if gpu.Vendor == "" {
		return errors.New("gpu requirements need at least vendor=, for example --gpu vendor=NVIDIA,model=H100")
	}
	*v = append(*v, gpu)
	return nil
}
