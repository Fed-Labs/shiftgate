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
	form.Set("line_items[0][price]", request.PriceID)
	form.Set("line_items[0][quantity]", strconv.Itoa(quantity))
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
	defer response.Body.Close()
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
