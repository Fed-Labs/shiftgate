package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/update"
)

// runUpdate drives the machine's update state through the agent: status, an
// immediate check, an install, a rollback, and the block list an operator uses
// to answer a release that misbehaved here.
func runUpdate(ctx context.Context, client *agentclient.Client, settings options, arguments []string) error {
	if len(arguments) == 0 {
		return usageError("usage: shiftgate update status|check|apply|rollback|block|unblock")
	}
	subcommand := arguments[0]
	args := arguments[1:]
	switch subcommand {
	case "status":
		status, err := client.UpdateStatus(ctx)
		if err != nil {
			return operationError(err)
		}
		return printUpdateStatus(settings, status)
	case "check":
		result, err := client.UpdateCheck(ctx)
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, result)
		}
		return printUpdateEvaluations(settings, result.Evaluations)
	case "apply":
		flags := flag.NewFlagSet("update apply", flag.ContinueOnError)
		flags.SetOutput(settings.stderr)
		version := flags.String("version", "", "install this specific release instead of the current selection")
		if err := flags.Parse(args); err != nil {
			return usageError(err.Error())
		}
		installed, err := client.UpdateApply(ctx, *version)
		if err != nil {
			return updateOperationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, installed)
		}
		_, _ = fmt.Fprintf(settings.stdout, "Installed %s (was %s). Restart the agent service to run it.\n", installed.Version, installed.PreviousVersion)
		return nil
	case "rollback":
		flags := flag.NewFlagSet("update rollback", flag.ContinueOnError)
		flags.SetOutput(settings.stderr)
		backupID := flags.String("backup", "", "roll back to this preserved binary instead of the most recent")
		if err := flags.Parse(args); err != nil {
			return usageError(err.Error())
		}
		installed, err := client.UpdateRollback(ctx, *backupID)
		if err != nil {
			return updateOperationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, installed)
		}
		_, _ = fmt.Fprintf(settings.stdout, "Rolled back to %s (was %s). Restart the agent service to run it.\n", installed.Version, installed.PreviousVersion)
		return nil
	case "block":
		if len(args) < 1 {
			return usageError("usage: shiftgate update block VERSION [--reason TEXT]")
		}
		flags := flag.NewFlagSet("update block", flag.ContinueOnError)
		flags.SetOutput(settings.stderr)
		reason := flags.String("reason", "", "why this version is refused on this machine")
		if err := flags.Parse(args[1:]); err != nil {
			return usageError(err.Error())
		}
		status, err := client.UpdateBlock(ctx, args[0], *reason)
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, status)
		}
		_, _ = fmt.Fprintf(settings.stdout, "Version %s is blocked on this machine.\n", args[0])
		return nil
	case "unblock":
		if len(args) != 1 {
			return usageError("usage: shiftgate update unblock VERSION")
		}
		status, err := client.UpdateUnblock(ctx, args[0])
		if err != nil {
			return operationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, status)
		}
		_, _ = fmt.Fprintf(settings.stdout, "Version %s is allowed again on this machine.\n", args[0])
		return nil
	default:
		return usageError("unknown update subcommand " + subcommand)
	}
}

func printUpdateStatus(settings options, status update.Status) error {
	if settings.jsonOutput {
		return writeJSON(settings.stdout, status)
	}
	_, _ = fmt.Fprintf(settings.stdout, "agent %s on channel %s, policy %s\n", status.CurrentVersion, status.Channel, status.Policy)
	if status.PendingVersion != "" {
		_, _ = fmt.Fprintf(settings.stdout, "pending restart: %s\n", status.PendingVersion)
	}
	if status.LastCheckedAt != nil {
		_, _ = fmt.Fprintf(settings.stdout, "last checked:    %s\n", status.LastCheckedAt.Local().Format(time.DateTime))
	}
	if status.LastCheckError != "" {
		_, _ = fmt.Fprintf(settings.stdout, "last error:      %s\n", status.LastCheckError)
	}
	if status.Available != nil {
		_, _ = fmt.Fprintf(settings.stdout, "available:       %s%s\n", status.Available.Version, mandatoryMark(status.Available.Mandatory))
		if status.Available.Reason != "" {
			_, _ = fmt.Fprintf(settings.stdout, "  %s: %s\n", status.Available.Code, status.Available.Reason)
		}
	}
	for _, evaluation := range status.Evaluations {
		if !evaluation.Applicable && evaluation.Version != "" {
			_, _ = fmt.Fprintf(settings.stdout, "refused:         %s (%s)\n", evaluation.Version, evaluation.Code)
		}
	}
	for _, blocked := range status.Blocked {
		_, _ = fmt.Fprintf(settings.stdout, "blocked:         %s (%s)\n", blocked.Version, blocked.Reason)
	}
	for _, backup := range status.Backups {
		_, _ = fmt.Fprintf(settings.stdout, "preserved:       %s from %s\n", backup.ID, backup.Version)
	}
	if length := len(status.History); length > 0 {
		_, _ = fmt.Fprintln(settings.stdout, "history:")
		shown := status.History
		if length > 10 {
			shown = status.History[:10]
		}
		for _, event := range shown {
			_, _ = fmt.Fprintf(settings.stdout, "  %s %-11s %s -> %s %s\n",
				event.At.Local().Format(time.DateTime), event.Kind, event.FromVersion, event.ToVersion, event.Detail)
		}
	}
	return nil
}

func printUpdateEvaluations(settings options, evaluations []update.Evaluation) error {
	if settings.jsonOutput {
		return writeJSON(settings.stdout, evaluations)
	}
	if len(evaluations) == 0 {
		_, _ = fmt.Fprintln(settings.stdout, "The feed publishes no releases.")
		return nil
	}
	for _, evaluation := range evaluations {
		state := "refused"
		if evaluation.Applicable {
			state = "applicable"
		}
		_, _ = fmt.Fprintf(settings.stdout, "%-10s %s%s\n", state, evaluation.Version, mandatoryMark(evaluation.Mandatory))
		if evaluation.Reason != "" {
			_, _ = fmt.Fprintf(settings.stdout, "  %s: %s\n", evaluation.Code, evaluation.Reason)
		}
	}
	return nil
}

func mandatoryMark(mandatory bool) string {
	if mandatory {
		return " (mandatory)"
	}
	return ""
}

// updateOperationError separates a deferred update, which is the machine being
// busy rather than a failure, from a refused one.
func updateOperationError(err error) error {
	var api *agentclient.APIError
	if errors.As(err, &api) && api.Code == "UPDATE_DEFERRED" {
		return exitError{code: 6, err: fmt.Errorf("update deferred: %s", api.Message)}
	}
	return operationError(err)
}
