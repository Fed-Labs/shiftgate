package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/config"
)

func newBillingTestServer(t *testing.T, configuration config.ControlPlane) *httptest.Server {
	t.Helper()
	server := New(configuration, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer
}

func TestHandlePlansServesCatalog(t *testing.T) {
	httpServer := newBillingTestServer(t, config.DefaultControlPlane())
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, httpServer.URL+"/v1/plans", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("plans status = %d", response.StatusCode)
	}
	var plans []map[string]any
	if err := json.NewDecoder(response.Body).Decode(&plans); err != nil {
		t.Fatal(err)
	}
	if len(plans) != 4 {
		t.Fatalf("plans returned %d entries", len(plans))
	}
	keys := map[string]bool{}
	for _, plan := range plans {
		keys[plan["key"].(string)] = true
		// The catalog the pricing page renders must carry the limits the
		// control plane enforces — the same document serves both.
		if _, ok := plan["max_machines"]; !ok {
			t.Fatalf("plan %v lacks a machine limit", plan["key"])
		}
		if _, ok := plan["max_storage_bytes"]; !ok {
			t.Fatalf("plan %v lacks a storage limit", plan["key"])
		}
	}
	for _, key := range []string{"free", "pro", "business", "enterprise"} {
		if !keys[key] {
			t.Fatalf("plans response is missing %q", key)
		}
	}
}

// checkoutCall invokes the checkout handler directly with an authenticated
// principal, so plan and configuration validation is exercised without a
// database. Validation that precedes the entitlement lookup is the surface
// under test here; the PostgreSQL integration test covers the full route.
func checkoutCall(t *testing.T, configuration config.ControlPlane, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	server := New(configuration, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/organizations/org_1/billing/checkout", strings.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{UserID: "user_1", Email: "admin@example.com", Role: RoleAdmin}))
	request.Header.Set("Content-Type", "application/json")
	request.SetPathValue("organizationID", "org_1")
	recorder := httptest.NewRecorder()
	server.handleBillingCheckout(recorder, request)
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder, decoded
}

func TestCheckoutPlanValidation(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.StripeSecretKey = "sk_test_123"
	cases := []struct {
		body   string
		status int
		code   string
	}{
		{`{"plan":"nonexistent"}`, http.StatusBadRequest, "PLAN_UNKNOWN"},
		{`{"plan":"free"}`, http.StatusBadRequest, "PLAN_NOT_PURCHASABLE"},
		{`{"plan":"enterprise"}`, http.StatusBadRequest, "PLAN_NOT_PURCHASABLE"},
		{`{"plan":"pro"}`, http.StatusServiceUnavailable, "PLAN_PRICE_NOT_CONFIGURED"},
	}
	for _, testCase := range cases {
		recorder, body := checkoutCall(t, configuration, testCase.body)
		if recorder.Code != testCase.status {
			t.Fatalf("body %s: status = %d (%v), want %d", testCase.body, recorder.Code, body, testCase.status)
		}
		if body["code"] != testCase.code {
			t.Fatalf("body %s: code = %v, want %s", testCase.body, body["code"], testCase.code)
		}
	}
}

func TestCheckoutWithoutStripeKey(t *testing.T) {
	// No secret key configured: the route must report billing as unavailable
	// rather than calling Stripe or failing open.
	recorder, body := checkoutCall(t, config.DefaultControlPlane(), `{"plan":"pro"}`)
	if recorder.Code != http.StatusServiceUnavailable || body["code"] != "BILLING_NOT_CONFIGURED" {
		t.Fatalf("status %d body %v", recorder.Code, body)
	}
}

// Every Stripe-touching path fails closed on a deployment without billing
// keys, and the refusal happens before anything is parsed or persisted: this
// webhook runs over a nil store and would panic if it touched the database
// first. The contract's answer is 501 BILLING_DISABLED.
func TestStripeWebhookFailsClosedWithoutConfiguration(t *testing.T) {
	server := New(config.DefaultControlPlane(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/webhooks/stripe", strings.NewReader(`{"id":"evt_1","type":"customer.subscription.created","data":{"object":{}}}`))
	request.Header.Set("Stripe-Signature", "t=1,v1="+strings.Repeat("00", 32))
	recorder := httptest.NewRecorder()
	server.handleStripeWebhook(recorder, request)
	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if recorder.Code != http.StatusNotImplemented || body["code"] != "BILLING_DISABLED" {
		t.Fatalf("webhook without signing key: status %d body %v", recorder.Code, body)
	}
}

// With a signing key configured the signature is verified before anything is
// parsed: an unsigned or forged payload is refused without a peek at its
// contents, never applied and never half-applied.
func TestStripeWebhookRefusesBadSignatureBeforeParsing(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.StripeWebhookKey = strings.Repeat("whsec_", 8)
	server := New(configuration, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/webhooks/stripe", strings.NewReader(`{"id":"evt_1","type":"customer.subscription.created","data":{"object":{}}}`))
	request.Header.Set("Stripe-Signature", "t=1,v1="+strings.Repeat("00", 32))
	recorder := httptest.NewRecorder()
	server.handleStripeWebhook(recorder, request)
	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if recorder.Code != http.StatusUnauthorized || body["code"] != "WEBHOOK_SIGNATURE_INVALID" {
		t.Fatalf("forged webhook: status %d body %v", recorder.Code, body)
	}
}

func TestCheckoutURLValidation(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.StripeSecretKey = "sk_test_123"
	configuration.StripePrices = map[string]string{"pro": "price_pro_monthly"}
	recorder, body := checkoutCall(t, configuration, `{"plan":"pro","success_url":"javascript:alert(1)","cancel_url":"https://app.example.com/cancel"}`)
	if recorder.Code != http.StatusBadRequest || body["code"] != "CHECKOUT_URL_INVALID" {
		t.Fatalf("javascript: url accepted: status %d body %v", recorder.Code, body)
	}
	recorder, body = checkoutCall(t, configuration, `{"plan":"pro","success_url":"https://app.example.com/ok","cancel_url":"/relative"}`)
	if recorder.Code != http.StatusBadRequest || body["code"] != "CHECKOUT_URL_INVALID" {
		t.Fatalf("relative url accepted: status %d body %v", recorder.Code, body)
	}
	recorder, body = checkoutCall(t, configuration, `{"plan":"pro"}`)
	if recorder.Code != http.StatusBadRequest || body["code"] != "CHECKOUT_URL_INVALID" {
		t.Fatalf("missing urls accepted: status %d body %v", recorder.Code, body)
	}
}

func TestCheckoutFlatPlanSeatValidation(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.StripeSecretKey = "sk_test_123"
	configuration.StripePrices = map[string]string{"business": "price_business_monthly"}
	// Business is flat with 5 seats included: zero seats means "included only"
	// and is valid; a negative count is not; below-included is not.
	recorder, body := checkoutCall(t, configuration, `{"plan":"business","seats":-1,"success_url":"https://app.example.com/ok","cancel_url":"https://app.example.com/cancel"}`)
	if recorder.Code != http.StatusBadRequest || body["code"] != "CHECKOUT_SEATS_INVALID" {
		t.Fatalf("negative seats: status %d body %v", recorder.Code, body)
	}
	recorder, body = checkoutCall(t, configuration, `{"plan":"business","seats":3,"success_url":"https://app.example.com/ok","cancel_url":"https://app.example.com/cancel"}`)
	if recorder.Code != http.StatusBadRequest || body["code"] != "CHECKOUT_SEATS_INVALID" {
		t.Fatalf("below-included seats: status %d body %v", recorder.Code, body)
	}
}

func TestCheckoutIntervalValidation(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.StripeSecretKey = "sk_test_123"
	configuration.StripePrices = map[string]string{"pro": "price_pro_monthly", "pro_annual": "price_pro_yearly"}
	recorder, body := checkoutCall(t, configuration, `{"plan":"pro","interval":"week","success_url":"https://app.example.com/ok","cancel_url":"https://app.example.com/cancel"}`)
	if recorder.Code != http.StatusBadRequest || body["code"] != "CHECKOUT_INTERVAL_INVALID" {
		t.Fatalf("bogus interval: status %d body %v", recorder.Code, body)
	}
}

func TestCheckoutCouponAllowlist(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.StripeSecretKey = "sk_test_123"
	configuration.StripePrices = map[string]string{"pro": "price_pro_monthly"}
	recorder, body := checkoutCall(t, configuration, `{"plan":"pro","coupon":"NOT-ALLOWED","success_url":"https://app.example.com/ok","cancel_url":"https://app.example.com/cancel"}`)
	if recorder.Code != http.StatusBadRequest || body["code"] != "COUPON_INVALID" {
		t.Fatalf("unlisted coupon accepted: status %d body %v", recorder.Code, body)
	}
	configuration.CheckoutCoupons = []string{"FOUNDING-TEAM"}
	server := New(configuration, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !server.couponAllowed("FOUNDING-TEAM") {
		t.Fatal("allowlisted coupon refused")
	}
	if !server.couponAllowed("founding-team") {
		t.Fatal("coupon allowlist should match case-insensitively")
	}
	if server.couponAllowed("SOMETHING-ELSE") {
		t.Fatal("unlisted coupon allowed")
	}
	if server.couponAllowed("") {
		t.Fatal("empty coupon allowed without allowlist entry")
	}
}

func TestCheckoutUnauthenticatedRoute(t *testing.T) {
	httpServer := newBillingTestServer(t, config.DefaultControlPlane())
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, httpServer.URL+"/v1/organizations/org_1/billing/checkout", strings.NewReader(`{"plan":"pro"}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated checkout status = %d", response.StatusCode)
	}
}

func TestPortalRequiresConfiguration(t *testing.T) {
	server := New(config.DefaultControlPlane(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/organizations/org_1/billing/portal", strings.NewReader(`{}`))
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{UserID: "user_1", Role: RoleAdmin}))
	request.SetPathValue("organizationID", "org_1")
	recorder := httptest.NewRecorder()
	server.handleBillingPortal(recorder, request)
	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if recorder.Code != http.StatusServiceUnavailable || body["code"] != "BILLING_NOT_CONFIGURED" {
		t.Fatalf("portal without Stripe: status %d body %v", recorder.Code, body)
	}
}

func TestResolveSubscriptionLimitsPrefersCatalog(t *testing.T) {
	// A catalog plan's limits come from the catalog no matter what the
	// webhook metadata claims — the customer's plan defines what they get,
	// not a forged or stale metadata field. Storage follows the unit-economics
	// revision: Pro includes 120 GiB; Business pools 500 GiB + 100 GiB/seat.
	plan, storage, machines := resolveSubscriptionLimits(map[string]string{
		"plan": "pro", "max_storage_bytes": "999999999999", "max_machines": "999",
	})
	if plan != "pro" {
		t.Fatalf("plan = %q", plan)
	}
	if storage != 120*1024*1024*1024 {
		t.Fatalf("storage = %d, want the pro catalog limit (120 GiB)", storage)
	}
	if machines != 10 {
		t.Fatalf("machines = %d, want the pro catalog limit", machines)
	}
	_, storage, machines = resolveSubscriptionLimits(map[string]string{"plan": "business"})
	if storage != 600*1024*1024*1024 || machines != -1 {
		t.Fatalf("business limits = (%d, %d), want 600 GiB (500 + 100×1 seat) and unlimited", storage, machines)
	}
	_, storage, _ = resolveSubscriptionLimits(map[string]string{"plan": "business", "seats": "7"})
	if storage != 1200*1024*1024*1024 {
		t.Fatalf("business 7-seat storage = %d, want 1200 GiB (500 + 7×100)", storage)
	}
}

func TestResolveSubscriptionLimitsNegotiatedPlans(t *testing.T) {
	// A plan outside the catalog — an enterprise tier configured in Stripe —
	// carries its own limits, with defaults when absent.
	plan, storage, machines := resolveSubscriptionLimits(map[string]string{
		"plan": "enterprise-custom", "max_storage_bytes": "1099511627776", "max_machines": "500",
	})
	if plan != "enterprise-custom" {
		t.Fatalf("plan = %q", plan)
	}
	if storage != 1099511627776 || machines != 500 {
		t.Fatalf("custom limits = (%d, %d)", storage, machines)
	}
	plan, storage, machines = resolveSubscriptionLimits(map[string]string{"plan": "enterprise-custom"})
	if plan != "enterprise-custom" {
		t.Fatalf("plan = %q", plan)
	}
	if storage != 100*1024*1024*1024 || machines != 10 {
		t.Fatalf("default limits = (%d, %d)", storage, machines)
	}
}

func TestResolveSubscriptionLimitsLegacyPaidPlan(t *testing.T) {
	plan, _, _ := resolveSubscriptionLimits(map[string]string{})
	if plan != "paid" {
		t.Fatalf("empty plan = %q, want the legacy paid label", plan)
	}
}

func TestSafeReturnURL(t *testing.T) {
	cases := []struct {
		value string
		want  string
		valid bool
	}{
		{"", "", false},
		{"https://app.example.com/billing", "https://app.example.com/billing", true},
		{"http://localhost:3000/billing", "http://localhost:3000/billing", true},
		{"javascript:alert(1)", "", false},
		{"/app/billing", "", false},
		{"https://", "", false},
		{"not a url", "", false},
	}
	for _, testCase := range cases {
		got, ok := safeReturnURL(testCase.value)
		if ok != testCase.valid || got != testCase.want {
			t.Fatalf("safeReturnURL(%q) = (%q, %v), want (%q, %v)", testCase.value, got, ok, testCase.want, testCase.valid)
		}
	}
}

// An oversized or negative spend cap must fail the checkout request — it
// rides into an INTEGER column and a single bad value must never brick the
// extras write (interval, add-on flags, trial conversion) behind the
// customer's back.
func TestBillingCheckoutSpendCapValidation(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.StripeSecretKey = "sk_test_123"
	configuration.StripePrices = map[string]string{"pro": "price_pro_monthly"}
	for _, body := range []string{
		`{"plan":"pro","spend_cap_cents":-1,"success_url":"https://app.example.com/ok","cancel_url":"https://app.example.com/cancel"}`,
		`{"plan":"pro","spend_cap_cents":2000000000,"success_url":"https://app.example.com/ok","cancel_url":"https://app.example.com/cancel"}`,
	} {
		recorder, decoded := checkoutCall(t, configuration, body)
		if recorder.Code != http.StatusBadRequest || decoded["code"] != "CHECKOUT_SPEND_CAP_INVALID" {
			t.Fatalf("body %s: status %d body %v, want CHECKOUT_SPEND_CAP_INVALID", body, recorder.Code, decoded)
		}
	}
}

// Annual checkout must refuse loudly when the annual price is unmapped —
// never silently bill the monthly price while the entitlement row claims
// annual prepay.
func TestBillingCheckoutAnnualRequiresConfiguredAnnualPrice(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.StripeSecretKey = "sk_test_123"
	configuration.StripePrices = map[string]string{"pro": "price_pro_monthly"}
	recorder, decoded := checkoutCall(t, configuration, `{"plan":"pro","interval":"year","success_url":"https://app.example.com/ok","cancel_url":"https://app.example.com/cancel"}`)
	if recorder.Code != http.StatusServiceUnavailable || decoded["code"] != "PLAN_PRICE_NOT_CONFIGURED" {
		t.Fatalf("annual checkout without annual price: status %d body %v, want PLAN_PRICE_NOT_CONFIGURED", recorder.Code, decoded)
	}
}

// The feature walls must refuse with the feature's published code and the
// upgrade/trial door (BR-12), not a bare generic error.
func TestBillingFeatureRefusalCarriesDoor(t *testing.T) {
	recorder := httptest.NewRecorder()
	refusePlanFeature(recorder, "org_1", billing.FailoverPlanRequiredCode, billing.FailoverPlanRequiredMessage)
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", recorder.Code)
	}
	if body["code"] != billing.FailoverPlanRequiredCode {
		t.Fatalf("code = %v, want %s", body["code"], billing.FailoverPlanRequiredCode)
	}
	for _, key := range []string{"upgrade_url", "trial_url"} {
		url, _ := body[key].(string)
		if url == "" {
			t.Fatalf("refusal is missing %s: %v", key, body)
		}
	}
}
