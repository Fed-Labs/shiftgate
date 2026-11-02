package main

import (
	"context"
	"fmt"
	"strings"

	"shift.dev/shift/internal/controlclient"
)

// trial.go: `shiftgate trial start|status` (plan 2 BR-02). Every paywall
// refusal points at this command (plan 2 BR-12), so its output always shows
// the next step.

var trialSubcommands = []string{"start", "status"}

func runTrial(ctx context.Context, settings options, arguments []string) error {
	if len(arguments) == 0 {
		return usageError("trial subcommand is required; valid: " + strings.Join(trialSubcommands, ", "))
	}
	subcommand, args := arguments[0], arguments[1:]
	organization, remaining, err := extractStringFlag(args, "org")
	if err != nil {
		return usageError(err.Error())
	}
	if len(remaining) != 0 {
		return usageError("usage: shiftgate trial start|status [--org ID]")
	}
	session, err := controlSession(settings)
	if err != nil {
		return err
	}
	organizationID, err := resolveOrganization(ctx, session, organization)
	if err != nil {
		return err
	}
	switch subcommand {
	case "start":
		status, err := session.StartTrial(ctx, organizationID)
		if err != nil {
			return printRefusal(settings, err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, status)
		}
		_, _ = fmt.Fprintf(settings.stdout, "Business trial started: %d days, no card. Expires %s.\n", status.DaysLeft, status.ExpiresAt)
		_, _ = fmt.Fprintln(settings.stdout, "Keep everything by upgrading before it lapses: shiftgate fleet entitlement")
		return nil
	case "status":
		status, err := session.Trial(ctx, organizationID)
		if err != nil {
			return controlOperationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, status)
		}
		if !status.Trial {
			_, _ = fmt.Fprintln(settings.stdout, "No trial yet. Start one: shiftgate trial start (14 days of Business, no card, once per organization)")
			return nil
		}
		_, _ = fmt.Fprintf(settings.stdout, "Trial: %d days left (expires %s)\n", status.DaysLeft, status.ExpiresAt)
		if status.Converted {
			_, _ = fmt.Fprintln(settings.stdout, "Converted to a paid subscription.")
		}
		return nil
	default:
		return unknownSubcommandError("trial", subcommand, trialSubcommands)
	}
}

// printRefusal renders a 402/403 paywall refusal with its next steps (plan 2
// BR-12: no refusal prints without a door). JSON mode keeps the envelope
// machine-readable.
func printRefusal(settings options, err error) error {
	var apiError *controlclient.APIError
	if !asAPIError(err, &apiError) {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, map[string]any{
			"error": map[string]any{
				"code":        apiError.Code,
				"message":     apiError.Message,
				"upgrade_url": apiError.UpgradeURL,
				"trial_url":   apiError.TrialURL,
			},
		})
	}
	_, _ = fmt.Fprintf(settings.stdout, "%s: %s\n", apiError.Code, apiError.Message)
	if apiError.UpgradeURL != "" {
		_, _ = fmt.Fprintf(settings.stdout, "  upgrade: %s\n", apiError.UpgradeURL)
	}
	if apiError.TrialURL != "" {
		_, _ = fmt.Fprintf(settings.stdout, "  trial:   %s\n", apiError.TrialURL)
	}
	return exitError{code: 1, err: fmt.Errorf("%s", apiError.Code)}
}

// asAPIError unwraps err into a *controlclient.APIError when possible.
func asAPIError(err error, target **controlclient.APIError) bool {
	for current := err; current != nil; {
		if apiError, ok := current.(*controlclient.APIError); ok {
			*target = apiError
			return true
		}
		unwrapper, ok := current.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		current = unwrapper.Unwrap()
	}
	return false
}
