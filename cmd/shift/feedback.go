package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

// feedback.go: the referral loop's CLI surface (plan 1 BR-04/BR-11, master
// BR-19). `shiftgate refer` prints the code + URL + reward terms; the
// post-third-migrate hint lives here too and respects SHIFT_NO_HINTS=1 and
// --json (hints never pollute machine-readable output).

const referralBaseURL = "https://shiftgate.dev/r/"

func runRefer(ctx context.Context, settings options, arguments []string) error {
	organization, remaining, err := extractStringFlag(arguments, "org")
	if err != nil {
		return usageError(err.Error())
	}
	if len(remaining) != 0 {
		return usageError("usage: shiftgate refer [--org ID]")
	}
	session, err := controlSession(settings)
	if err != nil {
		return err
	}
	organizationID, err := resolveOrganization(ctx, session, organization)
	if err != nil {
		return err
	}
	stats, err := session.ReferralStats(ctx, organizationID)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, stats)
	}
	_, _ = fmt.Fprintf(settings.stdout, "Referral code: %s\n", stats.Code)
	_, _ = fmt.Fprintf(settings.stdout, "Share link:    %s%s\n", referralBaseURL, stats.Code)
	_, _ = fmt.Fprintf(settings.stdout, "Invited: %d · activated: %d · rewards granted: %d\n", stats.Invited, stats.Activated, stats.Rewards)
	if stats.Badge != "" {
		_, _ = fmt.Fprintf(settings.stdout, "Badge: %s\n", stats.Badge)
	}
	_, _ = fmt.Fprintln(settings.stdout, "Give a month, get a month: when a team you invite runs its first move, you both get Pro free for a month (up to 12).")
	return nil
}

// maybeReferralHint runs the hint after a successful migrate/failover once the
// organization has three or more successful moves in a rolling 30-day window
// (plan 1 BR-11). Best effort: no session, no hint. It never writes to
// stdout, never runs under --json, and SHIFT_NO_HINTS=1 disables it.
func maybeReferralHint(ctx context.Context, settings options) {
	if settings.jsonOutput || os.Getenv("SHIFT_NO_HINTS") != "" {
		return
	}
	session, err := controlSession(settings)
	if err != nil {
		return
	}
	organizationID, err := resolveOrganization(ctx, session, "")
	if err != nil {
		return
	}
	migrations, err := session.Migrations(ctx, organizationID)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -30)
	successes := 0
	for _, migration := range migrations {
		if migration.Status == "completed" && migration.CreatedAt.After(cutoff) {
			successes++
		}
	}
	if successes < 3 {
		return
	}
	_, _ = fmt.Fprintln(os.Stderr, "Liked the move? shiftgate refer — give a month, get a month.")
	_, _ = fmt.Fprintln(os.Stderr, "(SHIFT_NO_HINTS=1 turns all hints off)")
}
