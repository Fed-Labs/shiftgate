package controlplane

import (
	"context"
	"net/http"
	"strings"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
)

// abuse.go: velocity limits and signup hygiene (plan 4 BR-13). The policy is
// audit-and-throttle, never auto-ban: flags are reversible audit events and
// caps answer 429 in the existing envelope. terms §2 already forbids
// multi-account abuse; the audit metadata cites it.

const (
	// relayTicketsPerHour caps relay-ticket issuance per organization (BR-13c).
	relayTicketsPerHour = 120
	// freeOrgsPerEmailDomain before org.velocity_flag is raised (BR-13b).
	freeOrgsPerEmailDomain = 2
)

// checkMigrationRate enforces MaxMigrationsPerDay per organization. Returns
// false after writing the 429 refusal.
func (server *Server) checkMigrationRate(writer http.ResponseWriter, request *http.Request, organizationID string) bool {
	entitlement, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		return true
	}
	plan, ok := billing.FindPlan(entitlement.Plan)
	if !ok {
		return true
	}
	limit := plan.MaxMigrationsPerDay
	if limit < 0 {
		return true
	}
	used, err := server.database.MigrationsCreatedToday(request.Context(), organizationID, time.Now().UTC())
	if err != nil {
		return true
	}
	if used >= limit {
		writeJSON(writer, http.StatusTooManyRequests, map[string]any{
			"code":        billing.RateLimitUpgradeCode,
			"message":     "daily migration job limit reached for this plan",
			"used":        used,
			"limit":       limit,
			"upgrade_url": upgradeURL(organizationID, billing.RateLimitUpgradeCode),
		})
		return false
	}
	return true
}

// checkRelayTicketRate enforces the 120/h relay-ticket ceiling (BR-13c).
func (server *Server) checkRelayTicketRate(writer http.ResponseWriter, request *http.Request, organizationID string) bool {
	used, err := server.database.RelayTicketsIssuedThisHour(request.Context(), organizationID, time.Now().UTC())
	if err != nil {
		return true
	}
	if used >= relayTicketsPerHour {
		writeJSON(writer, http.StatusTooManyRequests, map[string]any{
			"code":        billing.RateLimitUpgradeCode,
			"message":     "relay ticket issuance rate limit reached; direct peer-to-peer transfers are unaffected",
			"limit":       relayTicketsPerHour,
			"upgrade_url": upgradeURL(organizationID, billing.RateLimitUpgradeCode),
		})
		return false
	}
	return true
}

// flagDomainVelocity raises the reversible org.velocity_flag audit event when
// one email domain holds more free organizations than the heuristic allows
// (BR-13b). Manual review only — the flag bans nothing. Called at signup.
func (server *Server) flagDomainVelocity(ctx context.Context, organizationID, email string) {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return
	}
	domain := strings.ToLower(email[at+1:])
	count, err := server.database.FreeOrganizationsForEmailDomain(ctx, domain)
	if err != nil || count <= freeOrgsPerEmailDomain {
		return
	}
	_ = server.database.RecordAudit(ctx, database.AuditInput{
		OrganizationID: organizationID,
		Action:         "org.velocity_flag",
		ResourceType:   "organization",
		ResourceID:     organizationID,
		Metadata: map[string]string{
			"email_domain":   domain,
			"free_org_count": itoa(count),
			"note":           "terms §2 forbids multi-account abuse; reversible flag for manual review, not a ban",
		},
	})
	_ = server.database.AddBillingEvent(ctx, organizationID, "org.velocity_flag", map[string]string{"email_domain": domain})
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for v := value; v > 0; v /= 10 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
	}
	return string(digits)
}
