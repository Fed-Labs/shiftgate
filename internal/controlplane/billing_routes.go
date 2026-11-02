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
		seats := int(parseMetadataInt(metadata["seats"], 0))
		return plan, billing.EffectiveStorageBytes(catalogPlan, seats), catalogPlan.MaxMachines
	}
	if plan == "" {
		plan = "paid"
	}
	maxStorageBytes = parseMetadataInt(metadata["max_storage_bytes"], 100*1024*1024*1024)
	maxMachines = int(parseMetadataInt(metadata["max_machines"], 10))
	return plan, maxStorageBytes, maxMachines
}

// hasLiveSubscription reports whether the entitlement carries a paid
// subscription right now. A running trial is not a subscription (StartTrial
// keeps the plan key at "free"): its upgrade checkout is the conversion
// path, not a double-subscription.
func hasLiveSubscription(entitlement database.EntitlementRecord) bool {
	return entitlement.Active() && entitlement.Plan != "free" && entitlement.Plan != ""
}

// requirePlanFeature is the plan-gating hook every Business-tier feature call
// passes through. The billing state decides: the catalog (or a negotiated
// tier's contract), the subscription's liveness, a running Business trial,
// and purchased add-on flags (HA Pack, SourceGuard) — all resolved in
// billing.EntitlementState so pricing, enforcement, and the dashboard cannot
// drift. Refusals carry the feature's published code and the upgrade/trial
// door (BR-12: no refusal prints without a door).
func (server *Server) requirePlanFeature(writer http.ResponseWriter, request *http.Request, organizationID string, feature billing.Feature) bool {
	entitlement, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err)
		return false
	}
	server.releaseExpiredTrial(request.Context(), entitlement)
	if !entitlementStateFor(entitlement).Allows(feature) {
		code, message := billing.FeatureRefusal(feature)
		server.recordRefusalEvent(request.Context(), organizationID, "api", string(feature))
		refusePlanFeature(writer, organizationID, code, message)
		return false
	}
	return true
}

// entitlementStateFor projects the stored entitlement onto the billing
// package's trial-aware gate truth — the same projection the heartbeat's
// live_migration_allowed field uses.
func entitlementStateFor(entitlement database.EntitlementRecord) billing.EntitlementState {
	return billing.EntitlementState{
		PlanKey:            entitlement.Plan,
		SubscriptionActive: entitlement.Active(),
		TrialActive:        entitlement.TrialActive(),
		FailoverAddOn:      entitlement.Failover,
		SourceGuardAddOn:   entitlement.SourceGuard,
	}
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
	// Seats is the purchased seat ceiling for flat-with-included plans
	// (Business: 5 included, +$19 per extra). Zero means "included seats
	// only". For legacy per-seat plans it is the quantity and is required.
	Seats int `json:"seats"`
	// Interval selects the billing interval: "month" (default) or "year"
	// (annual prepay, two months free).
	Interval string `json:"interval"`
	// AddOns lists add-on keys ("ha", "sourceguard") carried as extra
	// checkout line items; the webhook reads the metadata back to set the
	// entitlement capability flags.
	AddOns []string `json:"addons"`
	// Coupon is an optional Stripe promotion code (e.g. the founding-partner
	// code). The code itself lives in operator config / env, never in the
	// repository; the control plane only forwards what the client supplies
	// and what config allows.
	Coupon string `json:"coupon"`
	// SpendCapCents overrides the plan's default overage spend cap for this
	// subscription (0 = plan default). Persisted from session metadata when
	// the webhook applies the subscription.
	SpendCapCents int64 `json:"spend_cap_cents"`
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
	interval := strings.TrimSpace(input.Interval)
	if interval == "" {
		interval = "month"
	}
	if interval != "month" && interval != "year" {
		writeError(writer, http.StatusBadRequest, "CHECKOUT_INTERVAL_INVALID", "interval must be month or year")
		return
	}
	if interval == "year" && plan.PriceCentsAnnual == 0 {
		writeError(writer, http.StatusBadRequest, "CHECKOUT_INTERVAL_INVALID", "this plan has no annual price")
		return
	}
	priceID := server.config.StripePrices[plan.Key]
	if interval == "year" {
		// Annual prepay must never silently fall back to the monthly price:
		// the catalog promises two months free, so a missing annual mapping is
		// refused loudly rather than billed wrongly.
		annual := server.config.StripePrices[plan.Key+"_annual"]
		if annual == "" {
			writeError(writer, http.StatusServiceUnavailable, "PLAN_PRICE_NOT_CONFIGURED", "this plan has no annual Stripe price configured")
			return
		}
		priceID = annual
	}
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
	addOnKeys := make([]string, 0, len(input.AddOns))
	addOnPriceIDs := make([]string, 0, len(input.AddOns))
	for _, key := range input.AddOns {
		addOn, found := billing.FindAddOn(key)
		if !found {
			writeError(writer, http.StatusBadRequest, "ADDON_UNKNOWN", "add-on "+key+" is not in the catalog")
			return
		}
		allowed := false
		for _, base := range addOn.BasePlans {
			if base == plan.Key {
				allowed = true
				break
			}
		}
		if !allowed {
			writeError(writer, http.StatusBadRequest, "ADDON_NOT_AVAILABLE", "add-on "+key+" does not attach to plan "+plan.Key)
			return
		}
		addOnPrice := server.config.StripePrices["addon_"+key]
		if addOnPrice == "" {
			writeError(writer, http.StatusServiceUnavailable, "PLAN_PRICE_NOT_CONFIGURED", "add-on "+key+" has no Stripe price configured")
			return
		}
		addOnKeys = append(addOnKeys, key)
		addOnPriceIDs = append(addOnPriceIDs, addOnPrice)
	}
	coupon := strings.TrimSpace(input.Coupon)
	if coupon != "" && !server.couponAllowed(coupon) {
		writeError(writer, http.StatusBadRequest, "COUPON_INVALID", "coupon code is not accepted")
		return
	}
	// The overage spend cap rides subscription metadata into an INTEGER
	// column: an out-of-range value fails the checkout request loudly, never
	// the extras write behind the customer's back.
	if input.SpendCapCents < 0 || input.SpendCapCents > maxSpendCapCents {
		writeError(writer, http.StatusBadRequest, "CHECKOUT_SPEND_CAP_INVALID", "spend_cap_cents must be between 0 and 1000000000")
		return
	}
	seatQuantity := input.Seats
	if plan.PerSeat && seatQuantity < 1 {
		writeError(writer, http.StatusBadRequest, "CHECKOUT_SEATS_INVALID", "this plan is billed per seat and requires at least one seat")
		return
	}
	if plan.IncludedSeats > 0 {
		// Flat-with-included: the request names the purchased seat ceiling;
		// zero means the included seats only. A ceiling below what the team
		// already has would under-count the subscription the moment the next
		// member joins.
		if seatQuantity < 0 {
			writeError(writer, http.StatusBadRequest, "CHECKOUT_SEATS_INVALID", "seat count cannot be negative")
			return
		}
		if seatQuantity == 0 {
			seatQuantity = plan.IncludedSeats
		}
		if seatQuantity < plan.IncludedSeats {
			writeError(writer, http.StatusBadRequest, "CHECKOUT_SEATS_INVALID", "seat count below the plan's included seats")
			return
		}
	}
	if plan.PerSeat || plan.IncludedSeats > 0 {
		// Seat consistency runs both directions: buying fewer seats than the
		// team already has would under-count the subscription the moment the
		// next member joins.
		memberCount, countErr := server.database.CountMembers(request.Context(), organizationID)
		if countErr != nil {
			server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", countErr)
			return
		}
		if seatQuantity > 0 && seatQuantity < memberCount {
			writeError(writer, http.StatusPaymentRequired, "SEATS_BELOW_MEMBERS", "seat count must cover every current organization member")
			return
		}
	}
	entitlement, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err)
		return
	}
	server.releaseExpiredTrial(request.Context(), entitlement)
	// Never double-bill: a checkout session is subscription-mode and would
	// open a SECOND live subscription alongside the existing one. Plan
	// changes, seat growth, and add-ons run through the billing portal on an
	// existing subscription (pricing FAQ: "plan changes run through the
	// customer portal"); checkout is the path for a first subscription or a
	// resubscription after cancellation. A running trial is not a
	// subscription — its checkout is the conversion path.
	if hasLiveSubscription(entitlement) {
		writeError(writer, http.StatusConflict, "SUBSCRIPTION_ACTIVE", "a subscription is already active; change plans, seats, or add-ons through the billing portal — a second checkout would create a second subscription")
		return
	}
	// API-key principals carry no email: fall back to the organization owner
	// so every checkout session names a real billing contact.
	customerEmail := principal.Email
	if customerEmail == "" {
		if ownerEmail, ownerErr := server.database.OwnerEmail(request.Context(), organizationID); ownerErr == nil {
			customerEmail = ownerEmail
		}
	}
	checkout, err := server.stripe.CreateCheckoutSession(request.Context(), billing.CheckoutSessionRequest{
		OrganizationID: organizationID,
		PlanKey:        plan.Key,
		PriceID:        priceID,
		CustomerID:     entitlement.StripeCustomerID,
		CustomerEmail:  customerEmail,
		SuccessURL:     successURL,
		CancelURL:      cancelURL,
		SeatQuantity:   seatQuantity,
		PerSeatBilling: plan.PerSeat,
		Interval:       interval,
		AddOnPriceIDs:  addOnPriceIDs,
		AddOnKeys:      addOnKeys,
		Coupon:         coupon,
		SpendCapCents:  input.SpendCapCents,
	})
	if err != nil {
		server.logger.Error("stripe checkout failed", "organization_id", organizationID, "plan", plan.Key, "error", err)
		writeError(writer, http.StatusBadGateway, "CHECKOUT_FAILED", "Stripe rejected the checkout session request")
		return
	}
	_ = server.database.RecordAudit(request.Context(), server.auditInput(request, "billing.checkout_started", "plan", plan.Key, map[string]string{"plan": plan.Key, "seats": strconv.Itoa(seatQuantity), "interval": interval}))
	writeJSON(writer, http.StatusOK, map[string]string{"url": checkout.URL, "session_id": checkout.ID, "plan": plan.Key})
}

// couponAllowed reports whether a promotion code may be forwarded to Stripe.
// Only codes the operator allowlisted (config / SHIFT_CHECKOUT_COUPONS) pass;
// the codes themselves never live in the repository.
func (server *Server) couponAllowed(code string) bool {
	for _, allowed := range server.config.CheckoutCoupons {
		if strings.EqualFold(strings.TrimSpace(allowed), code) {
			return true
		}
	}
	return false
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
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err)
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
	organizationID := request.PathValue("organizationID")
	record, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "ENTITLEMENT_NOT_FOUND", "organization entitlement was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err)
		return
	}
	// The entitlement read path is where trial expiry reverts capacity: an
	// expired, unconverted trial must report — and enforce — free-tier limits,
	// not the Business capacity StartTrial raised.
	server.releaseExpiredTrial(request.Context(), record)
	if trialReleaseDue(record) {
		if refreshed, refreshErr := server.database.Entitlement(request.Context(), organizationID); refreshErr == nil {
			record = refreshed
		}
	}
	writeJSON(writer, http.StatusOK, Entitlement{OrganizationID: record.OrganizationID, Plan: record.Plan, Status: record.Status, MaxStorageBytes: record.MaxStorageBytes, UsedStorageBytes: record.UsedStorageBytes, MaxMachines: record.MaxMachines, StripeCustomerID: record.StripeCustomerID, TrialActive: record.TrialActive()})
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
	records, err := server.database.Usage(request.Context(), request.PathValue("organizationID"), from, to, pageFromRequest(request))
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "USAGE_LOOKUP_FAILED", err)
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
	// Non-terminal payment states decide nothing: an abandoned checkout must
	// not rewrite entitlements (mid-trial upgrades would be silently demoted
	// to free-tier values for the rest of the window). The later active /
	// canceled / incomplete_expired event applies the real outcome.
	if subscriptionStatusDeferred(status) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "deferred"})
		return
	}
	processed, err := server.database.ApplyStripeSubscription(request.Context(), event.ID, event.Type, payload, organizationID, plan, status, subscription.Customer, subscription.ID, maxStorage, maxMachines)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "WEBHOOK_PROCESSING_FAILED", err)
		return
	}
	if !processed {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}
	// Persist the purchased seat ceiling alongside the subscription so the
	// member ceiling and the Stripe quantity the seat sync pushes stay equal.
	// The ceiling comes from checkout metadata; the line item is the extra
	// seat line, never the base plan price.
	if organizationID != "" {
		seatPlan, _ := billing.FindPlan(subscription.Metadata["plan"])
		if itemID, seats := subscription.SeatPurchase(seatPlan, knownStripePriceIDs(server.config.StripePrices, subscription.Metadata["plan"])); seats > 0 {
			if err := server.database.SetEntitlementSeats(request.Context(), organizationID, seats, itemID); err != nil {
				server.logger.Warn("could not persist entitlement seats", "organization_id", organizationID, "error", err)
			}
		}
	}
	// Billing-v2 extras (plan 2 BR-05): interval, add-on capability flags,
	// overage spend cap. A live subscription clears any trial marker (Stripe
	// truth wins outright) and records the conversion.
	if organizationID != "" {
		extras := parseSubscriptionExtras(subscription.Metadata)
		live := (database.EntitlementRecord{Status: status}).Active()
		spendCap := extras.OverageCapCents
		if spendCap > maxSpendCapCents {
			spendCap = maxSpendCapCents
		}
		if err := server.database.SetEntitlementExtras(request.Context(), organizationID, extras.Interval, extras.SeatsIncluded, extras.SourceGuard, extras.Failover, spendCap, live); err != nil {
			server.logger.Warn("could not persist entitlement extras", "organization_id", organizationID, "error", err)
		}
		if live {
			if err := server.database.MarkTrialConverted(request.Context(), organizationID, time.Now().UTC()); err != nil {
				server.logger.Warn("could not record trial conversion", "organization_id", organizationID, "error", err)
			}
		}
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "processed"})
}

// maxSpendCapCents bounds the customer-settable overage spend cap: it rides
// into an INTEGER column, so the checkout request and the webhook clamp share
// one limit.
const maxSpendCapCents = 1_000_000_000

// subscriptionStatusDeferred reports whether a subscription status is a
// non-terminal payment state the entitlement write must ignore. "incomplete"
// is "checkout opened, payment not yet settled"; "incomplete_expired" is its
// abandoned end. Neither may rewrite a running trial's entitlements or
// stamp a dead plan label on a free organization.
func subscriptionStatusDeferred(status string) bool {
	return status == database.StatusIncomplete || status == database.StatusIncompleteExpired
}

// knownStripePriceIDs lists the configured base and add-on price ids for one
// plan, so the webhook can recognize the generated extra-seat line by
// exclusion when its unit amount is not unique.
func knownStripePriceIDs(prices map[string]string, planKey string) map[string]bool {
	known := map[string]bool{}
	for _, key := range []string{planKey, planKey + "_annual"} {
		if priceID := prices[key]; priceID != "" {
			known[priceID] = true
		}
	}
	for _, addOn := range billing.AddOnCatalog() {
		if priceID := prices["addon_"+addOn.Key]; priceID != "" {
			known[priceID] = true
		}
	}
	return known
}

// subscriptionExtras is the webhook's billing-v2 metadata decode.
type subscriptionExtras struct {
	Interval        string
	SeatsIncluded   int
	SourceGuard     bool
	Failover        bool
	OverageCapCents int64
}

// parseSubscriptionExtras decodes the session metadata the checkout wrote
// (interval, addons, spend_cap_cents) plus the plan's included-seat count.
func parseSubscriptionExtras(metadata map[string]string) subscriptionExtras {
	extras := subscriptionExtras{Interval: metadata["interval"]}
	if extras.Interval == "" {
		extras.Interval = "month"
	}
	extras.OverageCapCents = parseMetadataInt(metadata["spend_cap_cents"], 0)
	if plan, ok := billing.FindPlan(metadata["plan"]); ok {
		extras.SeatsIncluded = plan.IncludedSeats
	}
	for _, key := range strings.Split(metadata["addons"], ",") {
		switch strings.TrimSpace(key) {
		case "ha":
			extras.Failover = true
		case "sourceguard":
			extras.SourceGuard = true
		}
	}
	return extras
}
