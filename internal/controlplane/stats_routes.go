package controlplane

import (
	"fmt"
	"net/http"
	"time"
)

// stats_routes.go: the public numbers page API (master W10/BR-05) and the
// authenticated weekly scoreboard. Both are privacy-minimal: counts and
// banded money only, never workload contents or per-org PII. The public
// endpoint is unauthenticated by design — transparency is the point — and is
// cached briefly at the edge by the caller, not here. The scoreboard is the
// ops ritual's instrument and is gated to administrators; even then money is
// banded exactly as the public page bands it, because exact revenue is
// company-wide data that no organization member needs to scrape.

// handlePublicStats serves GET /v1/public/stats.
func (server *Server) handlePublicStats(writer http.ResponseWriter, request *http.Request) {
	stats, err := server.database.PublicStats(request.Context())
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "STATS_LOOKUP_FAILED", err)
		return
	}
	writer.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(writer, http.StatusOK, stats)
}

// scoreboardResponse is the weekly scoreboard's wire shape: week-over-week
// counts only. Exact MRR is deliberately absent — money ships as a band, the
// same representation the public numbers page publishes.
type scoreboardResponse struct {
	From                string         `json:"from"`
	To                  string         `json:"to"`
	NewOrgs             int            `json:"new_orgs"`
	NewOrgsPrev         int            `json:"new_orgs_prev"`
	MachinesRegistered  int            `json:"machines_registered"`
	MachinesPrev        int            `json:"machines_registered_prev"`
	CheckpointsCreated  int            `json:"checkpoints_created"`
	CheckpointsPrev     int            `json:"checkpoints_created_prev"`
	MigrationsAttempted int            `json:"migrations_attempted"`
	MigrationsSucceeded int            `json:"migrations_succeeded"`
	MigrationsFailed    int            `json:"migrations_failed"`
	ActiveSubscriptions map[string]int `json:"active_subscriptions_by_plan"`
	MRRBandUSD          string         `json:"mrr_band_usd"`
}

// handleScoreboard serves GET /v1/admin/scoreboard: week-over-week counters
// for the weekly ops ritual. Authorization is platform-admin — an API key with
// the admin scope or the metrics scrape token (requireAuth enforces the same
// predicate for the whole /v1/admin surface; the checks here are defense in
// depth so the counters stay closed however the route is reached). A user
// session alone is never a platform operator credential, whatever
// organization role it holds: no customer account reads platform-wide
// business metrics on its own name. And the response is counts and banded
// money even for operators — exact revenue is company data, not an
// org-admin's to scrape.
func (server *Server) handleScoreboard(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	if principal.APIKeyID != "" {
		if !scopeAllows(principal.Scopes, RoleAdmin, "administration") {
			writeError(writer, http.StatusForbidden, "SCOREBOARD_FORBIDDEN", "an admin-scope API key is required for the scoreboard")
			return
		}
	} else if !server.presentedMetricsToken(request) {
		writeError(writer, http.StatusForbidden, "SCOREBOARD_FORBIDDEN", "platform operator credentials are required for the scoreboard")
		return
	}
	counters, err := server.database.Scoreboard(request.Context(), time.Now().UTC())
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "SCOREBOARD_LOOKUP_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, scoreboardResponse{
		From:                counters.From,
		To:                  counters.To,
		NewOrgs:             counters.NewOrgs,
		NewOrgsPrev:         counters.NewOrgsPrev,
		MachinesRegistered:  counters.MachinesRegistered,
		MachinesPrev:        counters.MachinesPrev,
		CheckpointsCreated:  counters.CheckpointsCreated,
		CheckpointsPrev:     counters.CheckpointsPrev,
		MigrationsAttempted: counters.MigrationsAttempted,
		MigrationsSucceeded: counters.MigrationsSucceeded,
		MigrationsFailed:    counters.MigrationsFailed,
		ActiveSubscriptions: counters.ActiveSubscriptions,
		MRRBandUSD:          revenueBandUSD(counters.MRRCents / 100),
	})
}

// revenueBandUSD rounds a dollar value to the same public band the numbers
// page shows ("$500–1k"), so no surface ever publishes exact revenue.
func revenueBandUSD(value int64) string {
	steps := []int64{0, 100, 500, 1000, 5000, 10000, 50000}
	lower := int64(0)
	for _, step := range steps {
		if value < step {
			break
		}
		lower = step
	}
	upper := int64(0)
	for _, step := range steps {
		if step > lower {
			upper = step
			break
		}
	}
	if upper == 0 {
		return "$" + formatBandUSD(lower) + "+"
	}
	return "$" + formatBandUSD(lower) + "–" + formatBandUSD(upper)
}

func formatBandUSD(value int64) string {
	if value >= 1000 {
		return fmt.Sprintf("%dk", value/1000)
	}
	return fmt.Sprintf("%d", value)
}
