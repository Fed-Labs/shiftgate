package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "shift-slurm:", err)
		os.Exit(1)
	}
}

// checkpointTimeoutSeconds is the checkpoint budget the control plane grants
// the agent's CRIU dump (its own cap is ten minutes). Multi-GB Slurm
// workloads routinely need minutes; the 30-second default would abort every
// real checkpoint.
const checkpointTimeoutSeconds = 600

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("shift-slurm", flag.ContinueOnError)
	flags.SetOutput(stderr)
	controlURL := flags.String("control-url", os.Getenv("SHIFT_CONTROL_URL"), "control-plane URL")
	tokenFile := flags.String("token-file", os.Getenv("SHIFT_CONTROL_TOKEN_FILE"), "0600 file containing API token")
	token := flags.String("token", os.Getenv("SHIFT_CONTROL_TOKEN"), "API token; prefer --token-file")
	org := flags.String("organization", os.Getenv("SHIFT_ORGANIZATION_ID"), "organization id")
	caFile := flags.String("ca-file", os.Getenv("SHIFT_CA_FILE"), "optional additional CA PEM")
	jobID := flags.String("job-id", os.Getenv("SLURM_JOB_ID"), "Slurm job id")
	timeout := flags.Duration("timeout", 30*time.Second, "HTTP timeout for quick operations")
	checkpointTimeout := flags.Duration("checkpoint-timeout", 15*time.Minute, "client deadline for the synchronous checkpoint command")
	dryRun := flags.Bool("dry-run", false, "validate and print without changing state")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *controlURL == "" || *org == "" || *jobID == "" {
		return errors.New("--control-url, --organization, and --job-id are required")
	}
	secret, err := resolveToken(*token, *tokenFile)
	if err != nil {
		return err
	}
	client, err := NewClient(*controlURL, secret, *caFile, *timeout)
	if err != nil {
		return err
	}
	positional := flags.Args()
	if len(positional) == 0 {
		return errors.New("operation is required: checkpoint, migrate, poll, cancel")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	switch positional[0] {
	case "checkpoint":
		if len(positional) != 3 {
			return errors.New("usage: checkpoint MACHINE_ID WORKLOAD_ID")
		}
		machineID, workloadID := positional[1], positional[2]
		if *dryRun {
			return writeJSON(stdout, map[string]any{"action": "checkpoint", "machine_id": machineID, "workload_id": workloadID, "timeout_seconds": checkpointTimeoutSeconds, "dry_run": true})
		}
		checkpointContext, checkpointCancel := context.WithTimeout(context.Background(), *checkpointTimeout)
		defer checkpointCancel()
		result, err := client.WithTimeout(*checkpointTimeout).CreateCheckpoint(checkpointContext, *org, machineID, workloadID, checkpointTimeoutSeconds, operationKey(*jobID, "checkpoint:"+workloadID))
		if err != nil {
			return fmt.Errorf("checkpoint of workload %s on %s failed: %w", workloadID, machineID, err)
		}
		return writeJSON(stdout, result)
	case "migrate":
		if len(positional) != 4 && len(positional) != 5 {
			return errors.New("usage: migrate SOURCE_MACHINE_ID DESTINATION_MACHINE_ID WORKLOAD_ID [cold|live]")
		}
		mode := "cold"
		if len(positional) == 5 {
			mode = positional[4]
		}
		if mode != "cold" && mode != "live" {
			return errors.New("mode must be cold or live")
		}
		result, err := client.CreateMigration(ctx, *org, map[string]string{"workload_id": positional[3], "source_machine_id": positional[1], "destination_machine_id": positional[2], "mode": mode}, operationKey(*jobID, "migrate"), *dryRun)
		if err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "poll":
		if len(positional) != 2 {
			return errors.New("usage: poll MIGRATION_ID")
		}
		result, err := client.Migration(ctx, *org, positional[1])
		if err != nil {
			return err
		}
		// The migration record is the status source; an events outage must
		// not fail the whole poll after the lookup succeeded.
		events, err := client.Events(ctx, *org, positional[1])
		output := map[string]any{"migration": result}
		if err != nil {
			fmt.Fprintf(stderr, "shift-slurm: migration events unavailable: %v\n", err)
			output["events_error"] = err.Error()
		} else {
			output["events"] = events
		}
		return writeJSON(stdout, output)
	case "cancel":
		if len(positional) != 2 {
			return errors.New("usage: cancel MIGRATION_ID")
		}
		return client.Cancel(ctx, *org, positional[1], operationKey(*jobID, "cancel"))
	default:
		return fmt.Errorf("unknown operation %q", positional[0])
	}
}

func resolveToken(value, file string) (string, error) {
	if file != "" {
		info, err := os.Stat(file)
		if err != nil {
			return "", err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("token file %s must not be group/world readable", file)
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		value = strings.TrimSpace(string(contents))
	}
	if value == "" {
		return "", errors.New("control-plane token is empty")
	}
	return value, nil
}

func operationKey(jobID, operation string) string {
	digest := sha256.Sum256([]byte(jobID + ":" + operation))
	return "slurm-" + hex.EncodeToString(digest[:16])
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
