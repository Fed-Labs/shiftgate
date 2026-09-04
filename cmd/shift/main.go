package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/controlclient"
	"shift.dev/shift/internal/model"
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
		fmt.Fprintln(os.Stderr, "shift:", err)
		var exit exitError
		if errors.As(err, &exit) {
			os.Exit(exit.code)
		}
		os.Exit(1)
	}
}

func run(arguments []string, stdout, stderr io.Writer) error {
	global := flag.NewFlagSet("shiftgate", flag.ContinueOnError)
	global.SetOutput(stderr)
	settings := options{stdout: stdout, stderr: stderr, timeout: 30 * time.Second}
	global.StringVar(&settings.agentEndpoint, "agent", defaultAgentEndpoint(), "SHIFT agent endpoint")
	global.StringVar(&settings.controlURL, "control-url", os.Getenv("SHIFT_CONTROL_URL"), "SHIFT control-plane URL")
	global.StringVar(&settings.tokenStorePath, "token-store", controlclient.DefaultTokenStorePath(), "where the control-plane session is stored")
	global.BoolVar(&settings.jsonOutput, "json", false, "emit JSON")
	global.DurationVar(&settings.timeout, "timeout", 30*time.Second, "request timeout")
	if err := global.Parse(arguments); err != nil {
		return exitError{code: 2, err: err}
	}
	remaining := global.Args()
	if len(remaining) == 0 {
		printUsage(stdout)
		return exitError{code: 2, err: errors.New("a command is required")}
	}
	command := remaining[0]
	commandArgs := remaining[1:]
	if command == "version" {
		if settings.jsonOutput {
			return writeJSON(stdout, map[string]string{"version": config.Version})
		}
		fmt.Fprintln(stdout, "SHIFT", config.Version)
		return nil
	}
	if command == "completion" {
		return completion(stdout, commandArgs)
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
		return runDoctor(ctx, client, settings)
	case "machines", "machine":
		// Dual-mode by configuration: with a control plane configured the
		// fleet view is the more useful answer, and a lone agent still has
		// its local answer when none is.
		if settings.controlURL != "" {
			return runFleetMachines(ctx, settings, commandArgs)
		}
		return runMachine(ctx, client, settings)
	case "workloads":
		if settings.controlURL != "" {
			return runFleetWorkloads(ctx, settings, commandArgs)
		}
		return runWorkload(ctx, client, settings, append([]string{"list"}, commandArgs...))
	case "workload":
		return runWorkload(ctx, client, settings, commandArgs)
	case "checkpoint":
		return runCheckpoint(ctx, client, settings, commandArgs)
	case "restore":
		return runRestore(ctx, client, settings, commandArgs)
	case "fork":
		return runFork(ctx, client, settings, commandArgs)
	case "migrate":
		return runMigrate(ctx, client, settings, commandArgs)
	case "status":
		return runStatus(ctx, client, settings, commandArgs)
	case "update":
		return runUpdate(ctx, client, settings, commandArgs)
	case "logs":
		return runWorkload(ctx, client, settings, append([]string{"logs"}, commandArgs...))
	default:
		printUsage(stderr)
		return exitError{code: 2, err: fmt.Errorf("unknown command %q", command)}
	}
}

// isControlCommand reports whether a command targets the control plane rather
// than the local agent.
func isControlCommand(command string) bool {
	switch command {
	case "login", "logout", "whoami", "plans", "fleet", "marketplace", "market":
		return true
	}
	return false
}

func runDoctor(ctx context.Context, client *agentclient.Client, settings options) error {
	result, err := client.Doctor(ctx)
	if err != nil {
		return operationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, result)
	}
	for _, check := range result.Checks {
		status := "FAIL"
		if check.OK {
			status = "OK"
		}
		fmt.Fprintf(settings.stdout, "%-5s %-20s %s\n", status, check.Name, check.Message)
	}
	if !result.Healthy {
		return exitError{code: 3, err: errors.New("one or more required checks failed")}
	}
	return nil
}

func runMachine(ctx context.Context, client *agentclient.Client, settings options) error {
	machine, err := client.Machine(ctx)
	if err != nil {
		return operationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, []model.MachineCapabilities{machine})
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME\tOS\tARCH\tCPU\tMEMORY\tGPU\tRUNTIMES\tCRIU")
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
	fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", machine.Hostname, machine.Distribution, machine.Architecture, machine.CPUs, humanBytes(int64(machine.MemoryBytes)), gpu, runtimes, criu)
	return writer.Flush()
}

func runWorkload(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if len(arguments) == 0 {
		return exitError{code: 2, err: errors.New("workload subcommand is required")}
	}
	subcommand := arguments[0]
	args := arguments[1:]
	switch subcommand {
	case "list":
		values, err := client.Workloads(ctx)
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, values)
		}
		writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "NAME\tSTATUS\tPID\tROOT\tCHECKPOINT")
		for _, workload := range values {
			pid := "-"
			if workload.Process != nil {
				pid = strconv.Itoa(workload.Process.PID)
			}
			checkpointID := shortID(workload.LatestCheckpointID)
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", workload.Spec.Name, workload.Status, pid, workload.Spec.RootPath, checkpointID)
		}
		return writer.Flush()
	case "create":
		return workloadCreate(ctx, client, settings, args)
	case "inspect":
		if len(args) != 1 {
			return usageError("usage: shiftgate workload inspect ID")
		}
		value, err := client.Workload(ctx, args[0])
		if err != nil {
			return operationError(err)
		}
		return writeJSON(settings.stdout, value)
	case "start", "pause", "resume":
		if len(args) != 1 {
			return usageError("usage: shiftgate workload " + subcommand + " ID")
		}
		value, err := client.WorkloadAction(ctx, args[0], subcommand, struct{}{})
		if err != nil {
			return operationError(err)
		}
		return printWorkload(settings, value)
	case "stop":
		flags := flag.NewFlagSet("workload stop", flag.ContinueOnError)
		flags.SetOutput(settings.stderr)
		timeout := flags.Int("timeout", 10, "graceful stop timeout in seconds")
		if len(args) == 0 {
			return usageError("usage: shiftgate workload stop ID [--timeout SECONDS]")
		}
		id := args[0]
		if err := flags.Parse(args[1:]); err != nil {
			return usageError(err.Error())
		}
		value, err := client.WorkloadAction(ctx, id, "stop", map[string]int{"timeout_seconds": *timeout})
		if err != nil {
			return operationError(err)
		}
		return printWorkload(settings, value)
	case "delete":
		if len(args) != 1 {
			return usageError("usage: shiftgate workload delete ID")
		}
		if err := client.DeleteWorkload(ctx, args[0]); err != nil {
			return operationError(err)
		}
		if !settings.jsonOutput {
			fmt.Fprintln(settings.stdout, "Workload deleted.")
		}
		return nil
	case "logs":
		if len(args) == 0 {
			return usageError("usage: shiftgate workload logs ID [--tail LINES]")
		}
		flags := flag.NewFlagSet("workload logs", flag.ContinueOnError)
		flags.SetOutput(settings.stderr)
		tail := flags.Int("tail", 200, "number of trailing lines")
		if err := flags.Parse(args[1:]); err != nil {
			return usageError(err.Error())
		}
		content, err := client.Logs(ctx, args[0], *tail)
		if err != nil {
			return operationError(err)
		}
		fmt.Fprint(settings.stdout, content)
		return nil
	default:
		return usageError("unknown workload subcommand " + subcommand)
	}
}

func workloadCreate(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if len(arguments) == 0 {
		return usageError("usage: shiftgate workload create NAME --path PATH -- COMMAND [ARGS...]")
	}
	name := arguments[0]
	flags := flag.NewFlagSet("workload create", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	root := flags.String("path", "", "explicit workload root")
	working := flags.String("workdir", "", "working directory")
	start := flags.Bool("start", false, "start immediately")
	network := flags.String("network", string(model.NetworkReconnect), "preserve, reconnect, or drain")
	devicePolicy := flags.String("device-policy", string(model.DeviceRejectIncompatible), "reject_incompatible or warn_incompatible")
	cpu := flags.Float64("cpu", 0, "CPU limit in cores")
	memory := flags.String("memory", "", "memory limit, for example 2GiB")
	var environment stringValues
	var gpus gpuValues
	flags.Var(&environment, "env", "environment variable KEY=VALUE; repeatable")
	flags.Var(&gpus, "gpu", "required GPU, fields vendor=,model=,memory=,compute=,runtime=,restore=; repeatable")
	if err := flags.Parse(arguments[1:]); err != nil {
		return usageError(err.Error())
	}
	command := flags.Args()
	if *root == "" || len(command) == 0 {
		return usageError("an explicit --path and command after -- are required")
	}
	switch model.DevicePolicy(*devicePolicy) {
	case model.DeviceRejectIncompatible, model.DeviceWarnIncompatible:
	default:
		return usageError("device policy must be reject_incompatible or warn_incompatible")
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
		NetworkPolicy: model.NetworkPolicy(*network),
		DevicePolicy:  model.DevicePolicy(*devicePolicy),
		Resources:     model.ResourceRequirements{CPUCount: *cpu, MemoryBytes: uint64(memoryBytes), GPUs: gpus},
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

func runCheckpoint(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if len(arguments) == 0 {
		return usageError("checkpoint subcommand is required")
	}
	switch arguments[0] {
	case "list":
		flags := flag.NewFlagSet("checkpoint list", flag.ContinueOnError)
		flags.SetOutput(settings.stderr)
		workload := flags.String("workload", "", "filter by workload id")
		if err := flags.Parse(arguments[1:]); err != nil {
			return usageError(err.Error())
		}
		values, err := client.Checkpoints(ctx, *workload)
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, values)
		}
		writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "ID\tWORKLOAD\tKIND\tCREATED\tPLAIN\tSTORED")
		for _, value := range values {
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", shortID(value.ID), value.WorkloadName, value.Kind, value.CreatedAt.Local().Format(time.DateTime), humanBytes(value.PlainBytes), humanBytes(value.StoredBytes))
		}
		return writer.Flush()
	case "create":
		if len(arguments) < 2 {
			return usageError("usage: shiftgate checkpoint create WORKLOAD [options]")
		}
		flags := flag.NewFlagSet("checkpoint create", flag.ContinueOnError)
		flags.SetOutput(settings.stderr)
		leaveRunning := flags.Bool("leave-running", true, "resume source after checkpoint")
		tcpState := flags.Bool("tcp-state", false, "request CRIU TCP repair state")
		parent := flags.String("parent", "", "parent checkpoint for incremental mode")
		timeout := flags.Int("timeout", 1800, "checkpoint timeout in seconds")
		if err := flags.Parse(arguments[2:]); err != nil {
			return usageError(err.Error())
		}
		kind := model.CheckpointFull
		if *parent != "" {
			kind = model.CheckpointIncremental
		}
		manifest, err := client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
			WorkloadID: arguments[1], Kind: kind, ParentID: *parent,
			LeaveRunning: leaveRunning, TCPState: *tcpState, TimeoutSeconds: *timeout,
		})
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, manifest)
		}
		fmt.Fprintf(settings.stdout, "Checkpoint %s created. %s -> %s, deduplicated %s, duration %s.\n", shortID(manifest.ID), humanBytes(manifest.Metrics.PlainBytes), humanBytes(manifest.Metrics.StoredBytes), humanBytes(manifest.Metrics.DeduplicatedBytes), manifest.Metrics.Duration.Round(time.Millisecond))
		if capture := manifest.Filesystem; capture.Filesystem != "" {
			source := "the frozen workload root"
			if capture.Snapshot != "" {
				source = "a " + capture.Snapshot + " snapshot"
			}
			changes := ""
			if capture.ChangedFiles+capture.AddedFiles+capture.DeletedFiles > 0 {
				changes = fmt.Sprintf("; %d changed, %d added, %d deleted since the previous checkpoint", capture.ChangedFiles, capture.AddedFiles, capture.DeletedFiles)
			}
			fmt.Fprintf(settings.stdout, "Filesystem: %s, captured from %s%s.\n", capture.Filesystem, source, changes)
		}
		if len(manifest.Dependencies) > 0 {
			outside := 0
			for _, dependency := range manifest.Dependencies {
				if !dependency.InsideRoot {
					outside++
				}
			}
			fmt.Fprintf(settings.stdout, "Dependencies: %d of %d resolved files live outside the workload root and do not travel with the checkpoint.\n", outside, len(manifest.Dependencies))
		}
		return nil
	case "mirror":
		if len(arguments) != 2 {
			return usageError("usage: shiftgate checkpoint mirror ID")
		}
		result, err := client.MirrorCheckpoint(ctx, arguments[1])
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, result)
		}
		fmt.Fprintf(settings.stdout, "Checkpoint %s mirrored: %d objects, %s.\n", shortID(result.CheckpointID), result.Objects, humanBytes(result.Bytes))
		return nil
	case "inspect":
		if len(arguments) != 2 {
			return usageError("usage: shiftgate checkpoint inspect ID")
		}
		manifest, err := client.Checkpoint(ctx, arguments[1])
		if err != nil {
			return operationError(err)
		}
		return writeJSON(settings.stdout, manifest)
	default:
		return usageError("unknown checkpoint subcommand " + arguments[0])
	}
}

func runRestore(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if len(arguments) == 0 {
		return usageError("usage: shiftgate restore CHECKPOINT [--timeout SECONDS]")
	}
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	timeout := flags.Int("timeout", 1800, "restore timeout in seconds")
	if err := flags.Parse(arguments[1:]); err != nil {
		return usageError(err.Error())
	}
	record, err := client.Restore(ctx, arguments[0], *timeout)
	if err != nil {
		return operationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, record)
	}
	fmt.Fprintf(settings.stdout, "Checkpoint restored and committed. Process PID: %d\n", record.PID)
	return nil
}

func runMigrate(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if len(arguments) == 0 {
		return usageError("usage: shiftgate migrate WORKLOAD --to https://HOST:PORT [options]")
	}
	workload := arguments[0]
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	destination := flags.String("to", "", "destination peer agent URL")
	machineID := flags.String("machine-id", "", "expected destination machine id")
	serverName := flags.String("server-name", "", "TLS server name")
	mode := flags.String("mode", string(model.MigrationCold), "cold or live")
	timeout := flags.Int("timeout", 7200, "migration timeout in seconds")
	wait := flags.Bool("wait", true, "wait for completion")
	if err := flags.Parse(arguments[1:]); err != nil {
		return usageError(err.Error())
	}
	if *destination == "" {
		return usageError("--to is required")
	}
	migration, err := client.CreateMigration(ctx, agentclient.MigrationCreateRequest{
		WorkloadID:  workload,
		Destination: model.Destination{MachineID: *machineID, AgentURL: *destination, ServerName: *serverName},
		Mode:        model.MigrationMode(*mode), TimeoutSeconds: *timeout,
	})
	if err != nil {
		return operationError(err)
	}
	if !*wait {
		if settings.jsonOutput {
			return writeJSON(settings.stdout, migration)
		}
		fmt.Fprintln(settings.stdout, migration.ID)
		return nil
	}
	return waitForMigration(ctx, client, settings, migration.ID)
}

func runStatus(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if len(arguments) == 1 {
		migration, err := client.Migration(ctx, arguments[0])
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, migration)
		}
		printMigration(settings.stdout, migration)
		return nil
	}
	values, err := client.Migrations(ctx)
	if err != nil {
		return operationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, values)
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tWORKLOAD\tSTAGE\tPROGRESS\tDESTINATION\tUPDATED")
	for _, value := range values {
		progress := 0.0
		if len(value.Events) > 0 {
			progress = value.Events[len(value.Events)-1].Progress * 100
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%.0f%%\t%s\t%s\n", shortID(value.ID), shortID(value.WorkloadID), value.Stage, progress, value.Destination.MachineID, value.UpdatedAt.Local().Format(time.DateTime))
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
		migration, err := client.Migration(ctx, id)
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			if migration.Stage == model.MigrationCompleted || migration.Stage == model.MigrationRolledBack {
				return writeJSON(settings.stdout, migration)
			}
		} else if len(migration.Events) > 0 {
			event := migration.Events[len(migration.Events)-1]
			if event.Sequence != lastSequence {
				lastSequence = event.Sequence
				printer.event(migration.Stage, event)
			}
		}
		switch migration.Stage {
		case model.MigrationCompleted:
			printer.end()
			if !settings.jsonOutput {
				fmt.Fprintf(settings.stdout, "Migration successful. Downtime: %s, transferred: %s, deduplicated: %s.\n", migration.Metrics.Downtime.Round(time.Millisecond), humanBytes(migration.Metrics.TransferredBytes), humanBytes(migration.Metrics.DeduplicatedBytes))
				if migration.Network.Identity.IP != "" {
					sockets := "listeners re-established; connections must reconnect"
					if migration.Network.SocketsCarried {
						sockets = "TCP state carried with the checkpoint"
					}
					fmt.Fprintf(settings.stdout, "Network: virtual address %s, %s.\n", migration.Network.Identity.IP, sockets)
				}
			}
			if migration.FailureCode != "" {
				return exitError{code: 5, err: errors.New(migration.FailureReason)}
			}
			return nil
		case model.MigrationRolledBack, model.MigrationFailed, model.MigrationCancelled:
			printer.end()
			if settings.jsonOutput {
				_ = writeJSON(settings.stdout, migration)
			}
			return exitError{code: 5, err: fmt.Errorf("migration %s: %s", migration.Stage, migration.FailureReason)}
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
		fmt.Fprintf(printer.writer, "[%s] %3.0f%% %s", stage, event.Progress*100, event.Message)
		if event.BytesTotal > 0 {
			fmt.Fprintf(printer.writer, " (%s / %s)", humanBytes(event.BytesDone), humanBytes(event.BytesTotal))
		}
		fmt.Fprintln(printer.writer)
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
	fmt.Fprintf(printer.writer, "\r[%s%s] %3.0f%% %s %s", strings.Repeat("=", filled), strings.Repeat(" ", width-filled), event.Progress*100, stage, event.Message)
	if event.BytesTotal > 0 {
		fmt.Fprintf(printer.writer, " (%s / %s)", humanBytes(event.BytesDone), humanBytes(event.BytesTotal))
	}
}

// end closes an in-place bar so whatever follows starts on a fresh line.
func (printer *progressPrinter) end() {
	if printer.terminal {
		fmt.Fprintln(printer.writer)
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
	fmt.Fprintf(settings.stdout, "%s: %s (PID %s)\n", workload.Spec.Name, workload.Status, pid)
	return nil
}

func printMigration(writer io.Writer, migration model.Migration) {
	progress := 0.0
	message := ""
	if len(migration.Events) > 0 {
		event := migration.Events[len(migration.Events)-1]
		progress = event.Progress * 100
		message = event.Message
	}
	fmt.Fprintf(writer, "%s %s %.0f%% %s\n", migration.ID, migration.Stage, progress, message)
}

func runFork(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if len(arguments) == 0 {
		return usageError("usage: shiftgate fork WORKLOAD [--name NAME] [--root PATH] [--checkpoint ID] [--activate]\n       shiftgate fork list\n       shiftgate fork inspect FORK")
	}
	switch arguments[0] {
	case "list":
		records, err := client.Forks(ctx)
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, records)
		}
		writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "FORK\tSTATE\tSOURCE\tWORKLOAD\tROOT\tGEN\tACTIVE")
		for _, record := range records {
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%d\t%t\n", record.ID, record.State,
				record.SourceWorkloadID, record.ForkWorkloadID, record.RootPath, record.Generation, record.Activated)
		}
		return writer.Flush()
	case "inspect":
		if len(arguments) < 2 {
			return usageError("usage: shiftgate fork inspect FORK")
		}
		record, err := client.Fork(ctx, arguments[1])
		if err != nil {
			return operationError(err)
		}
		return writeJSON(settings.stdout, record)
	}
	flags := flag.NewFlagSet("fork", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	name := flags.String("name", "", "name for the forked workload")
	root := flags.String("root", "", "root directory for the forked workload")
	checkpointID := flags.String("checkpoint", "", "fork from this checkpoint instead of the current state")
	activate := flags.Bool("activate", false, "also restore the forked process tree so both workloads run")
	timeout := flags.Int("timeout", 1800, "fork timeout in seconds")
	if err := flags.Parse(arguments[1:]); err != nil {
		return usageError(err.Error())
	}
	input := agentclient.ForkRequest{Name: *name, RootPath: *root, CheckpointID: *checkpointID, Activate: *activate, TimeoutSeconds: *timeout}
	record, err := client.ForkWorkload(ctx, arguments[0], input)
	if err != nil {
		return operationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, record)
	}
	fmt.Fprintf(settings.stdout, "Forked workload %s from checkpoint %s\n", record.ForkWorkloadID, record.SourceCheckpointID)
	fmt.Fprintf(settings.stdout, "  fork id:     %s\n  root:        %s\n  checkpoint:  %s\n  generation:  %d\n",
		record.ID, record.RootPath, record.ForkCheckpointID, record.Generation)
	if record.Activated {
		fmt.Fprintf(settings.stdout, "  process:     running as PID %d\n", record.PID)
	} else {
		fmt.Fprintln(settings.stdout, "  process:     not started; run shiftgate restore with the fork checkpoint or shiftgate fork --activate")
	}
	return nil
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, `Usage: shiftgate [--agent ENDPOINT] [--control-url URL] [--json] COMMAND

Agent commands (local machine):
  doctor                         Check local migration prerequisites
  machines                       Show this machine
  workload create|list|...        Manage workloads
  checkpoint create|list|mirror|inspect
                                  Manage checkpoints
  restore CHECKPOINT              Restore a checkpoint locally
  fork WORKLOAD|list|inspect      Derive an independent workload from a state
  migrate WORKLOAD --to URL       Move a workload to another machine
  status [MIGRATION]              Show migration status
  update status|check|apply|rollback|block|unblock
                                  Manage this machine's updates
  logs WORKLOAD                   Show workload logs
  completion bash|zsh|fish        Generate shell completion
  version                         Print version

Control-plane commands (shiftgate login first; --control-url or SHIFT_CONTROL_URL):
  login [--sso]                    Log in and store a session (single sign-on with --sso)
  logout                          Revoke the session and clear the stored tokens
  whoami                          Show identity and organizations
  plans                           Show the public plan catalog
  fleet machines|workloads|migrations|checkpoints|audit|retention|sso|api-keys|entitlement|usage
                                  Manage the organization's registry
  marketplace offers|inventory|place|publish|withdraw|reservations|reserve|commit|release|fail
                                  Trade compute on the marketplace

With --control-url set, 'machines' and 'workloads' show the fleet view.`)
}

func completion(writer io.Writer, arguments []string) error {
	if len(arguments) != 1 {
		return usageError("usage: shiftgate completion bash|zsh|fish")
	}
	commands := "doctor machines workloads workload checkpoint restore fork migrate status update logs login logout whoami plans fleet marketplace version"
	switch arguments[0] {
	case "bash":
		fmt.Fprintf(writer, "complete -W %q shiftgate\n", commands)
	case "zsh":
		fmt.Fprintf(writer, "#compdef shiftgate\n_arguments '1:command:(%s)'\n", strings.ReplaceAll(commands, " ", " "))
	case "fish":
		for _, command := range strings.Fields(commands) {
			fmt.Fprintf(writer, "complete -c shiftgate -f -n '__fish_use_subcommand' -a %s\n", command)
		}
	default:
		return usageError("unsupported shell " + arguments[0])
	}
	return nil
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
			return int64(parsed * float64(multiplier.value)), nil
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
