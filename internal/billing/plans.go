// Package billing holds SHIFTGATE's plan catalog and the outbound Stripe
// client. Plans are defined here — never in a frontend component — and every
// entitlement decision reads these values, so the plan a customer sees on the
// pricing page and the limits the control plane enforces cannot drift apart.
package billing

// Plan is one subscription tier. PriceCents and limits use -1 for "custom /
// unlimited": enterprise pricing is negotiated per customer, and unlimited
// machine counts are a real tier feature, not an unbounded default.
type Plan struct {
	Key             string   `json:"key"`
	Name            string   `json:"name"`
	PriceCents      int64    `json:"price_cents"`
	PerSeat         bool     `json:"per_seat,omitempty"`
	Description     string   `json:"description"`
	Features        []string `json:"features"`
	MaxMachines     int      `json:"max_machines"`
	MaxStorageBytes int64    `json:"max_storage_bytes"`
}

// Unlimited and Custom mark negotiated or unmetered values.
const (
	Unlimited = -1
	Custom    = -1
)

// Catalog returns every plan in display order. The free tier's limits match
// the entitlements table defaults so an organization that has never touched
// Stripe lands on the same numbers as one that webhooks back a free plan.
func Catalog() []Plan {
	return []Plan{
		{
			Key:             "free",
			Name:            "Free",
			PriceCents:      0,
			Description:     "For trying SHIFTGATE.",
			Features:        []string{"2 machines", "10 GB checkpoint storage", "Cold migration mode", "Community support"},
			MaxMachines:     2,
			MaxStorageBytes: 10 * 1024 * 1024 * 1024,
		},
		{
			Key:             "pro",
			Name:            "Pro",
			PriceCents:      2900,
			Description:     "Personal machines, cloud checkpoints, history.",
			Features:        []string{"10 machines", "500 GB checkpoint storage", "Live migration", "Checkpoint history & lineage", "Priority support"},
			MaxMachines:     10,
			MaxStorageBytes: 500 * 1024 * 1024 * 1024,
		},
		{
			Key:             "business",
			Name:            "Business",
			PriceCents:      9900,
			PerSeat:         true,
			Description:     "Teams, permissions, audit logs.",
			Features:        []string{"Unlimited machines", "2 TB checkpoint storage", "Team members & roles", "Audit logging", "SSO (SAML)", "Priority support"},
			MaxMachines:     Unlimited,
			MaxStorageBytes: 2 * 1024 * 1024 * 1024 * 1024,
		},
		{
			Key:             "enterprise",
			Name:            "Enterprise",
			PriceCents:      Custom,
			Description:     "Custom infrastructure, security controls, support.",
			Features:        []string{"Unlimited everything", "Self-hosted option", "Custom data residency", "Dedicated support engineer", "SLA", "On-premise deployment"},
			MaxMachines:     Unlimited,
			MaxStorageBytes: Unlimited,
		},
	}
}

// FindPlan resolves a plan key — a Stripe subscription's plan metadata or a
// checkout request — to its catalog definition.
func FindPlan(key string) (Plan, bool) {
	for _, plan := range Catalog() {
		if plan.Key == key {
			return plan, true
		}
	}
	return Plan{}, false
}

// PlanKeys lists every catalog plan key.
func PlanKeys() []string {
	catalog := Catalog()
	keys := make([]string, 0, len(catalog))
	for _, plan := range catalog {
		keys = append(keys, plan.Key)
	}
	return keys
}

// KnownPlan reports whether key names a catalog plan.
func KnownPlan(key string) bool {
	_, ok := FindPlan(key)
	return ok
}
