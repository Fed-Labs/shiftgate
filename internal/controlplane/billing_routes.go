package controlplane

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
)

// resolveSubscriptionLimits turns a subscription's metadata into the plan key
// and entitlement limits the control plane stores. Limits are the server's
// decision, not the webhook's: a catalog plan always applies its catalog
// limits, so the numbers the control plane enforces match the plan customers
// see regardless of what the subscription metadata claims. Only a plan outside
// the catalog (a negotiated tier configured by price metadata in Stripe) may
// carry its own limits, with safe defaults. An empty plan keeps the legacy
// "paid" label.
func resolveSubscriptionLimits(metadata map[string]string) (plan string, maxStorageBytes int64, maxMachines int) {
	plan = metadata["plan"]
	if catalogPlan, ok := billing.FindPlan(plan); ok {
		return plan, catalogPlan.MaxStorageBytes, catalogPlan.MaxMachines
	}
	if plan == "" {
		plan = "paid"
	}
	maxStorageBytes = parseMetadataInt(metadata["max_storage_bytes"], 100*1024*1024*1024)
	maxMachines = int(parseMetadataInt(metadata["max_machines"], 10))
	return plan, maxStorageBytes, maxMachines
}

// handlePlans serves the plan catalog. It is public: the pricing page renders
// these values, and because the catalog also defines the limits the control
// plane enforces, the page and the entitlement checks share one source of
// truth. No secret or account data is included.
func (server *Server) handlePlans(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, billing.Catalog())
}

// CheckoutRequest opens a Stripe-hosted checkout for one plan.
type CheckoutRequest struct {
	Plan       string `json:"plan"`
	SuccessURL string `json:"success_url"`
	CancelURL  string `json:"cancel_url"`
	Seats      int    `json:"seats"`
}

// PortalRequest opens a Stripe billing-portal session.
type PortalRequest struct {
	ReturnURL string `json:"return_url"`
}

func safeReturnURL(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", false
	}
	return trimmed, true
}

// handleBillingCheckout starts a Stripe checkout session for a plan. The plan
// must exist in the catalog and have a configured Stripe price; the session
// carries only the organization id and plan key as metadata — every limit is
// resolved server-side when the signed subscription webhook arrives.
func (server *Server) handleBillingCheckout(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	organizationID := request.PathValue("organizationID")
	var input CheckoutRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	plan, ok := billing.FindPlan(input.Plan)
	if !ok {
		writeError(writer, http.StatusBadRequest, "PLAN_UNKNOWN", "plan is not in the catalog")
		return
	}
	if plan.PriceCents <= 0 {
		writeError(writer, http.StatusBadRequest, "PLAN_NOT_PURCHASABLE", "this plan does not use self-service checkout")
		return
	}
	if !server.stripe.Enabled() {
		writeError(writer, http.StatusServiceUnavailable, "BILLING_NOT_CONFIGURED", "Stripe billing is not configured on this control plane")
		return
	}
	priceID := server.config.StripePrices[plan.Key]
	if priceID == "" {
		writeError(writer, http.StatusServiceUnavailable, "PLAN_PRICE_NOT_CONFIGURED", "this plan has no Stripe price configured")
		return
	}
	// The caller names the pages Stripe returns to; the control plane has no
	// dependable public URL of its own to invent them from.
	successURL, ok := safeReturnURL(input.SuccessURL)
	if !ok {
		writeError(writer, http.StatusBadRequest, "CHECKOUT_URL_INVALID", "success_url must be an absolute http(s) URL")
		return
	}
	cancelURL, ok := safeReturnURL(input.CancelURL)
	if !ok {
		writeError(writer, http.StatusBadRequest, "CHECKOUT_URL_INVALID", "cancel_url must be an absolute http(s) URL")
		return
	}
	if plan.PerSeat && input.Seats < 1 {
		writeError(writer, http.StatusBadRequest, "CHECKOUT_SEATS_INVALID", "this plan is billed per seat and requires at least one seat")
		return
	}
	entitlement, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err.Error())
		return
	}
	checkout, err := server.stripe.CreateCheckoutSession(request.Context(), billing.CheckoutSessionRequest{
		OrganizationID: organizationID,
		PlanKey:        plan.Key,
		PriceID:        priceID,
		CustomerID:     entitlement.StripeCustomerID,
		CustomerEmail:  principal.Email,
		SuccessURL:     successURL,
		CancelURL:      cancelURL,
		SeatQuantity:   input.Seats,
	})
	if err != nil {
		server.logger.Error("stripe checkout failed", "organization_id", organizationID, "plan", plan.Key, "error", err)
		writeError(writer, http.StatusBadGateway, "CHECKOUT_FAILED", "Stripe rejected the checkout session request")
		return
	}
	_ = server.database.RecordAudit(request.Context(), server.auditInput(request, "billing.checkout_started", "plan", plan.Key, map[string]string{"plan": plan.Key, "seats": strconv.Itoa(input.Seats)}))
	writeJSON(writer, http.StatusOK, map[string]string{"url": checkout.URL, "session_id": checkout.ID, "plan": plan.Key})
}

// handleBillingPortal opens a Stripe billing-portal session for the
// organization's stored customer. An organization that has never subscribed
// has no customer and gets an explicit error rather than a session.
func (server *Server) handleBillingPortal(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	var input PortalRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	if !server.stripe.Enabled() {
		writeError(writer, http.StatusServiceUnavailable, "BILLING_NOT_CONFIGURED", "Stripe billing is not configured on this control plane")
		return
	}
	returnURL, ok := safeReturnURL(input.ReturnURL)
	if !ok {
		writeError(writer, http.StatusBadRequest, "PORTAL_URL_INVALID", "return_url must be an absolute http(s) URL")
		return
	}
	entitlement, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err.Error())
		return
	}
	if entitlement.StripeCustomerID == "" {
		writeError(writer, http.StatusBadRequest, "PORTAL_NO_CUSTOMER", "this organization has no Stripe customer yet; complete a checkout first")
		return
	}
	portalURL, err := server.stripe.PortalSessionURL(request.Context(), entitlement.StripeCustomerID, returnURL)
	if err != nil {
		server.logger.Error("stripe portal failed", "organization_id", organizationID, "error", err)
		writeError(writer, http.StatusBadGateway, "PORTAL_FAILED", "Stripe rejected the billing-portal session request")
		return
	}
	_ = server.database.RecordAudit(request.Context(), server.auditInput(request, "billing.portal_opened", "organization", organizationID, nil))
	writeJSON(writer, http.StatusOK, map[string]string{"url": portalURL})
}

func (server *Server) handleEntitlement(writer http.ResponseWriter, request *http.Request) {
	record, err := server.database.Entitlement(request.Context(), request.PathValue("organizationID"))
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "ENTITLEMENT_NOT_FOUND", "organization entitlement was not found")
			return
		}
		writeError(writer, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, Entitlement{OrganizationID: record.OrganizationID, Plan: record.Plan, Status: record.Status, MaxStorageBytes: record.MaxStorageBytes, UsedStorageBytes: record.UsedStorageBytes, MaxMachines: record.MaxMachines, StripeCustomerID: record.StripeCustomerID})
}

func (server *Server) handleUsage(writer http.ResponseWriter, request *http.Request) {
	from := time.Now().UTC().AddDate(0, -1, 0)
	to := time.Now().UTC().AddDate(0, 0, 1)
	if value := request.URL.Query().Get("from"); value != "" {
		parsed, err := time.Parse("2006-01-02", value)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "USAGE_DATE_INVALID", "usage from must be an ISO-8601 date")
			return
		}
		from = parsed
	}
	if value := request.URL.Query().Get("to"); value != "" {
		parsed, err := time.Parse("2006-01-02", value)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "USAGE_DATE_INVALID", "usage to must be an ISO-8601 date")
			return
		}
		to = parsed
	}
	if !to.After(from) {
		writeError(writer, http.StatusBadRequest, "USAGE_RANGE_INVALID", "usage end date must be after the start date")
		return
	}
	records, err := server.database.Usage(request.Context(), request.PathValue("organizationID"), from, to)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "USAGE_LOOKUP_FAILED", err.Error())
		return
	}
	result := make([]UsageSummary, 0, len(records))
	for _, record := range records {
		result = append(result, UsageSummary{Kind: record.Kind, Quantity: record.Quantity, PeriodStart: record.PeriodStart.Format("2006-01-02"), PeriodEnd: record.PeriodEnd.Format("2006-01-02")})
	}
	writeJSON(writer, http.StatusOK, result)
}

// handleStripeWebhook applies subscription lifecycle events. The signature is
// verified before anything is parsed, events outside subscription lifecycle
// are acknowledged and ignored, and ApplyStripeSubscription is idempotent on
// the event id so Stripe's at-least-once delivery cannot double-apply.
func (server *Server) handleStripeWebhook(writer http.ResponseWriter, request *http.Request) {
	if strings.TrimSpace(server.config.StripeWebhookKey) == "" {
		writeError(writer, http.StatusNotImplemented, "BILLING_DISABLED", "Stripe billing is not configured")
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, controlBodyLimit))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "WEBHOOK_INVALID", err.Error())
		return
	}
	if err := VerifyStripeSignature(payload, request.Header.Get("Stripe-Signature"), server.config.StripeWebhookKey, time.Now().UTC(), 5*time.Minute); err != nil {
		writeError(writer, http.StatusUnauthorized, "WEBHOOK_SIGNATURE_INVALID", err.Error())
		return
	}
	var event StripeEvent
	if err := json.Unmarshal(payload, &event); err != nil || event.ID == "" || event.Type == "" {
		writeError(writer, http.StatusBadRequest, "WEBHOOK_INVALID", "Stripe event id and type are required")
		return
	}
	if !strings.HasPrefix(event.Type, "customer.subscription.") {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	subscription, err := parseStripeSubscription(event)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "WEBHOOK_INVALID", err.Error())
		return
	}
	organizationID := subscription.Metadata["organization_id"]
	plan, maxStorage, maxMachines := resolveSubscriptionLimits(subscription.Metadata)
	status := subscription.Status
	if event.Type == "customer.subscription.deleted" {
		status = "canceled"
	}
	processed, err := server.database.ApplyStripeSubscription(request.Context(), event.ID, event.Type, payload, organizationID, plan, status, subscription.Customer, subscription.ID, maxStorage, maxMachines)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "WEBHOOK_PROCESSING_FAILED", err.Error())
		return
	}
	if !processed {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "processed"})
}
