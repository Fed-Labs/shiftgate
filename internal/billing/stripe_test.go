package billing

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeStripe is a stand-in Stripe API: it records form bodies and can be told
// to reject requests.
type fakeStripe struct {
	mu         sync.Mutex
	forms      []url.Values
	paths      []string
	authorized string
	reject     bool
}

func newFakeStripe() *fakeStripe {
	return &fakeStripe{}
}

func (f *fakeStripe) handler(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	f.paths = append(f.paths, request.URL.Path)
	f.authorized = request.Header.Get("Authorization")
	var form url.Values
	if err == nil {
		form, _ = url.ParseQuery(string(body))
	}
	f.forms = append(f.forms, form)
	f.mu.Unlock()
	if f.reject {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"error":{"message":"Invalid API Key provided"}}`))
		return
	}
	switch request.URL.Path {
	case "/v1/checkout/sessions":
		_, _ = writer.Write([]byte(`{"id":"cs_test_1","url":"https://checkout.stripe.com/c/pay/cs_test_1"}`))
	case "/v1/billing_portal/sessions":
		_, _ = writer.Write([]byte(`{"url":"https://billing.stripe.com/session/portal_1"}`))
	default:
		writer.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeStripe) lastForm() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.forms) == 0 {
		return nil
	}
	return f.forms[len(f.forms)-1]
}

func TestStripeClientDisabledWithoutKey(t *testing.T) {
	client := NewStripeClient("")
	if client.Enabled() {
		t.Fatal("empty key reports enabled")
	}
	if _, err := client.CreateCheckoutSession(context.Background(), CheckoutSessionRequest{PriceID: "price_1"}); err == nil {
		t.Fatal("disabled client created a checkout session")
	}
	if _, err := client.PortalSessionURL(context.Background(), "cus_1", "https://app.example.com/billing"); err == nil {
		t.Fatal("disabled client opened a portal session")
	}
	var nilClient *StripeClient
	if nilClient.Enabled() {
		t.Fatal("nil client reports enabled")
	}
}

func TestCreateCheckoutSessionForm(t *testing.T) {
	stripe := newFakeStripe()
	server := httptest.NewServer(http.HandlerFunc(stripe.handler))
	defer server.Close()
	client := NewStripeClient("sk_test_123")
	client.baseURL = server.URL
	session, err := client.CreateCheckoutSession(context.Background(), CheckoutSessionRequest{
		OrganizationID: "org_1",
		PlanKey:        "pro",
		PriceID:        "price_pro",
		CustomerEmail:  "admin@example.com",
		SuccessURL:     "https://app.example.com/billing?status=success",
		CancelURL:      "https://app.example.com/billing?status=cancel",
		SeatQuantity:   0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "cs_test_1" || session.URL == "" {
		t.Fatalf("session = %+v", session)
	}
	if stripe.authorized != "Bearer sk_test_123" {
		t.Fatalf("authorization header = %q", stripe.authorized)
	}
	form := stripe.lastForm()
	if form.Get("mode") != "subscription" {
		t.Fatalf("mode = %q", form.Get("mode"))
	}
	if form.Get("metadata[organization_id]") != "org_1" {
		t.Fatalf("organization metadata = %q", form.Get("metadata[organization_id]"))
	}
	if form.Get("metadata[plan]") != "pro" {
		t.Fatalf("plan metadata = %q", form.Get("metadata[plan]"))
	}
	if form.Get("line_items[0][price]") != "price_pro" {
		t.Fatalf("price = %q", form.Get("line_items[0][price]"))
	}
	if form.Get("line_items[0][quantity]") != "1" {
		t.Fatalf("quantity = %q", form.Get("line_items[0][quantity]"))
	}
	if form.Get("customer_email") != "admin@example.com" {
		t.Fatalf("customer_email = %q", form.Get("customer_email"))
	}
	if form.Get("customer") != "" {
		t.Fatalf("customer = %q, want empty when only an email is provided", form.Get("customer"))
	}
}

func TestCreateCheckoutSessionPrefersStoredCustomer(t *testing.T) {
	stripe := newFakeStripe()
	server := httptest.NewServer(http.HandlerFunc(stripe.handler))
	defer server.Close()
	client := NewStripeClient("sk_test_123")
	client.baseURL = server.URL
	if _, err := client.CreateCheckoutSession(context.Background(), CheckoutSessionRequest{
		OrganizationID: "org_1",
		PlanKey:        "business",
		PriceID:        "price_business",
		CustomerID:     "cus_1",
		CustomerEmail:  "admin@example.com",
		SeatQuantity:   5,
	}); err != nil {
		t.Fatal(err)
	}
	form := stripe.lastForm()
	if form.Get("customer") != "cus_1" {
		t.Fatalf("customer = %q", form.Get("customer"))
	}
	if form.Get("customer_email") != "" {
		t.Fatalf("customer_email = %q, want empty when a customer is known", form.Get("customer_email"))
	}
	if form.Get("line_items[0][quantity]") != "5" {
		t.Fatalf("seat quantity = %q", form.Get("line_items[0][quantity]"))
	}
}

func TestCreateCheckoutSessionValidation(t *testing.T) {
	client := NewStripeClient("sk_test_123")
	cases := []CheckoutSessionRequest{
		{OrganizationID: "org_1", PlanKey: "pro"},
		{PlanKey: "pro", PriceID: "price_1", CustomerEmail: "a@example.com"},
		{OrganizationID: "org_1", PriceID: "price_1", CustomerEmail: "a@example.com"},
		{OrganizationID: "org_1", PlanKey: "pro", PriceID: "price_1"},
	}
	for _, request := range cases {
		if _, err := client.CreateCheckoutSession(context.Background(), request); err == nil {
			t.Fatalf("accepted checkout request %+v", request)
		}
	}
}

func TestPortalSessionURL(t *testing.T) {
	stripe := newFakeStripe()
	server := httptest.NewServer(http.HandlerFunc(stripe.handler))
	defer server.Close()
	client := NewStripeClient("sk_test_123")
	client.baseURL = server.URL
	portalURL, err := client.PortalSessionURL(context.Background(), "cus_1", "https://app.example.com/billing")
	if err != nil {
		t.Fatal(err)
	}
	if portalURL != "https://billing.stripe.com/session/portal_1" {
		t.Fatalf("portal url = %q", portalURL)
	}
	form := stripe.lastForm()
	if form.Get("customer") != "cus_1" || form.Get("return_url") != "https://app.example.com/billing" {
		t.Fatalf("portal form = %v", form)
	}
	if _, err := client.PortalSessionURL(context.Background(), "", "https://app.example.com/billing"); err == nil {
		t.Fatal("portal session created without a customer")
	}
}

func TestStripeErrorsCarryMessage(t *testing.T) {
	stripe := newFakeStripe()
	stripe.reject = true
	server := httptest.NewServer(http.HandlerFunc(stripe.handler))
	defer server.Close()
	client := NewStripeClient("sk_test_123")
	client.baseURL = server.URL
	_, err := client.CreateCheckoutSession(context.Background(), CheckoutSessionRequest{
		OrganizationID: "org_1", PlanKey: "pro", PriceID: "price_1", CustomerEmail: "a@example.com",
	})
	if err == nil {
		t.Fatal("rejected checkout was reported as success")
	}
	if !strings.Contains(err.Error(), "Invalid API Key provided") {
		t.Fatalf("error lost the Stripe message: %v", err)
	}
	if !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("error lost the status: %v", err)
	}
}
