package controlplane

import (
	"net/http"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
)

// usage_routes.go: the self-serve usage summary (plan 4 BR-05). One object,
// no pagination, reads only existing tables (usage_records, entitlements,
// machines, migration_jobs). Roles: admin/owner — the RoleAdmin middleware
// wraps the route.

// UsageSummaryResponse is the single-object usage summary.
type UsageSummaryResponse struct {
	Period      UsagePeriod    `json:"period"`
	Storage     UsageStorage   `json:"storage"`
	Transfer    UsageTransfer  `json:"transfer"`
	Machines    UsageMachines  `json:"machines"`
	Migrations  map[string]int `json:"migrations_7d"`
	Seats       UsageSeats     `json:"seats"`
	APIRequests UsageAPI       `json:"api"`
}

type UsagePeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type UsageStorage struct {
	UsedBytes int64   `json:"used_bytes"`
	MaxBytes  int64   `json:"max_bytes"`
	Pct       float64 `json:"pct"`
}

type UsageTransfer struct {
	Bytes30d   int64            `json:"bytes_30d"`
	LimitBytes int64            `json:"limit_bytes"`
	ByChannel  map[string]int64 `json:"by_channel"`
	Throttle   string           `json:"throttle"` // "ok" | "warn" | "exceeded"
	ResetsAt   string           `json:"resets_at"`
}

type UsageMachines struct {
	Online int `json:"online"`
	Total  int `json:"total"`
}

type UsageSeats struct {
	Included  int `json:"included"` // -1 when the plan key is not in the catalog (negotiated plans are unlimited until invoiced)
	Purchased int `json:"purchased"`
	Members   int `json:"members"`
}

type UsageAPI struct {
	RequestsToday int64 `json:"requests_today"`
	DailyBudget   int   `json:"daily_budget"` // -1 unlimited
}

// handleUsageSummary assembles the summary. Cached at most 60 s implicitly by
// the query windows (day/month buckets) — no cache layer needed to meet the
// acceptance bar, and staleness is bounded to the bucket granularity.
func (server *Server) handleUsageSummary(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	ctx := request.Context()
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -30)
	to := now.AddDate(0, 0, 1)

	entitlement, err := server.database.Entitlement(ctx, organizationID)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "ENTITLEMENT_NOT_FOUND", "organization entitlement was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err)
		return
	}
	plan, knownPlan := billing.FindPlan(entitlement.Plan)
	if entitlement.TrialActive() && entitlement.Active() {
		if business, ok := billing.FindPlan("business"); ok {
			plan = business
			knownPlan = true
		}
	}
	// A negotiated plan key the catalog does not know reports its budgets as
	// -1 (unlimited/unknown) rather than 0: a zero budget and zero seats would
	// tell a custom-plan customer they can make no API calls and have no
	// seats at all.
	seatsIncluded := plan.IncludedSeats
	dailyBudget := plan.MaxAPIRequestsPerDay
	if !knownPlan {
		seatsIncluded = -1
		dailyBudget = -1
	}
	maxStorage := entitlement.MaxStorageBytes
	if maxStorage < 0 {
		maxStorage = 0
	}
	pct := 0.0
	if maxStorage > 0 {
		pct = float64(entitlement.UsedStorageBytes) / float64(maxStorage) * 100
	}

	budget, err := server.checkTransferBudget(ctx, organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "USAGE_LOOKUP_FAILED", err)
		return
	}
	throttle := "ok"
	if budget.exceeded() {
		throttle = "exceeded"
	} else if budget.warn() {
		throttle = "warn"
	}
	byChannel, err := server.database.TransferBytesByChannel(ctx, organizationID, from, to)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "USAGE_LOOKUP_FAILED", err)
		return
	}
	migrations, err := server.database.MigrationsSummary(ctx, organizationID, now.AddDate(0, 0, -7), to)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "USAGE_LOOKUP_FAILED", err)
		return
	}
	machines, err := server.database.Machines(ctx, organizationID, database.NormalizePage(0, 0))
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "USAGE_LOOKUP_FAILED", err)
		return
	}
	online := 0
	for _, machine := range machines {
		if machine.Status == "online" {
			online++
		}
	}
	members, err := server.database.CountMembers(ctx, organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "USAGE_LOOKUP_FAILED", err)
		return
	}
	requestsToday, err := server.database.APIRequestsToday(ctx, organizationID, now)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "USAGE_LOOKUP_FAILED", err)
		return
	}

	writeJSON(writer, http.StatusOK, UsageSummaryResponse{
		Period: UsagePeriod{From: from.Format("2006-01-02"), To: to.Format("2006-01-02")},
		Storage: UsageStorage{
			UsedBytes: entitlement.UsedStorageBytes,
			MaxBytes:  maxStorage,
			Pct:       pct,
		},
		Transfer: UsageTransfer{
			Bytes30d:   budget.UsedBytes,
			LimitBytes: budget.LimitBytes,
			ByChannel:  byChannel,
			Throttle:   throttle,
			ResetsAt:   budget.NextReset.Format(time.RFC3339),
		},
		Machines:   UsageMachines{Online: online, Total: len(machines)},
		Migrations: migrations,
		Seats: UsageSeats{
			Included:  seatsIncluded,
			Purchased: entitlement.Seats,
			Members:   members,
		},
		APIRequests: UsageAPI{RequestsToday: requestsToday, DailyBudget: dailyBudget},
	})
}
