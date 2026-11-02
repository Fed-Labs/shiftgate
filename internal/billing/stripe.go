package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultStripeAPI = "https://api.stripe.com"

// StripeClient talks to Stripe's REST API with the secret key: it creates
// checkout and billing-portal sessions. Subscriptions themselves arrive as
// signed webhooks (controlplane.handleStripeWebhook), so the client never
// writes entitlements directly. A nil client is valid and reports itself
// disabled, letting callers gate on Enabled() instead of nil checks.
type StripeClient struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

// NewStripeClient builds a client for the given secret key. An empty key
// yields a disabled client rather than an error: billing is optional
// configuration, and "not configured" is a state the control plane reports
// honestly, never one it papers over.
func NewStripeClient(apiKey string) *StripeClient {
	return &StripeClient{apiKey: apiKey, baseURL: defaultStripeAPI, client: &http.Client{Timeout: 15 * time.Second}}
}

// Enabled reports whether outbound billing is configured.
func (c *StripeClient) Enabled() bool {
	return c != nil && strings.TrimSpace(c.apiKey) != ""
}

// SetBaseURL redirects the client from api.stripe.com to another endpoint.
// It exists for tests and for deployments that front Stripe with a proxy the
// operator controls; the default never changes.
func (c *StripeClient) SetBaseURL(baseURL string) {
	if c == nil {
		return
	}
	if strings.HasPrefix(baseURL, "https://") || strings.HasPrefix(baseURL, "http://") {
		c.baseURL = strings.TrimSuffix(baseURL, "/")
	}
}

// CheckoutSessionRequest describes the Stripe-hosted checkout to open.
// OrganizationID and PlanKey travel in the session metadata so the resulting
// subscription webhook routes itself to the right organization and plan
// without trusting any client-supplied limits.
type CheckoutSessionRequest struct {
	OrganizationID string
	PlanKey        string
	PriceID        string
	CustomerID     string
	CustomerEmail  string
	SuccessURL     string
	CancelURL      string
	SeatQuantity   int
	// PerSeatBilling marks legacy per-seat plans whose base line quantity is
	// the seat count. Flat-with-included plans bill the base price once and
	// track extra seats on the subscription item instead.
	PerSeatBilling bool
	// Interval is "month" or "year"; it is recorded in session metadata so
	// the webhook and invoice display can report the billing interval.
	Interval string
	// AddOnPriceIDs are extra line items (HA Pack, SourceGuard Assurance);
	// AddOnKeys mirrors them into metadata so the webhook can set the
	// entitlement capability flags.
	AddOnPriceIDs []string
	AddOnKeys     []string
	// Coupon is an optional Stripe promotion code to apply at checkout.
	Coupon string
	// SpendCapCents is the monthly overage spend cap recorded in metadata
	// (0 = plan default).
	SpendCapCents int64
}

// CheckoutSession is the created Stripe session; URL is where the browser
// goes to pay.
type CheckoutSession struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// PortalSessionURL opens a Stripe billing-portal session for an existing
// customer and returns the hosted URL.
func (c *StripeClient) PortalSessionURL(ctx context.Context, customerID, returnURL string) (string, error) {
	if !c.Enabled() {
		return "", errors.New("stripe billing is not configured")
	}
	if customerID == "" {
		return "", errors.New("stripe customer id is required for the billing portal")
	}
	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("return_url", returnURL)
	var session struct {
		URL string `json:"url"`
	}
	if err := c.post(ctx, "/v1/billing_portal/sessions", form, &session); err != nil {
		return "", err
	}
	return session.URL, nil
}

// CreateCheckoutSession opens a subscription-mode checkout session.
func (c *StripeClient) CreateCheckoutSession(ctx context.Context, request CheckoutSessionRequest) (CheckoutSession, error) {
	if !c.Enabled() {
		return CheckoutSession{}, errors.New("stripe billing is not configured")
	}
	if request.PriceID == "" {
		return CheckoutSession{}, errors.New("stripe price id is required for checkout")
	}
	if request.OrganizationID == "" || request.PlanKey == "" {
		return CheckoutSession{}, errors.New("checkout requires the organization and plan in session metadata")
	}
	if request.CustomerID == "" && request.CustomerEmail == "" {
		return CheckoutSession{}, errors.New("checkout requires an existing customer or an email to create one")
	}
	quantity := request.SeatQuantity
	if quantity < 1 {
		quantity = 1
	}
	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("success_url", request.SuccessURL)
	form.Set("cancel_url", request.CancelURL)
	form.Set("metadata[organization_id]", request.OrganizationID)
	form.Set("metadata[plan]", request.PlanKey)
	interval := request.Interval
	if interval == "" {
		interval = "month"
	}
	form.Set("metadata[interval]", interval)
	// The purchased seat ceiling travels in session metadata (copied to the
	// subscription): it is the value the webhook stores in entitlements.seats
	// and the only record that survives quantity syncs. For flat-with-included
	// plans it is included + extra; for legacy per-seat plans it is the seat
	// quantity.
	if request.SeatQuantity > 0 {
		form.Set("metadata[seats]", strconv.Itoa(request.SeatQuantity))
	}
	if len(request.AddOnKeys) > 0 {
		form.Set("metadata[addons]", strings.Join(request.AddOnKeys, ","))
	}
	if request.SpendCapCents > 0 {
		form.Set("metadata[spend_cap_cents]", strconv.FormatInt(request.SpendCapCents, 10))
	}
	// The base line always bills the flat plan price once. Legacy per-seat
	// plans carry the seat quantity on that line; flat-with-included plans
	// bill extra seats as a dedicated per-seat line item below (extras only,
	// at SeatOverageCents), so seat growth can never multiply the base fee and
	// the seat sync later updates that line, not the plan price.
	form.Set("line_items[0][price]", request.PriceID)
	if request.PerSeatBilling {
		form.Set("line_items[0][quantity]", strconv.Itoa(quantity))
	} else {
		form.Set("line_items[0][quantity]", "1")
	}
	for index, addOnPriceID := range request.AddOnPriceIDs {
		form.Set(fmt.Sprintf("line_items[%d][price]", index+1), addOnPriceID)
		form.Set(fmt.Sprintf("line_items[%d][quantity]", index+1), "1")
	}
	if plan, ok := FindPlan(request.PlanKey); ok && plan.IncludedSeats > 0 && !request.PerSeatBilling {
		if extras := request.SeatQuantity - plan.IncludedSeats; extras > 0 {
			if unitAmount := SeatOverageUnitAmount(plan, interval); unitAmount > 0 {
				index := 1 + len(request.AddOnPriceIDs)
				form.Set(fmt.Sprintf("line_items[%d][quantity]", index), strconv.Itoa(extras))
				form.Set(fmt.Sprintf("line_items[%d][price_data][currency]", index), "usd")
				form.Set(fmt.Sprintf("line_items[%d][price_data][unit_amount]", index), strconv.FormatInt(unitAmount, 10))
				form.Set(fmt.Sprintf("line_items[%d][price_data][recurring][interval]", index), interval)
				form.Set(fmt.Sprintf("line_items[%d][price_data][product_data][name]", index), "Extra seats")
			}
		}
	}
	if request.Coupon != "" {
		form.Set("discounts[0][promotion_code]", request.Coupon)
	} else {
		form.Set("allow_promotion_codes", "true")
	}
	if request.CustomerID != "" {
		form.Set("customer", request.CustomerID)
	} else {
		// Without a stored customer Stripe creates one from the email; the
		// subscription webhook then reports it back and the control plane
		// persists it for portal sessions.
		form.Set("customer_email", request.CustomerEmail)
	}
	var session CheckoutSession
	if err := c.post(ctx, "/v1/checkout/sessions", form, &session); err != nil {
		return CheckoutSession{}, err
	}
	if session.ID == "" || session.URL == "" {
		return CheckoutSession{}, errors.New("stripe returned a checkout session without a url")
	}
	return session, nil
}

// UpdateSubscriptionQuantity sets the billed quantity of one subscription
// item — the seat count of a per-seat plan. The control plane calls it when
// organization membership changes so Stripe's invoice quantity and the
// enforced seat ceiling cannot drift apart. Proration is left off: quantity
// changes ride the next invoice rather than generating surprise mid-cycle
// charges.
func (c *StripeClient) UpdateSubscriptionQuantity(ctx context.Context, subscriptionItemID string, quantity int) error {
	if !c.Enabled() {
		return errors.New("stripe billing is not configured")
	}
	if subscriptionItemID == "" {
		return errors.New("stripe subscription item id is required to update quantity")
	}
	if quantity < 1 {
		return errors.New("subscription quantity must be at least one")
	}
	form := url.Values{}
	form.Set("quantity", strconv.Itoa(quantity))
	form.Set("proration_behavior", "none")
	return c.post(ctx, "/v1/subscription_items/"+url.PathEscape(subscriptionItemID), form, nil)
}

// ReportMeteredUsage reports one metered quantity against a subscription item
// (storage GB-months or transfer GB above the included allotment). Stripe
// aggregates usage records per billing period on the metered price; the
// control plane's daily rollup calls this only for active paid subscriptions
// and only for the excess over plan-included amounts. Free organizations
// never emit meter events.
func (c *StripeClient) ReportMeteredUsage(ctx context.Context, subscriptionItemID string, quantity int64, timestamp time.Time) error {
	if !c.Enabled() {
		return errors.New("stripe billing is not configured")
	}
	if subscriptionItemID == "" {
		return errors.New("stripe subscription item id is required to report usage")
	}
	if quantity < 0 {
		return errors.New("metered usage cannot be negative")
	}
	form := url.Values{}
	form.Set("quantity", strconv.FormatInt(quantity, 10))
	form.Set("action", "increment")
	if !timestamp.IsZero() {
		form.Set("timestamp", strconv.FormatInt(timestamp.UTC().Unix(), 10))
	}
	return c.post(ctx, "/v1/subscription_items/"+url.PathEscape(subscriptionItemID)+"/usage_records", form, nil)
}

// StripeSubscription is the subset of subscription state the reconciliation
// job needs: status, plan metadata, the purchased seat ceiling, and the
// subscription item the seat-quantity sync updates.
type StripeSubscription struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	ItemID string `json:"-"`
	Seats  int    `json:"-"`
	Plan   string `json:"-"`
	AddOns string `json:"-"`
}

// GetSubscription fetches one subscription's current state from Stripe. The
// reconciliation job uses it to heal entitlement drift in both directions.
func (c *StripeClient) GetSubscription(ctx context.Context, subscriptionID string) (StripeSubscription, error) {
	if !c.Enabled() {
		return StripeSubscription{}, errors.New("stripe billing is not configured")
	}
	if subscriptionID == "" {
		return StripeSubscription{}, errors.New("stripe subscription id is required")
	}
	var raw struct {
		ID       string            `json:"id"`
		Status   string            `json:"status"`
		Metadata map[string]string `json:"metadata"`
		Items    struct {
			Data []struct {
				ID       string `json:"id"`
				Quantity int    `json:"quantity"`
				Price    struct {
					ID         string `json:"id"`
					UnitAmount int64  `json:"unit_amount"`
				} `json:"price"`
			} `json:"data"`
		} `json:"items"`
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/subscriptions/"+url.PathEscape(subscriptionID), nil)
	if err != nil {
		return StripeSubscription{}, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return StripeSubscription{}, fmt.Errorf("stripe subscription fetch: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return StripeSubscription{}, fmt.Errorf("stripe subscription fetch: read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return StripeSubscription{}, fmt.Errorf("stripe /v1/subscriptions/%s responded %s: %s", subscriptionID, response.Status, stripeErrorMessage(body))
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return StripeSubscription{}, fmt.Errorf("stripe subscription fetch: decode response: %w", err)
	}
	subscription := StripeSubscription{ID: raw.ID, Status: raw.Status, Plan: raw.Metadata["plan"], AddOns: raw.Metadata["addons"]}
	// The seat ceiling of record is the session's seats metadata. The item id
	// is the dedicated extra-seat line (the last line matching the seat
	// overage amount — add-ons may share the unit price), falling back to the
	// first item for legacy subscriptions whose base line carries the seat
	// quantity.
	if parsed, err := strconv.Atoi(raw.Metadata["seats"]); err == nil && parsed > 0 {
		subscription.Seats = parsed
	}
	seatPlan, _ := FindPlan(raw.Metadata["plan"])
	seatUnit := SeatOverageUnitAmount(seatPlan, raw.Metadata["interval"])
	seatItemID := ""
	for _, item := range raw.Items.Data {
		if seatUnit > 0 && item.Price.UnitAmount == seatUnit {
			seatItemID = item.ID
		}
	}
	if seatItemID != "" {
		subscription.ItemID = seatItemID
	} else if len(raw.Items.Data) > 0 {
		subscription.ItemID = raw.Items.Data[0].ID
	}
	if subscription.Seats == 0 && len(raw.Items.Data) > 0 {
		subscription.Seats = raw.Items.Data[0].Quantity
	}
	return subscription, nil
}

// PauseSubscription pauses collection on a subscription (dunning save offer).
// Entitlements drop to free-tier limits while paused; data is preserved.
func (c *StripeClient) PauseSubscription(ctx context.Context, subscriptionID string) error {
	if !c.Enabled() {
		return errors.New("stripe billing is not configured")
	}
	if subscriptionID == "" {
		return errors.New("stripe subscription id is required")
	}
	form := url.Values{}
	form.Set("pause_collection[behavior]", "keep_as_draft")
	return c.post(ctx, "/v1/subscriptions/"+url.PathEscape(subscriptionID), form, nil)
}

// post sends one form-encoded request and decodes the JSON response into out.
// A nil out is allowed when the caller only needs the error.
func (c *StripeClient) post(ctx context.Context, path string, form url.Values, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("stripe %s: %w", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("stripe %s: read response: %w", path, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("stripe %s responded %s: %s", path, response.Status, stripeErrorMessage(body))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("stripe %s: decode response: %w", path, err)
	}
	return nil
}

// stripeErrorMessage extracts a human-readable message from Stripe's error
// envelope without leaking the full body into logs.
func stripeErrorMessage(body []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	if len(body) > 200 {
		body = body[:200]
	}
	return strings.TrimSpace(string(body))
}
