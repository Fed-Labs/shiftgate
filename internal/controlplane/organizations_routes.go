package controlplane

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
)

func (server *Server) handleOrganizations(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	organizations, err := server.database.Organizations(request.Context(), principal.UserID, pageFromRequest(request))
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ORGANIZATIONS_LOOKUP_FAILED", err)
		return
	}
	result := make([]Organization, 0, len(organizations))
	for _, value := range organizations {
		if principal.APIKeyID != "" && value.ID != principal.OrganizationID {
			continue
		}
		result = append(result, organizationResponse(value))
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) handleMemberAdd(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Email string `json:"email"`
		Role  Role   `json:"role"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	email, err := normalizeEmail(input.Email)
	if err != nil || !input.Role.Valid() || input.Role == RoleOwner {
		writeError(writer, http.StatusBadRequest, "MEMBER_INVALID", "a valid non-owner member email and role are required")
		return
	}
	if !server.requirePlanFeature(writer, request, request.PathValue("organizationID"), billing.FeatureTeamMembers) {
		return
	}
	organizationID := request.PathValue("organizationID")
	// Seat ceiling: the purchased quantity bounds membership, and the extra
	// seats are pushed to Stripe below so the invoice quantity cannot drift
	// from what the control plane enforces. Flat-with-included plans
	// (Business: 5 included, +$19 per extra) admit IncludedSeats at minimum
	// and grow with purchased seats; legacy per-seat plans bound by the raw
	// purchase.
	entitlement, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err)
		return
	}
	seatCeiling := 0
	if plan, known := billing.FindPlan(entitlement.Plan); known {
		seatCeiling = billing.SeatCeiling(plan, entitlement.Seats)
	}
	if err := server.database.AddMember(request.Context(), organizationID, email, string(input.Role), seatCeiling, server.auditInput(request, "organization.member_upsert", "organization_member", email, map[string]any{"role": input.Role})); err != nil {
		if errors.Is(err, database.ErrOwnerRoleImmutable) {
			writeError(writer, http.StatusConflict, "OWNER_ROLE_IMMUTABLE", err.Error())
			return
		}
		if errors.Is(err, database.ErrSeatCeilingExceeded) {
			writeError(writer, http.StatusPaymentRequired, "SEATS_EXCEEDED", "seat count must cover every organization member; purchase more seats before adding members")
			return
		}
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "USER_NOT_FOUND", "user account was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "MEMBER_UPDATE_FAILED", err)
		return
	}
	server.syncSeatQuantity(request.Context(), organizationID)
	writeJSON(writer, http.StatusOK, map[string]string{"status": "updated"})
}

// syncSeatQuantity keeps the Stripe subscription item aligned with the seats
// the organization bills. Flat-with-included plans bill extras only
// (quantity = members − included, floored at 1 for Stripe when any extra is
// due); legacy per-seat plans bill the raw count. Membership changes may
// raise the billed quantity (a team that outgrew its purchase is billed for
// the seats it uses), but they can never lower the purchased ceiling:
// entitlements.seats changes only through checkout and the customer portal,
// so a member leaving cannot silently downgrade a subscription. It is
// best-effort bookkeeping: a Stripe outage must not fail the membership
// change itself.
func (server *Server) syncSeatQuantity(ctx context.Context, organizationID string) {
	if !server.stripe.Enabled() {
		return
	}
	entitlement, err := server.database.Entitlement(ctx, organizationID)
	if err != nil || entitlement.StripeItemID == "" {
		return
	}
	members, err := server.database.CountMembers(ctx, organizationID)
	if err != nil || members < 1 {
		return
	}
	quantity := members
	if plan, known := billing.FindPlan(entitlement.Plan); known && plan.IncludedSeats > 0 {
		// Extras-only billing: the flat price covers IncludedSeats; the item
		// carries the overage seats. No extras, no quantity push.
		extras := members - plan.IncludedSeats
		if extras < 1 {
			return
		}
		quantity = extras
	} else if entitlement.Seats > quantity {
		quantity = entitlement.Seats
	}
	if err := server.stripe.UpdateSubscriptionQuantity(ctx, entitlement.StripeItemID, quantity); err != nil {
		server.logger.Warn("stripe seat quantity sync failed", "organization_id", organizationID, "error", err)
	}
}

func (server *Server) handleAuditList(writer http.ResponseWriter, request *http.Request) {
	if !server.requirePlanFeature(writer, request, request.PathValue("organizationID"), billing.FeatureAuditLogging) {
		return
	}
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(request.URL.Query().Get("offset"))
	records, err := server.database.AuditEvents(request.Context(), request.PathValue("organizationID"), limit, offset)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "AUDIT_LOOKUP_FAILED", err)
		return
	}
	result := make([]AuditEvent, 0, len(records))
	for _, record := range records {
		result = append(result, auditResponse(record))
	}
	writeJSON(writer, http.StatusOK, result)
}
