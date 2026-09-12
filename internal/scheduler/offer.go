// Package scheduler implements SHIFT Compute: the resource-offer inventory and
// the destination-selection algorithm that migration placement is built on.
//
// The package is deliberately free of transport, storage, and clock
// dependencies. A control plane, an agent, or a test supplies the offers and
// the request; the scheduler answers with a ranked, fully explained placement.
// Nothing here trades compute on its own: public trading is a configuration
// gate that is closed unless an operator opens it.
package scheduler

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"shift.dev/shift/internal/model"
)

// TrustTier orders how much the control plane can substantiate about the
// machine behind an offer. It is never taken at face value from the publisher:
// ClampTrust lowers a claimed tier to what the control plane can actually
// prove.
type TrustTier string

const (
	TrustUnverified   TrustTier = "unverified"
	TrustCommunity    TrustTier = "community"
	TrustOrganization TrustTier = "organization"
	TrustVerified     TrustTier = "verified"
)

var trustRank = map[TrustTier]int{
	TrustUnverified:   0,
	TrustCommunity:    1,
	TrustOrganization: 2,
	TrustVerified:     3,
}

func (tier TrustTier) Valid() bool {
	_, ok := trustRank[tier]
	return ok
}

func (tier TrustTier) Rank() int { return trustRank[tier] }

// AtLeast reports whether the tier satisfies a requested minimum.
func (tier TrustTier) AtLeast(minimum TrustTier) bool {
	if minimum == "" {
		return true
	}
	return tier.Valid() && minimum.Valid() && trustRank[tier] >= trustRank[minimum]
}

// ClampTrust reduces a claimed tier to the evidence available. An offer may
// only reach TrustOrganization when the requester shares the owning
// organization, and TrustVerified when the machine's signed identity was
// verified by the control plane.
func ClampTrust(claimed TrustTier, sameOrganization, identityVerified bool) TrustTier {
	if !claimed.Valid() {
		claimed = TrustUnverified
	}
	ceiling := TrustCommunity
	if sameOrganization {
		ceiling = TrustOrganization
	}
	if identityVerified && sameOrganization {
		ceiling = TrustVerified
	}
	if trustRank[claimed] > trustRank[ceiling] {
		return ceiling
	}
	return claimed
}

// Visibility controls who may be offered a machine.
type Visibility string

const (
	// VisibilityPrivate keeps an offer usable only by the machine's own owner.
	VisibilityPrivate Visibility = "private"
	// VisibilityOrganization exposes an offer to the owning organization.
	VisibilityOrganization Visibility = "organization"
	// VisibilityPublic exposes an offer to the marketplace. Public offers are
	// only considered when compute trading is explicitly enabled.
	VisibilityPublic Visibility = "public"
)

func (visibility Visibility) Valid() bool {
	switch visibility {
	case VisibilityPrivate, VisibilityOrganization, VisibilityPublic:
		return true
	default:
		return false
	}
}

// OfferStatus is the lifecycle of a published offer.
type OfferStatus string

const (
	OfferAvailable OfferStatus = "available"
	OfferReserved  OfferStatus = "reserved"
	OfferDraining  OfferStatus = "draining"
	OfferOffline   OfferStatus = "offline"
	OfferWithdrawn OfferStatus = "withdrawn"
)

func (status OfferStatus) Valid() bool {
	switch status {
	case OfferAvailable, OfferReserved, OfferDraining, OfferOffline, OfferWithdrawn:
		return true
	default:
		return false
	}
}

// Resources is the quantity of each resource class an offer exposes, or that a
// reservation has already committed. Every field is optional: a machine may
// expose CPU and RAM without exposing GPUs or bandwidth.
type Resources struct {
	CPUCount     float64           `json:"cpu_count,omitempty"`
	MemoryBytes  uint64            `json:"memory_bytes,omitempty"`
	StorageBytes uint64            `json:"storage_bytes,omitempty"`
	NetworkMbps  float64           `json:"network_mbps,omitempty"`
	GPUs         []model.GPUDevice `json:"gpus,omitempty"`
}

// Subtract returns the resources left after committing used. Quantities are
// clamped at zero rather than wrapping, and GPUs are removed by matching each
// committed device against the remaining pool.
func (resources Resources) Subtract(used Resources) Resources {
	remaining := Resources{
		CPUCount:     resources.CPUCount - used.CPUCount,
		NetworkMbps:  resources.NetworkMbps - used.NetworkMbps,
		MemoryBytes:  subtractBytes(resources.MemoryBytes, used.MemoryBytes),
		StorageBytes: subtractBytes(resources.StorageBytes, used.StorageBytes),
	}
	if remaining.CPUCount < 0 {
		remaining.CPUCount = 0
	}
	if remaining.NetworkMbps < 0 {
		remaining.NetworkMbps = 0
	}
	remaining.GPUs = remainingGPUs(resources.GPUs, used.GPUs)
	return remaining
}

func subtractBytes(total, used uint64) uint64 {
	if used >= total {
		return 0
	}
	return total - used
}

func remainingGPUs(pool, used []model.GPUDevice) []model.GPUDevice {
	if len(pool) == 0 {
		return nil
	}
	remaining := make([]model.GPUDevice, len(pool))
	copy(remaining, pool)
	for _, device := range used {
		index := indexOfGPU(remaining, device)
		if index < 0 {
			continue
		}
		remaining = append(remaining[:index], remaining[index+1:]...)
	}
	if len(remaining) == 0 {
		return nil
	}
	return remaining
}

func indexOfGPU(pool []model.GPUDevice, wanted model.GPUDevice) int {
	for index := range pool {
		if strings.EqualFold(pool[index].Vendor, wanted.Vendor) && strings.EqualFold(pool[index].Model, wanted.Model) {
			return index
		}
	}
	for index := range pool {
		if strings.EqualFold(pool[index].Vendor, wanted.Vendor) {
			return index
		}
	}
	return -1
}

// Pricing is quoted in integer micro-units of Currency so money never passes
// through binary floating point. A micro-unit is one millionth of the currency
// unit, i.e. 1_000_000 micros of "USD" is one US dollar.
type Pricing struct {
	Currency              string `json:"currency"`
	CPUHourMicros         int64  `json:"cpu_hour_micros,omitempty"`
	MemoryGiBHourMicros   int64  `json:"memory_gib_hour_micros,omitempty"`
	StorageGiBHourMicros  int64  `json:"storage_gib_hour_micros,omitempty"`
	GPUHourMicros         int64  `json:"gpu_hour_micros,omitempty"`
	EgressGiBMicros       int64  `json:"egress_gib_micros,omitempty"`
	MinimumChargeMicros   int64  `json:"minimum_charge_micros,omitempty"`
	BillingIncrementHours int64  `json:"billing_increment_hours,omitempty"`
}

// Free reports whether the offer costs nothing, which is the case for a
// machine an operator exposes to their own organization.
func (pricing Pricing) Free() bool {
	return pricing.CPUHourMicros == 0 && pricing.MemoryGiBHourMicros == 0 && pricing.StorageGiBHourMicros == 0 &&
		pricing.GPUHourMicros == 0 && pricing.EgressGiBMicros == 0 && pricing.MinimumChargeMicros == 0
}

func (pricing Pricing) validate() error {
	currency := strings.ToUpper(strings.TrimSpace(pricing.Currency))
	if pricing.Free() {
		if currency != "" && len(currency) != 3 {
			return errors.New("offer currency must be a three-letter ISO 4217 code")
		}
		return nil
	}
	if len(currency) != 3 {
		return errors.New("a priced offer requires a three-letter ISO 4217 currency code")
	}
	for name, value := range map[string]int64{
		"cpu_hour_micros":         pricing.CPUHourMicros,
		"memory_gib_hour_micros":  pricing.MemoryGiBHourMicros,
		"storage_gib_hour_micros": pricing.StorageGiBHourMicros,
		"gpu_hour_micros":         pricing.GPUHourMicros,
		"egress_gib_micros":       pricing.EgressGiBMicros,
		"minimum_charge_micros":   pricing.MinimumChargeMicros,
	} {
		if value < 0 {
			return errors.New("offer " + name + " cannot be negative")
		}
	}
	if pricing.BillingIncrementHours < 0 {
		return errors.New("offer billing_increment_hours cannot be negative")
	}
	return nil
}

// Cost estimates the charge in micro-units for running the given resources for
// the given duration, plus egress bytes. Durations are rounded up to the
// billing increment, which is how a real provider bills a partial hour.
func (pricing Pricing) Cost(resources Resources, duration time.Duration, egressBytes uint64) int64 {
	if duration < 0 {
		duration = 0
	}
	increment := time.Duration(pricing.BillingIncrementHours) * time.Hour
	billable := duration
	if increment > 0 {
		units := (int64(duration) + int64(increment) - 1) / int64(increment)
		billable = time.Duration(units * int64(increment))
	}
	hours := billable.Hours()
	total := int64(0)
	total += roundMicros(float64(pricing.CPUHourMicros) * resources.CPUCount * hours)
	total += roundMicros(float64(pricing.MemoryGiBHourMicros) * gibibytes(resources.MemoryBytes) * hours)
	total += roundMicros(float64(pricing.StorageGiBHourMicros) * gibibytes(resources.StorageBytes) * hours)
	total += roundMicros(float64(pricing.GPUHourMicros) * float64(len(resources.GPUs)) * hours)
	total += roundMicros(float64(pricing.EgressGiBMicros) * gibibytes(egressBytes))
	if total < pricing.MinimumChargeMicros {
		total = pricing.MinimumChargeMicros
	}
	return total
}

// HourlyRate is the cost of one hour of the given resources, which is the
// figure price constraints and price scoring compare.
func (pricing Pricing) HourlyRate(resources Resources) int64 {
	rate := Pricing{
		Currency:             pricing.Currency,
		CPUHourMicros:        pricing.CPUHourMicros,
		MemoryGiBHourMicros:  pricing.MemoryGiBHourMicros,
		StorageGiBHourMicros: pricing.StorageGiBHourMicros,
		GPUHourMicros:        pricing.GPUHourMicros,
	}
	return rate.Cost(resources, time.Hour, 0)
}

func gibibytes(value uint64) float64 { return float64(value) / (1 << 30) }

func roundMicros(value float64) int64 {
	if value <= 0 {
		return 0
	}
	return int64(value + 0.5)
}

// Geography describes where an offered machine physically runs. Data-residency
// constraints are enforced against Country, which must be an ISO 3166-1
// alpha-2 code so that "DE" and "de" cannot describe different places.
type Geography struct {
	Region  string `json:"region,omitempty"`
	Country string `json:"country,omitempty"`
	Zone    string `json:"zone,omitempty"`
}

func (geography Geography) normalize() Geography {
	return Geography{
		Region:  strings.ToLower(strings.TrimSpace(geography.Region)),
		Country: strings.ToUpper(strings.TrimSpace(geography.Country)),
		Zone:    strings.ToLower(strings.TrimSpace(geography.Zone)),
	}
}

func (geography Geography) validate() error {
	if geography.Country != "" && len(geography.Country) != 2 {
		return errors.New("offer country must be an ISO 3166-1 alpha-2 code")
	}
	return nil
}

// Policy is what the machine owner is willing to accept. Every field is a hard
// constraint: the scheduler rejects rather than downgrades a candidate whose
// policy is not satisfied.
type Policy struct {
	Visibility            Visibility `json:"visibility"`
	AllowedOrganizations  []string   `json:"allowed_organizations,omitempty"`
	DeniedOrganizations   []string   `json:"denied_organizations,omitempty"`
	AllowedCountries      []string   `json:"allowed_countries,omitempty"`
	MaxStateBytes         uint64     `json:"max_state_bytes,omitempty"`
	MaxDurationHours      int64      `json:"max_duration_hours,omitempty"`
	RequireEncryptedState bool       `json:"require_encrypted_state"`
	GPUSharingAllowed     bool       `json:"gpu_sharing_allowed"`
}

func (policy Policy) normalize() Policy {
	normalized := policy
	normalized.AllowedOrganizations = normalizeList(policy.AllowedOrganizations, false)
	normalized.DeniedOrganizations = normalizeList(policy.DeniedOrganizations, false)
	normalized.AllowedCountries = normalizeList(policy.AllowedCountries, true)
	if normalized.Visibility == "" {
		normalized.Visibility = VisibilityOrganization
	}
	return normalized
}

func normalizeList(values []string, upper bool) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if upper {
			trimmed = strings.ToUpper(trimmed)
		}
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		result = append(result, trimmed)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func containsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(value, wanted) {
			return true
		}
	}
	return false
}

// Availability is when an offer can be used and how fresh the machine's last
// contact is. An offer whose machine has stopped reporting is never a candidate,
// because a placement onto a silent machine would be a false promise.
type Availability struct {
	Status     OfferStatus `json:"status"`
	WindowFrom *time.Time  `json:"window_from,omitempty"`
	WindowTo   *time.Time  `json:"window_to,omitempty"`
	LastSeenAt *time.Time  `json:"last_seen_at,omitempty"`
}

func (availability Availability) validate() error {
	if !availability.Status.Valid() {
		return fmt.Errorf("unsupported offer status %q", availability.Status)
	}
	if availability.WindowFrom != nil && availability.WindowTo != nil && !availability.WindowTo.After(*availability.WindowFrom) {
		return errors.New("offer availability window must end after it starts")
	}
	return nil
}

// LatencySample is a measured round-trip time from an observer to the offered
// machine. The scheduler never estimates latency: an unmeasured path stays
// unmeasured, and a request that constrains latency rejects such an offer.
type LatencySample struct {
	// Observer is a machine id, "region:<region>", or "country:<code>".
	Observer    string    `json:"observer"`
	RoundTripMs float64   `json:"round_trip_ms"`
	MeasuredAt  time.Time `json:"measured_at"`
	SampleCount int       `json:"sample_count,omitempty"`
}

// Offer is a machine's advertised, optionally exposed capacity.
type Offer struct {
	ID             string       `json:"id"`
	OrganizationID string       `json:"organization_id"`
	MachineID      string       `json:"machine_id"`
	MachineName    string       `json:"machine_name,omitempty"`
	AgentURL       string       `json:"agent_url,omitempty"`
	Exposed        Resources    `json:"exposed"`
	Committed      Resources    `json:"committed"`
	Pricing        Pricing      `json:"pricing"`
	Geography      Geography    `json:"geography"`
	Policy         Policy       `json:"policy"`
	Availability   Availability `json:"availability"`
	Trust          TrustTier    `json:"trust"`
	// IdentityVerified is set by whoever assembled the inventory, and only when
	// the machine's signed identity was actually checked. It is never inferred
	// from the offer's own contents, because a publisher must not be able to
	// promote its own trust tier.
	IdentityVerified bool                      `json:"identity_verified"`
	Capabilities     model.MachineCapabilities `json:"capabilities"`
	Latency          []LatencySample           `json:"latency,omitempty"`
	CreatedAt        time.Time                 `json:"created_at"`
	UpdatedAt        time.Time                 `json:"updated_at"`
}

// Available is the capacity left after existing reservations.
func (offer Offer) Available() Resources { return offer.Exposed.Subtract(offer.Committed) }

// Normalize lowercases and trims the fields the scheduler compares, and applies
// the defaults that make an unspecified offer safe rather than permissive.
func (offer *Offer) Normalize() {
	offer.ID = strings.TrimSpace(offer.ID)
	offer.OrganizationID = strings.TrimSpace(offer.OrganizationID)
	offer.MachineID = strings.TrimSpace(offer.MachineID)
	offer.MachineName = strings.TrimSpace(offer.MachineName)
	offer.AgentURL = strings.TrimRight(strings.TrimSpace(offer.AgentURL), "/")
	offer.Pricing.Currency = strings.ToUpper(strings.TrimSpace(offer.Pricing.Currency))
	offer.Geography = offer.Geography.normalize()
	offer.Policy = offer.Policy.normalize()
	if offer.Availability.Status == "" {
		offer.Availability.Status = OfferOffline
	}
	if !offer.Trust.Valid() {
		offer.Trust = TrustUnverified
	}
}

// Validate rejects an offer the scheduler could not reason about. It is called
// before an offer is persisted so that stored inventory is always schedulable.
func (offer *Offer) Validate() error {
	offer.Normalize()
	if offer.ID == "" {
		return errors.New("offer id is required")
	}
	if offer.OrganizationID == "" {
		return errors.New("offer organization id is required")
	}
	if offer.MachineID == "" {
		return errors.New("offer machine id is required")
	}
	if !offer.Policy.Visibility.Valid() {
		return fmt.Errorf("unsupported offer visibility %q", offer.Policy.Visibility)
	}
	if offer.Exposed.CPUCount < 0 || offer.Exposed.NetworkMbps < 0 {
		return errors.New("exposed cpu and network quantities cannot be negative")
	}
	if offer.Committed.CPUCount < 0 || offer.Committed.NetworkMbps < 0 {
		return errors.New("committed cpu and network quantities cannot be negative")
	}
	if offer.Exposed.CPUCount == 0 && offer.Exposed.MemoryBytes == 0 && offer.Exposed.StorageBytes == 0 && offer.Exposed.NetworkMbps == 0 && len(offer.Exposed.GPUs) == 0 {
		return errors.New("an offer must expose at least one resource")
	}
	if offer.Exposed.CPUCount > 0 && offer.Capabilities.CPUs > 0 && offer.Exposed.CPUCount > float64(offer.Capabilities.CPUs) {
		return fmt.Errorf("offer exposes %.2f CPUs but the machine reports %d", offer.Exposed.CPUCount, offer.Capabilities.CPUs)
	}
	if offer.Exposed.MemoryBytes > 0 && offer.Capabilities.MemoryBytes > 0 && offer.Exposed.MemoryBytes > offer.Capabilities.MemoryBytes {
		return fmt.Errorf("offer exposes %d memory bytes but the machine reports %d", offer.Exposed.MemoryBytes, offer.Capabilities.MemoryBytes)
	}
	if err := offer.Pricing.validate(); err != nil {
		return err
	}
	if err := offer.Geography.validate(); err != nil {
		return err
	}
	if err := offer.Availability.validate(); err != nil {
		return err
	}
	if offer.Policy.MaxDurationHours < 0 {
		return errors.New("offer max_duration_hours cannot be negative")
	}
	for _, sample := range offer.Latency {
		if strings.TrimSpace(sample.Observer) == "" {
			return errors.New("a latency sample requires an observer")
		}
		if sample.RoundTripMs < 0 {
			return errors.New("a latency sample cannot be negative")
		}
	}
	return nil
}

// LatencyTo returns the measured round-trip time from an observer, preferring
// the most specific known path: the exact machine, then its region, then its
// country. The boolean reports whether any measurement existed.
func (offer Offer) LatencyTo(machineID string, geography Geography) (float64, bool) {
	geography = geography.normalize()
	keys := make([]string, 0, 3)
	if machineID = strings.TrimSpace(machineID); machineID != "" {
		keys = append(keys, machineID)
	}
	if geography.Region != "" {
		keys = append(keys, "region:"+geography.Region)
	}
	if geography.Country != "" {
		keys = append(keys, "country:"+geography.Country)
	}
	for _, key := range keys {
		for _, sample := range offer.Latency {
			if strings.EqualFold(strings.TrimSpace(sample.Observer), key) {
				return sample.RoundTripMs, true
			}
		}
	}
	return 0, false
}
