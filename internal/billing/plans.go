// Package billing holds SHIFT's plan catalog and the outbound Stripe
// client. Plans are defined here — never in a frontend component — and every
// entitlement decision reads these values, so the plan a customer sees on the
// pricing page and the limits the control plane enforces cannot drift apart.
package billing

import "strings"

// Feature is one gated capability. The pricing page renders the human feature
// strings; enforcement reads these structured values, so a capability can be
// promised and gated from the same definition.
type Feature string

const (
	FeatureLiveMigration    Feature = "live_migration"
	FeatureTeamMembers      Feature = "team_members"
	FeatureAuditLogging     Feature = "audit_logging"
	FeatureSSO              Feature = "sso"
	FeatureSCIM             Feature = "scim"
	FeaturePeriodicPolicies Feature = "periodic_policies"
	FeatureFailover         Feature = "failover"
	FeatureSourceGuard      Feature = "sourceguard"
	FeatureStorageReclaim   Feature = "storage_reclaim"
)

// Plan is one subscription tier. PriceCents and limits use -1 for "custom /
// unlimited": enterprise pricing is negotiated per customer, and unlimited
// machine counts are a real tier feature, not an unbounded default.
type Plan struct {
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	PriceCents  int64    `json:"price_cents"`
	Description string   `json:"description"`
	Features    []string `json:"features"`
	MaxMachines int      `json:"max_machines"`
	// MaxStorageBytes is the base checkpoint-storage quota. Business pools
	// extra capacity per seat (SeatDerivedStorage); EffectiveStorageBytes
	// computes what an organization actually gets.
	MaxStorageBytes int64 `json:"max_storage_bytes"`
	// PriceCentsAnnual is the prepay price for a full year (two months free
	// versus twelve monthly charges). Zero means "no annual offer"; -1 means
	// custom.
	PriceCentsAnnual int64 `json:"price_cents_annual"`
	// PerSeat is the legacy per-seat billing shape. Business is flat with
	// included seats plus per-seat overage (see IncludedSeats); a plan outside
	// the catalog may still be genuinely per-seat.
	PerSeat bool `json:"per_seat,omitempty"`
	// IncludedSeats is how many members the flat price covers before
	// SeatOverageCents applies per extra seat per month.
	IncludedSeats    int   `json:"included_seats,omitempty"`
	SeatOverageCents int64 `json:"seat_overage_cents,omitempty"`
	// SeatDerivedStorage marks the Business pooled formula:
	// MaxStorageBytes + StoragePerSeatBytes × seats, hard-capped at
	// StorageCapBytes.
	SeatDerivedStorage  bool  `json:"seat_derived_storage,omitempty"`
	StoragePerSeatBytes int64 `json:"storage_per_seat_bytes,omitempty"`
	StorageCapBytes     int64 `json:"storage_cap_bytes,omitempty"`
	// LiveMigration is the structured twin of the "Live migration" feature
	// string: the control plane reads it to gate live-mode migrations, so the
	// tiering the pricing page shows and the tiering the server enforces are
	// the same value.
	LiveMigration bool `json:"live_migration"`
	// TeamMembers, AuditLogging, and SSOAndSCIM are the same idea for the
	// Business-tier differentiators: enforcement consults these flags rather
	// than parsing the display strings.
	TeamMembers  bool `json:"team_members,omitempty"`
	AuditLogging bool `json:"audit_logging,omitempty"`
	SSOAndSCIM   bool `json:"sso_and_scim,omitempty"`
	// PeriodicPolicies, Failover, SourceGuard, and StorageReclaim gate the
	// v2 fleet features. Failover is included on Business and sold as the HA
	// Pack add-on for Pro; SourceGuard is included on Business and sold as an
	// add-on for Pro.
	PeriodicPolicies         bool `json:"periodic_policies,omitempty"`
	Failover                 bool `json:"failover,omitempty"`
	SourceGuard              bool `json:"sourceguard,omitempty"`
	SourceGuardCreditsMonths int  `json:"sourceguard_credits_months,omitempty"`
	StorageReclaim           bool `json:"storage_reclaim,omitempty"`
	// RelayAllowanceBytes is the included monthly platform-mediated transfer
	// allotment. Above it, paid tiers accrue metered transfer; the hard
	// monthly ceiling for every tier is FairUseTransferBytes.
	RelayAllowanceBytes int64 `json:"relay_allowance_bytes,omitempty"`
	// OverageStorageCentsPerGB is the metered storage add-on price per
	// GB-month; OverageTransferCentsPerGB is the metered transfer price per
	// GB. Both are 0 in the shipped catalog: metered overage billing is not
	// implemented, so the catalog does not advertise it — storage hard-stops
	// at the included quota. Non-zero values are reserved for the metering
	// loop and must not be published before it enforces them.
	OverageStorageCentsPerGB  int64 `json:"overage_storage_cents_per_gb,omitempty"`
	OverageTransferCentsPerGB int64 `json:"overage_transfer_cents_per_gb,omitempty"`
	// OverageCapCentsDefault is the default monthly overage spend cap the
	// customer may raise; overage is never billed above a set cap. Zero in
	// the shipped catalog while metered overage is unimplemented.
	OverageCapCentsDefault int64 `json:"overage_cap_cents_default,omitempty"`
	// MaxAPIRequestsPerDay is the daily API budget; MaxAPIRequestsPerHour is
	// the burst ceiling. Machine-to-machine presence heartbeats are exempt.
	MaxAPIRequestsPerDay  int `json:"max_api_requests_per_day,omitempty"`
	MaxAPIRequestsPerHour int `json:"max_api_requests_per_hour,omitempty"`
	// MaxMigrationsPerDay caps migration-job creation per organization.
	MaxMigrationsPerDay int `json:"max_migrations_per_day,omitempty"`
	// HistoryRetentionDays is how long checkpoint history and lineage stay
	// queryable.
	HistoryRetentionDays int `json:"history_retention_days,omitempty"`
	// TrialEligible marks plans a never-subscribed organization can try with
	// the 14-day no-card Business trial.
	TrialEligible bool `json:"trial_eligible,omitempty"`
}

// Allows reports whether the plan includes a gated feature. Plan-level
// answers say nothing about subscription state — a cancelled plan grants
// nothing; callers must combine this with the entitlement's Active() check
// (see internal/database EntitlementRecord).
func (p Plan) Allows(feature Feature) bool {
	switch feature {
	case FeatureLiveMigration:
		return p.LiveMigration
	case FeatureTeamMembers:
		return p.TeamMembers
	case FeatureAuditLogging:
		return p.AuditLogging
	case FeatureSSO, FeatureSCIM:
		return p.SSOAndSCIM
	case FeaturePeriodicPolicies:
		return p.PeriodicPolicies
	case FeatureFailover:
		return p.Failover
	case FeatureSourceGuard:
		return p.SourceGuard
	case FeatureStorageReclaim:
		return p.StorageReclaim
	default:
		return false
	}
}

// PlanFeatureAllowed resolves a plan key to its catalog definition and
// answers for one feature. A non-empty plan key outside the catalog is a
// negotiated tier (or the legacy "paid" label): its contract governs, so it
// is treated as including every catalog feature rather than failing closed
// on a customer whose enterprise agreement predates the catalog. An empty
// plan key fails closed: an entitlement row with no plan grants nothing.
func PlanFeatureAllowed(planKey string, feature Feature) bool {
	if strings.TrimSpace(planKey) == "" {
		return false
	}
	if plan, ok := FindPlan(planKey); ok {
		return plan.Allows(feature)
	}
	return true
}

// EntitlementState is the billing state one gate decision reads: the plan
// key, whether the subscription is entitled right now, whether the no-card
// Business trial is running, and the purchased add-on capability flags. It
// is the single trial-aware truth behind every admission point — the HTTP
// feature walls, the live-migration gates, and the heartbeat's
// live_migration_allowed field — so a running trial carries the Business
// feature set it was sold with, add-on buyers get the add-on they paid for,
// and nothing is granted to a lapsed subscription.
type EntitlementState struct {
	PlanKey            string
	SubscriptionActive bool
	TrialActive        bool
	FailoverAddOn      bool
	SourceGuardAddOn   bool
}

// Allows reports whether the gated feature may be used right now. A running
// trial on an otherwise active entitlement counts as a Business subscription
// for the window (the trial is sold as "14 days of Business"); add-on flags
// (HA Pack failover, SourceGuard Assurance) OR in while the subscription is
// active; everything else defers to the catalog.
func (state EntitlementState) Allows(feature Feature) bool {
	if !state.SubscriptionActive {
		return false
	}
	if state.TrialActive {
		return PlanFeatureAllowed("business", feature)
	}
	switch feature {
	case FeatureFailover:
		if state.FailoverAddOn {
			return true
		}
	case FeatureSourceGuard:
		if state.SourceGuardAddOn {
			return true
		}
	}
	return PlanFeatureAllowed(state.PlanKey, feature)
}

// LiveMigrationAllowed is the entitlement truth the heartbeat transports and
// the agent gate consumes: live mode is a paid entitlement, granted to a
// catalog plan that includes it, a negotiated tier, or a running Business
// trial — never to the cold-only free tier or a lapsed subscription.
func (state EntitlementState) LiveMigrationAllowed() bool {
	return state.Allows(FeatureLiveMigration)
}

// Unlimited and Custom mark negotiated or unmetered values.
const (
	Unlimited = -1
	Custom    = -1
)

// Byte quantities the catalog uses. GiB, not GB: the quotas are enforced in
// bytes and the pricing strings round for display only.
const (
	gib = int64(1024 * 1024 * 1024)
	tib = 1024 * gib

	FreeStorageBytes    = 10 * gib
	ProStorageBytes     = 120 * gib
	BusinessStorageBase = 500 * gib
	BusinessStorageSeat = 100 * gib
	BusinessStorageCap  = 2 * tib
	FreeRelayBytes      = 20 * gib
)

// Catalog returns every plan in display order. The free tier's limits match
// the entitlements table defaults so an organization that has never touched
// Stripe lands on the same numbers as one that webhooks back a free plan.
// Storage quotas follow the unit-economics revision: Pro includes 120 GiB and
// Business pools 500 GiB plus 100 GiB per seat (2 TiB at 15 seats) — the
// earlier 500 GB / 2 TB ceilings lost money at sustained use.
func Catalog() []Plan {
	return []Plan{
		{
			Key:                   "free",
			Name:                  "Free",
			PriceCents:            0,
			Description:           "For trying SHIFT.",
			Features:              []string{"2 machines", "10 GiB checkpoint storage", "Cold migration mode", "20 GiB/mo transfer (hard stop)", "7-day checkpoint history", "Community support (best-effort, no response-time target)"},
			MaxMachines:           2,
			MaxStorageBytes:       FreeStorageBytes,
			RelayAllowanceBytes:   FreeRelayBytes,
			MaxAPIRequestsPerDay:  1000,
			MaxAPIRequestsPerHour: 600,
			MaxMigrationsPerDay:   20,
			HistoryRetentionDays:  7,
			LiveMigration:         false,
		},
		{
			Key:                   "pro",
			Name:                  "Pro",
			PriceCents:            2900,
			PriceCentsAnnual:      29000,
			Description:           "Personal machines, cloud checkpoints, history.",
			Features:              []string{"10 machines", "120 GiB checkpoint storage", "Live migration (peer-to-peer included)", "Periodic checkpoint policies", "Checkpoint history & lineage (90 days)", "Storage reclamation", "HA Pack add-on (+$39/mo warm-standby failover)", "SourceGuard Assurance add-on (+$19/mo)", "Priority support (first response within 2 business days)"},
			MaxMachines:           10,
			MaxStorageBytes:       ProStorageBytes,
			RelayAllowanceBytes:   FairUseBaseTransferBytes(ProStorageBytes),
			MaxAPIRequestsPerDay:  50000,
			MaxAPIRequestsPerHour: 6000,
			MaxMigrationsPerDay:   200,
			HistoryRetentionDays:  90,
			LiveMigration:         true,
			PeriodicPolicies:      true,
			StorageReclaim:        true,
			TrialEligible:         true,
		},
		{
			Key:                      "business",
			Name:                     "Business",
			PriceCents:               9900,
			PriceCentsAnnual:         99000,
			Description:              "Teams, permissions, audit logs.",
			Features:                 []string{"Unlimited machines", "500 GiB pooled + 100 GiB per seat checkpoint storage (2 TiB at 15 seats)", "Live migration (peer-to-peer included)", "Periodic checkpoint policies", "Warm-standby failover included", "SourceGuard Assurance included (12× credits)", "5 seats included · +$19/seat/mo", "Checkpoint history & lineage (1 year)", "Storage reclamation", "Team members & roles", "Audit logging", "SSO (OIDC) & SCIM provisioning", "Priority support (first response within 2 business days)"},
			MaxMachines:              Unlimited,
			MaxStorageBytes:          BusinessStorageBase,
			SeatDerivedStorage:       true,
			StoragePerSeatBytes:      BusinessStorageSeat,
			StorageCapBytes:          BusinessStorageCap,
			PerSeat:                  false,
			IncludedSeats:            5,
			SeatOverageCents:         1900,
			RelayAllowanceBytes:      FairUseBaseTransferBytes(BusinessStorageBase),
			MaxAPIRequestsPerDay:     Unlimited,
			MaxAPIRequestsPerHour:    30000,
			MaxMigrationsPerDay:      2000,
			HistoryRetentionDays:     365,
			LiveMigration:            true,
			TeamMembers:              true,
			AuditLogging:             true,
			SSOAndSCIM:               true,
			PeriodicPolicies:         true,
			Failover:                 true,
			SourceGuard:              true,
			SourceGuardCreditsMonths: 12,
			StorageReclaim:           true,
			TrialEligible:            true,
		},
		{
			Key:                      "enterprise",
			Name:                     "Enterprise",
			PriceCents:               Custom,
			PriceCentsAnnual:         Custom,
			Description:              "Custom infrastructure, security controls, support.",
			Features:                 []string{"Unlimited everything", "Self-hosted control plane (Enterprise agreement)", "Data residency via self-hosted deployment", "Dedicated support engineer (agreement-defined)", "Support terms and any SLA defined in your contract; no published SLA", "On-premise deployment"},
			MaxMachines:              Unlimited,
			MaxStorageBytes:          Unlimited,
			MaxAPIRequestsPerDay:     Unlimited,
			MaxAPIRequestsPerHour:    Unlimited,
			MaxMigrationsPerDay:      Unlimited,
			HistoryRetentionDays:     Custom,
			LiveMigration:            true,
			TeamMembers:              true,
			AuditLogging:             true,
			SSOAndSCIM:               true,
			PeriodicPolicies:         true,
			Failover:                 true,
			SourceGuard:              true,
			SourceGuardCreditsMonths: 12,
			StorageReclaim:           true,
		},
	}
}

// AddOn is a purchasable plan add-on carried as extra checkout line items.
// The webhook records add-ons in subscription metadata and the entitlement
// row stores the resulting capability flags.
type AddOn struct {
	Key         string  `json:"key"`
	Name        string  `json:"name"`
	PriceCents  int64   `json:"price_cents"`
	Description string  `json:"description"`
	Feature     Feature `json:"feature"`
	// BasePlans lists the plan keys the add-on may attach to.
	BasePlans []string `json:"base_plans"`
}

// AddOnCatalog returns the self-serve add-ons. HA Pack unlocks warm-standby
// failover on Pro; SourceGuard Assurance is the credit-backed guarantee and
// signed monthly attestation. Business includes both.
func AddOnCatalog() []AddOn {
	return []AddOn{
		{
			Key:         "ha",
			Name:        "HA Pack",
			PriceCents:  3900,
			Description: "Warm-standby failover designation and standby replication on Pro.",
			Feature:     FeatureFailover,
			BasePlans:   []string{"pro"},
		},
		{
			Key:         "sourceguard",
			Name:        "SourceGuard Assurance",
			PriceCents:  1900,
			Description: "Credit-back guarantee (3× monthly fee) and signed monthly migration attestation.",
			Feature:     FeatureSourceGuard,
			BasePlans:   []string{"pro"},
		},
	}
}

// FindAddOn resolves an add-on key.
func FindAddOn(key string) (AddOn, bool) {
	for _, addOn := range AddOnCatalog() {
		if addOn.Key == key {
			return addOn, true
		}
	}
	return AddOn{}, false
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

// EffectiveStorageBytes computes the checkpoint-storage quota a plan carries
// at a given purchased-seat count. Seat-derived plans pool base capacity plus
// per-seat capacity, hard-capped; every other plan returns its flat quota.
func EffectiveStorageBytes(plan Plan, seats int) int64 {
	if !plan.SeatDerivedStorage {
		return plan.MaxStorageBytes
	}
	if seats < 1 {
		seats = 1
	}
	total := plan.MaxStorageBytes + plan.StoragePerSeatBytes*int64(seats)
	if plan.StorageCapBytes > 0 && total > plan.StorageCapBytes {
		return plan.StorageCapBytes
	}
	return total
}

// FairUseTransferMultiplier is the per-organization monthly platform-mediated
// transfer ceiling expressed as a multiple of the storage quota. Operators may
// tighten or loosen it (0.1–10) via the fair_use_transfer_multiplier config
// key; 2.0 is the default of record.
var FairUseTransferMultiplier = 2.0

// ClampFairUseTransferMultiplier validates and stores an operator override.
// Values outside the supported range are refused so a typo cannot silently
// grant unbounded transfer.
func ClampFairUseTransferMultiplier(value float64) bool {
	if value < 0.1 || value > 10 {
		return false
	}
	FairUseTransferMultiplier = value
	return true
}

// FairUseBaseTransferBytes is the fair-use ceiling formula at the default
// multiplier: 2× the storage quota. Free lands on 20 GiB, matching the
// documented 20 GB/mo hard stop.
func FairUseBaseTransferBytes(storageBytes int64) int64 {
	return int64(float64(storageBytes) * 2.0)
}

// FairUseTransferBytes is the hard monthly transfer ceiling for one
// organization: fair_use_transfer_multiplier × effective storage quota. It is
// enforced fail-closed at relay-ticket issuance and restore/egress credential
// issuance; peer-to-peer transfers are metered but not throttled here (they
// are outside platform enforcement — see docs/limitations.md).
func FairUseTransferBytes(plan Plan, seats int) int64 {
	quota := EffectiveStorageBytes(plan, seats)
	if quota < 0 {
		return Unlimited
	}
	return int64(float64(quota) * FairUseTransferMultiplier)
}

// SeatCeiling returns the member ceiling a plan enforces at a purchased-seat
// count. Flat-with-included plans admit IncludedSeats at minimum and grow with
// purchased seats (the seats column stores the purchased ceiling = included +
// extra). Legacy per-seat plans treat the purchased count as the ceiling.
// Zero or less means unlimited.
func SeatCeiling(plan Plan, purchasedSeats int) int {
	if plan.IncludedSeats > 0 {
		if purchasedSeats < plan.IncludedSeats {
			return plan.IncludedSeats
		}
		return purchasedSeats
	}
	if plan.PerSeat {
		return purchasedSeats
	}
	return 0
}

// ExtraSeatQuantity is the Stripe subscription-item quantity a purchased-seat
// count bills as on a flat-with-included plan: extras only, never below one
// (Stripe requires a positive quantity on the subscription item).
func ExtraSeatQuantity(plan Plan, purchasedSeats int) int {
	if plan.IncludedSeats <= 0 {
		if purchasedSeats < 1 {
			return 1
		}
		return purchasedSeats
	}
	extra := purchasedSeats - plan.IncludedSeats
	if extra < 1 {
		return 1
	}
	return extra
}

// SeatOverageUnitAmount is the per-extra-seat amount one checkout line item
// bills at for the given interval. Monthly is the catalog's per-seat price;
// annual prepay bills twelve months of seat overage (the two-month discount
// covers the plan fee only). The extras line is a distinct item from the
// base plan price so seat growth can never multiply the base fee.
func SeatOverageUnitAmount(plan Plan, interval string) int64 {
	if plan.SeatOverageCents <= 0 {
		return 0
	}
	if interval == "year" {
		return plan.SeatOverageCents * 12
	}
	return plan.SeatOverageCents
}

// The live-migration gate's error contract. Every admission point that
// refuses a live-mode migration — the control plane's job routes, the agent
// command dispatcher, the CLI, and the agent's local API — returns this
// exact code and message so a paywall refusal is recognizable (and
// scriptable) wherever it is hit. The gate is a licensing boundary at the
// product surface, not DRM; see docs/limitations.md for what it does and
// does not promise.
const (
	LiveMigrationPlanRequiredCode    = "LIVE_MIGRATION_PLAN_REQUIRED"
	LiveMigrationPlanRequiredMessage = "live migration requires a paid plan; the free tier is cold-only"
)

// Refusal codes for the v2 gates. Every surface refuses with the same code
// and a next step (upgrade_url / trial_url) so scripts and humans both find
// the door. Status codes follow the existing contract: entitlement walls are
// 402, quota walls 403/402, throttles 429.
const (
	PeriodicPolicyPlanRequiredCode    = "PLAN_REQUIRED_PERIODIC_POLICY"
	PeriodicPolicyPlanRequiredMessage = "periodic checkpoint policies require a paid plan"
	FailoverPlanRequiredCode          = "FAIL_OVER_PLAN_REQUIRED"
	FailoverPlanRequiredMessage       = "warm-standby failover requires the HA Pack add-on or the Business plan"
	StorageReclaimPlanRequiredCode    = "PLAN_REQUIRED_STORAGE_RECLAIM"
	StorageReclaimPlanRequiredMessage = "storage reclamation requires a paid plan; free-tier mirrors are operator-side"
	RelayQuotaExceededCode            = "RELAY_QUOTA_EXCEEDED"
	RelayQuotaExceededMessage         = "monthly transfer allotment exhausted"
	TransferFairUseExceededCode       = "TRANSFER_FAIR_USE_EXCEEDED"
	TransferFairUseExceededMessage    = "monthly fair-use transfer ceiling reached; resets next calendar month (UTC)"
	OverageCapReachedCode             = "OVERAGE_CAP_REACHED"
	OverageCapReachedMessage          = "overage spend cap reached; raise the cap or upgrade to keep writing"
	RateLimitUpgradeCode              = "RATE_LIMIT_UPGRADE"
	RateLimitUpgradeMessage           = "API request budget exhausted for this plan"
	TrialAlreadyUsedCode              = "TRIAL_ALREADY_USED"
	TrialAlreadyUsedMessage           = "this organization has already used its trial"
	TrialActiveCode                   = "TRIAL_ACTIVE"
	// FeaturePlanRequiredCode is the generic plan-wall refusal. Features with
	// a dedicated code above refuse with it instead (FeatureRefusal), so the
	// code a script matches on names the exact wall it hit.
	FeaturePlanRequiredCode    = "FEATURE_PLAN_REQUIRED"
	FeaturePlanRequiredMessage = "this feature requires a plan that includes it and an active subscription; see GET /v1/plans"
)

// FeatureRefusal returns the published refusal code and message for one
// gated feature. Every surface refuses with the same code and a next step
// (upgrade_url / trial_url) so scripts and humans both find the door; the
// per-feature codes are that contract.
func FeatureRefusal(feature Feature) (code, message string) {
	switch feature {
	case FeatureLiveMigration:
		return LiveMigrationPlanRequiredCode, LiveMigrationPlanRequiredMessage
	case FeaturePeriodicPolicies:
		return PeriodicPolicyPlanRequiredCode, PeriodicPolicyPlanRequiredMessage
	case FeatureFailover:
		return FailoverPlanRequiredCode, FailoverPlanRequiredMessage
	case FeatureStorageReclaim:
		return StorageReclaimPlanRequiredCode, StorageReclaimPlanRequiredMessage
	default:
		return FeaturePlanRequiredCode, FeaturePlanRequiredMessage
	}
}

// TrialDays is the no-card Business trial length. One trial per organization,
// ever; expiry reverts limits without touching data.
const TrialDays = 14

// LiveMigrationAllowed is the single decision behind the live-migration
// paywall: live mode needs a catalog plan that includes live migration (or a
// negotiated tier the catalog does not know, which its contract governs) and
// a currently active subscription. A cancelled or lapsed subscription falls
// back to free-tier capability, which is cold-only; an empty plan key fails
// closed. Every enforcement point — platform-mediated job creation, agent
// command dispatch, the CLI's migrate pre-check, and the agent's local
// migration API — calls this function (or its trial-aware sibling
// EntitlementState.LiveMigrationAllowed) so the tiering the pricing page
// shows and the tiering every surface enforces cannot drift apart.
func LiveMigrationAllowed(planKey string, subscriptionActive bool) bool {
	if !subscriptionActive {
		return false
	}
	return PlanFeatureAllowed(planKey, FeatureLiveMigration)
}
