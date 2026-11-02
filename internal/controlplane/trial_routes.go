package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
)

// trial_routes.go: the 14-day no-card Business trial, once per organization,
// ever. The refusal contract: 402 TRIAL_ALREADY_USED with the upgrade CTA
// when a trial row exists in any state, 409 when a paid subscription is
// already active (Stripe truth wins outright — the trial never stacks).

// handleTrialStart begins the trial. Fingerprint = sha256(org id + owner
// email domain + sorted machine ids + creation week) — abuse forensics only,
// never a tracking identifier.
func (server *Server) handleTrialStart(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	ctx := request.Context()
	entitlement, err := server.database.Entitlement(ctx, organizationID)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "ENTITLEMENT_NOT_FOUND", "organization entitlement was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err)
		return
	}
	server.releaseExpiredTrial(ctx, entitlement)
	if hasLiveSubscription(entitlement) {
		writeError(writer, http.StatusConflict, "SUBSCRIPTION_ACTIVE", "a paid subscription is already active; the trial does not stack with it")
		return
	}
	_, found, err := server.database.Trial(ctx, organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "TRIAL_LOOKUP_FAILED", err)
		return
	}
	if found {
		server.recordRefusalEvent(ctx, organizationID, "trial_start", billing.TrialAlreadyUsedCode)
		refusePlanFeature(writer, organizationID, billing.TrialAlreadyUsedCode, billing.TrialAlreadyUsedMessage)
		return
	}
	fingerprint, err := server.trialFingerprint(ctx, organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "TRIAL_FINGERPRINT_FAILED", err)
		return
	}
	record, err := server.database.StartTrial(ctx, organizationID, time.Now().UTC(), fingerprint, "self_serve")
	if err != nil {
		// A concurrent double-start loses the INSERT race with a unique
		// violation; that is the documented refusal (TRIAL_ALREADY_USED), not
		// a server fault.
		if errors.Is(err, database.ErrTrialAlreadyUsed) || database.IsConflict(err) {
			server.recordRefusalEvent(ctx, organizationID, "trial_start", billing.TrialAlreadyUsedCode)
			refusePlanFeature(writer, organizationID, billing.TrialAlreadyUsedCode, billing.TrialAlreadyUsedMessage)
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "TRIAL_START_FAILED", err)
		return
	}
	_ = server.database.AddFunnelEvent(ctx, database.FunnelEvent{OrganizationID: organizationID, Kind: database.FunnelTrialStart, Surface: "api", Feature: "business_trial"})
	_ = server.database.RecordAudit(ctx, server.auditInput(request, "billing.trial_started", "organization", organizationID, map[string]any{"expires_at": record.ExpiresAt.Format(time.RFC3339)}))
	writeJSON(writer, http.StatusOK, map[string]any{
		"organization_id": organizationID,
		"plan":            "business",
		"trial":           true,
		"started_at":      record.StartedAt.Format(time.RFC3339),
		"expires_at":      record.ExpiresAt.Format(time.RFC3339),
	})
}

// handleTrialStatus reports trial state and days remaining.
func (server *Server) handleTrialStatus(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	entitlement, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err)
		return
	}
	server.releaseExpiredTrial(request.Context(), entitlement)
	record, found, err := server.database.Trial(request.Context(), organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "TRIAL_LOOKUP_FAILED", err)
		return
	}
	if !found {
		// Eligibility is a real claim: an organization that already pays is
		// not eligible (the start endpoint answers 409 for it).
		writeJSON(writer, http.StatusOK, map[string]any{"organization_id": organizationID, "trial": false, "eligible": !hasLiveSubscription(entitlement)})
		return
	}
	daysLeft := int(time.Until(record.ExpiresAt).Hours() / 24)
	if daysLeft < 0 {
		daysLeft = 0
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"organization_id": organizationID,
		"trial":           true,
		"eligible":        false,
		"started_at":      record.StartedAt.Format(time.RFC3339),
		"expires_at":      record.ExpiresAt.Format(time.RFC3339),
		"days_left":       daysLeft,
		"converted":       record.ConvertedAt != nil,
	})
}

// trialReleaseDue reports whether trial expiry must restore free-tier
// capacity for this entitlement: the trial marker lingers in the past and no
// Stripe subscription replaced it (a converted trial keeps its subscription's
// limits).
func trialReleaseDue(entitlement database.EntitlementRecord) bool {
	return entitlement.TrialExpiresAt != nil && !entitlement.TrialActive() && entitlement.StripeSubscriptionID == ""
}

// releaseExpiredTrial settles trial expiry on an entitlement read path:
// ReleaseTrialLimits restores the free tier's capacity (2 machines, 10 GiB)
// and clears the trial marker. It is safe to call for any record — a
// converted trial, a live subscription, or no trial at all is a no-op.
func (server *Server) releaseExpiredTrial(ctx context.Context, entitlement database.EntitlementRecord) {
	if !trialReleaseDue(entitlement) {
		return
	}
	if err := server.database.ReleaseTrialLimits(ctx, entitlement.OrganizationID, time.Now().UTC()); err != nil {
		server.logger.Warn("could not release expired trial limits", "organization_id", entitlement.OrganizationID, "error", err)
	}
}

// trialFingerprint builds the abuse-forensics fingerprint: sha256(org id +
// owner email domain + sorted machine ids + creation week). It deliberately
// hashes values the operator already holds (never a device id or IP).
func (server *Server) trialFingerprint(ctx context.Context, organizationID string) (string, error) {
	ownerEmail, _ := server.database.OwnerEmail(ctx, organizationID)
	domain := ""
	if at := strings.LastIndex(ownerEmail, "@"); at >= 0 {
		domain = strings.ToLower(ownerEmail[at+1:])
	}
	machineIDs := []string{}
	if machines, err := server.database.Machines(ctx, organizationID, database.NormalizePage(0, 0)); err == nil {
		for _, machine := range machines {
			machineIDs = append(machineIDs, machine.MachineID)
		}
	}
	sort.Strings(machineIDs)
	week := time.Now().UTC().Format("2006-W02")
	return hashHex(organizationID, domain, strings.Join(machineIDs, ","), week), nil
}

// hashHex is the shared hex encoder for fingerprint material.
func hashHex(parts ...string) string {
	joined := strings.Join(parts, "|")
	sum := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(sum[:])
}
