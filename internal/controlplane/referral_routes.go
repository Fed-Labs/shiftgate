package controlplane

import (
	"errors"
	"net/http"

	"shift.dev/shift/internal/database"
)

// referral_routes.go: the referral loop's control-plane half (plan 1 BR-04).
// GET reads (and lazily issues) the organization's code; POST claim stamps a
// new organization with a code at signup. Self-referral is refused (same org
// or same email domain) and the reward fires once per activation.

// handleReferralStats returns the organization's code and scoreboard. The
// code is created on first read so `shiftgate refer` works with no separate
// enrollment step.
func (server *Server) handleReferralStats(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	stats, err := server.database.ReferralStatsFor(request.Context(), organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "REFERRAL_LOOKUP_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, stats)
}

// ReferralClaimRequest stamps a referral code on an organization at signup.
type ReferralClaimRequest struct {
	Code string `json:"code"`
}

// handleReferralClaim records a signup referral. The claim is best-effort at
// signup: an unknown code is a 404 (the signup flow decides whether that
// fails the registration; the API stays honest), self-referral — same
// organization or same owner email domain — is a 422, and an organization
// that already claimed some code cannot claim a second one (409): one
// referred organization earns one reward, ever.
func (server *Server) handleReferralClaim(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	var input ReferralClaimRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	if input.Code == "" {
		writeError(writer, http.StatusBadRequest, "REFERRAL_CODE_REQUIRED", "referral code is required")
		return
	}
	err := server.database.ClaimReferral(request.Context(), input.Code, organizationID)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "REFERRAL_CODE_UNKNOWN", "referral code was not found")
			return
		}
		if errors.Is(err, database.ErrReferralSelfClaim) {
			writeError(writer, http.StatusUnprocessableEntity, "REFERRAL_SELF_CLAIM", "a referral code cannot be claimed by its own organization")
			return
		}
		if errors.Is(err, database.ErrReferralAlreadyClaimed) {
			writeError(writer, http.StatusConflict, "REFERRAL_ALREADY_CLAIMED", "this organization has already claimed a referral code")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "REFERRAL_CLAIM_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "claimed", "code": input.Code})
}
