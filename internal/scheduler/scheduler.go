package scheduler

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"shift.dev/shift/internal/compatibility"
	"shift.dev/shift/internal/model"
)

// Rejection codes. Every filtered offer carries one so an operator can see why
// a machine was not chosen instead of guessing.
const (
	RejectTradingDisabled        = "TRADING_DISABLED"
	RejectVisibility             = "VISIBILITY_FORBIDDEN"
	RejectOrganizationDenied     = "ORGANIZATION_DENIED"
	RejectOrganizationNotAllowed = "ORGANIZATION_NOT_ALLOWED"
	RejectSelfPlacement          = "SELF_PLACEMENT"
	RejectExcluded               = "MACHINE_EXCLUDED"
	RejectStatus                 = "OFFER_UNAVAILABLE"
	RejectWindow                 = "OUTSIDE_AVAILABILITY_WINDOW"
	RejectStale                  = "MACHINE_PRESENCE_STALE"
	RejectTrust                  = "TRUST_TOO_LOW"
	RejectRegion                 = "REGION_NOT_ALLOWED"
	RejectCountry                = "COUNTRY_NOT_ALLOWED"
	RejectResidency              = "DATA_RESIDENCY_FORBIDDEN"
	RejectCPU                    = "CPU_INSUFFICIENT"
	RejectMemory                 = "MEMORY_INSUFFICIENT"
	RejectStorage                = "STORAGE_INSUFFICIENT"
	RejectNetwork                = "NETWORK_INSUFFICIENT"
	RejectGPU                    = "GPU_INCOMPATIBLE"
	RejectFeature                = "FEATURE_MISSING"
	RejectLatency                = "LATENCY_EXCEEDED"
	RejectLatencyUnknown         = "LATENCY_UNMEASURED"
	RejectPrice                  = "PRICE_EXCEEDED"
	RejectCurrency               = "CURRENCY_MISMATCH"
	RejectStateSize              = "STATE_TOO_LARGE"
	RejectDuration               = "DURATION_TOO_LONG"
	RejectEncryption             = "ENCRYPTED_STATE_REQUIRED"
	RejectGPUSharing             = "GPU_SHARING_FORBIDDEN"
	RejectCompatibility          = "RESTORE_INCOMPATIBLE"
)

// Constraints are the caller's hard requirements. An empty Constraints means
// "anything my organization already owns", which is the safe default: public
// marketplace offers additionally require trading to be enabled server side.
type Constraints struct {
	AllowedRegions            []string  `json:"allowed_regions,omitempty"`
	DeniedRegions             []string  `json:"denied_regions,omitempty"`
	AllowedCountries          []string  `json:"allowed_countries,omitempty"`
	DeniedCountries           []string  `json:"denied_countries,omitempty"`
	MinTrust                  TrustTier `json:"min_trust,omitempty"`
	MaxPriceMicrosPerHour     int64     `json:"max_price_micros_per_hour,omitempty"`
	Currency                  string    `json:"currency,omitempty"`
	MaxLatencyMillis          float64   `json:"max_latency_millis,omitempty"`
	RequiredFeatures          []string  `json:"required_features,omitempty"`
	ExcludeMachineIDs         []string  `json:"exclude_machine_ids,omitempty"`
	IncludeOtherOrganizations bool      `json:"include_other_organizations,omitempty"`
	StateIsEncrypted          bool      `json:"state_is_encrypted,omitempty"`
	SharesGPU                 bool      `json:"shares_gpu,omitempty"`
}

// Weights tune the relative importance of each scoring dimension. They are
// normalized before use, so callers may express them in any scale.
type Weights struct {
	Capability float64 `json:"capability"`
	Price      float64 `json:"price"`
	Latency    float64 `json:"latency"`
	Trust      float64 `json:"trust"`
	Locality   float64 `json:"locality"`
	Freshness  float64 `json:"freshness"`
}

// DefaultWeights favor a destination that can actually hold the workload and
// is close to it, without letting price dominate a fleet where most offers are
// free because the operator owns them.
func DefaultWeights() Weights {
	return Weights{Capability: 0.20, Price: 0.25, Latency: 0.20, Trust: 0.15, Locality: 0.10, Freshness: 0.10}
}

func (weights Weights) normalized() Weights {
	total := weights.Capability + weights.Price + weights.Latency + weights.Trust + weights.Locality + weights.Freshness
	if total <= 0 {
		return DefaultWeights()
	}
	return Weights{
		Capability: weights.Capability / total,
		Price:      weights.Price / total,
		Latency:    weights.Latency / total,
		Trust:      weights.Trust / total,
		Locality:   weights.Locality / total,
		Freshness:  weights.Freshness / total,
	}
}

// Request asks for a destination for one workload.
type Request struct {
	WorkloadID            string        `json:"workload_id,omitempty"`
	RequesterOrganization string        `json:"requester_organization"`
	SourceMachineID       string        `json:"source_machine_id,omitempty"`
	SourceGeography       Geography     `json:"source_geography,omitempty"`
	Requirements          Resources     `json:"requirements"`
	StateBytes            uint64        `json:"state_bytes,omitempty"`
	Duration              time.Duration `json:"duration,omitempty"`
	Constraints           Constraints   `json:"constraints,omitempty"`
	Weights               *Weights      `json:"weights,omitempty"`
	Limit                 int           `json:"limit,omitempty"`
	// Manifest, when set, makes placement run the same restore-compatibility
	// check the migration engine runs, so a scheduled destination cannot pass
	// scheduling and then fail pre-flight.
	Manifest *model.CheckpointManifest `json:"manifest,omitempty"`
}

func (request *Request) normalize() error {
	request.RequesterOrganization = strings.TrimSpace(request.RequesterOrganization)
	if request.RequesterOrganization == "" {
		return errors.New("a placement request requires the requesting organization")
	}
	request.SourceMachineID = strings.TrimSpace(request.SourceMachineID)
	request.SourceGeography = request.SourceGeography.normalize()
	request.Constraints.AllowedRegions = normalizeList(request.Constraints.AllowedRegions, false)
	request.Constraints.DeniedRegions = normalizeList(request.Constraints.DeniedRegions, false)
	request.Constraints.AllowedCountries = normalizeList(request.Constraints.AllowedCountries, true)
	request.Constraints.DeniedCountries = normalizeList(request.Constraints.DeniedCountries, true)
	request.Constraints.RequiredFeatures = normalizeList(request.Constraints.RequiredFeatures, false)
	request.Constraints.ExcludeMachineIDs = normalizeList(request.Constraints.ExcludeMachineIDs, false)
	request.Constraints.Currency = strings.ToUpper(strings.TrimSpace(request.Constraints.Currency))
	if request.Constraints.MinTrust != "" && !request.Constraints.MinTrust.Valid() {
		return fmt.Errorf("unsupported minimum trust tier %q", request.Constraints.MinTrust)
	}
	if request.Constraints.MaxPriceMicrosPerHour < 0 {
		return errors.New("maximum price cannot be negative")
	}
	if request.Constraints.MaxLatencyMillis < 0 {
		return errors.New("maximum latency cannot be negative")
	}
	if request.Duration < 0 {
		return errors.New("placement duration cannot be negative")
	}
	if request.Requirements.CPUCount < 0 || request.Requirements.NetworkMbps < 0 {
		return errors.New("resource requirements cannot be negative")
	}
	if request.Limit < 0 {
		return errors.New("placement limit cannot be negative")
	}
	return nil
}

// Scores are the per-dimension results that produced a candidate's total. They
// are part of the API because an unexplained placement decision is not
// actionable.
type Scores struct {
	Capability float64 `json:"capability"`
	Price      float64 `json:"price"`
	Latency    float64 `json:"latency"`
	Trust      float64 `json:"trust"`
	Locality   float64 `json:"locality"`
	Freshness  float64 `json:"freshness"`
	Total      float64 `json:"total"`
}

// Candidate is a feasible destination with its score and quoted cost.
type Candidate struct {
	OfferID          string                     `json:"offer_id"`
	MachineID        string                     `json:"machine_id"`
	MachineName      string                     `json:"machine_name,omitempty"`
	OrganizationID   string                     `json:"organization_id"`
	AgentURL         string                     `json:"agent_url,omitempty"`
	Geography        Geography                  `json:"geography"`
	Trust            TrustTier                  `json:"trust"`
	SameOrganization bool                       `json:"same_organization"`
	Available        Resources                  `json:"available"`
	HourlyMicros     int64                      `json:"hourly_micros"`
	EstimatedMicros  int64                      `json:"estimated_micros"`
	Currency         string                     `json:"currency,omitempty"`
	LatencyMillis    float64                    `json:"latency_millis,omitempty"`
	LatencyKnown     bool                       `json:"latency_known"`
	Scores           Scores                     `json:"scores"`
	Warnings         []string                   `json:"warnings,omitempty"`
	Compatibility    *model.CompatibilityReport `json:"compatibility,omitempty"`
}

// Rejection records an infeasible offer and the single reason it lost.
type Rejection struct {
	OfferID   string `json:"offer_id"`
	MachineID string `json:"machine_id"`
	Code      string `json:"code"`
	Reason    string `json:"reason"`
}

// Placement is the full, explained result of one scheduling pass.
type Placement struct {
	Candidates     []Candidate `json:"candidates"`
	Rejected       []Rejection `json:"rejected"`
	Evaluated      int         `json:"evaluated"`
	TradingEnabled bool        `json:"trading_enabled"`
	Weights        Weights     `json:"weights"`
	EvaluatedAt    time.Time   `json:"evaluated_at"`
}

// Best returns the top-ranked candidate.
func (placement Placement) Best() (Candidate, bool) {
	if len(placement.Candidates) == 0 {
		return Candidate{}, false
	}
	return placement.Candidates[0], true
}

// Config controls the scheduler instance. PublicTradingEnabled is false in
// every default configuration: the marketplace infrastructure exists, and
// selling compute to other organizations stays switched off until an operator
// turns it on.
type Config struct {
	PublicTradingEnabled bool
	StaleAfter           time.Duration
	Weights              Weights
	Now                  func() time.Time
}

// DefaultConfig returns the production defaults.
func DefaultConfig() Config {
	return Config{PublicTradingEnabled: false, StaleAfter: 2 * time.Minute, Weights: DefaultWeights()}
}

// Scheduler selects destinations. It holds no state, so one instance is safe
// for concurrent use.
type Scheduler struct {
	config  Config
	checker compatibility.Checker
}

func New(configuration Config) *Scheduler {
	if configuration.StaleAfter <= 0 {
		configuration.StaleAfter = 2 * time.Minute
	}
	if configuration.Now == nil {
		configuration.Now = func() time.Time { return time.Now().UTC() }
	}
	configuration.Weights = configuration.Weights.normalized()
	return &Scheduler{config: configuration}
}

// TradingEnabled reports whether public compute trading is switched on.
func (scheduler *Scheduler) TradingEnabled() bool { return scheduler.config.PublicTradingEnabled }

// Place evaluates every offer against the request and returns a ranked
// placement. Offers are never mutated.
func (scheduler *Scheduler) Place(request Request, offers []Offer) (Placement, error) {
	if err := request.normalize(); err != nil {
		return Placement{}, err
	}
	weights := scheduler.config.Weights
	if request.Weights != nil {
		weights = request.Weights.normalized()
	}
	now := scheduler.config.Now()
	placement := Placement{TradingEnabled: scheduler.config.PublicTradingEnabled, Weights: weights, EvaluatedAt: now, Evaluated: len(offers)}
	type feasible struct {
		offer     Offer
		candidate Candidate
	}
	accepted := make([]feasible, 0, len(offers))
	for _, offer := range offers {
		offer.Normalize()
		candidate, rejection := scheduler.evaluate(request, offer, now)
		if rejection != nil {
			placement.Rejected = append(placement.Rejected, *rejection)
			continue
		}
		accepted = append(accepted, feasible{offer: offer, candidate: candidate})
	}
	sort.Slice(placement.Rejected, func(i, j int) bool {
		if placement.Rejected[i].Code != placement.Rejected[j].Code {
			return placement.Rejected[i].Code < placement.Rejected[j].Code
		}
		return placement.Rejected[i].OfferID < placement.Rejected[j].OfferID
	})
	if len(accepted) == 0 {
		placement.Candidates = []Candidate{}
		return placement, nil
	}
	minimumRate, maximumRate := accepted[0].candidate.HourlyMicros, accepted[0].candidate.HourlyMicros
	minimumLatency, maximumLatency := 0.0, 0.0
	latencySeen := false
	for _, entry := range accepted {
		if entry.candidate.HourlyMicros < minimumRate {
			minimumRate = entry.candidate.HourlyMicros
		}
		if entry.candidate.HourlyMicros > maximumRate {
			maximumRate = entry.candidate.HourlyMicros
		}
		if !entry.candidate.LatencyKnown {
			continue
		}
		if !latencySeen {
			minimumLatency, maximumLatency, latencySeen = entry.candidate.LatencyMillis, entry.candidate.LatencyMillis, true
			continue
		}
		if entry.candidate.LatencyMillis < minimumLatency {
			minimumLatency = entry.candidate.LatencyMillis
		}
		if entry.candidate.LatencyMillis > maximumLatency {
			maximumLatency = entry.candidate.LatencyMillis
		}
	}
	candidates := make([]Candidate, 0, len(accepted))
	for _, entry := range accepted {
		candidate := entry.candidate
		candidate.Scores.Capability = capabilityScore(request.Requirements, candidate.Available)
		candidate.Scores.Price = invertedScore(float64(candidate.HourlyMicros), float64(minimumRate), float64(maximumRate))
		if candidate.LatencyKnown {
			candidate.Scores.Latency = invertedScore(candidate.LatencyMillis, minimumLatency, maximumLatency)
		} else {
			candidate.Scores.Latency = 0.5
		}
		candidate.Scores.Trust = float64(candidate.Trust.Rank()) / float64(trustRank[TrustVerified])
		candidate.Scores.Locality = localityScore(request.SourceGeography, candidate.Geography)
		candidate.Scores.Freshness = freshnessScore(entry.offer.Availability.LastSeenAt, now, scheduler.config.StaleAfter)
		candidate.Scores.Total = weights.Capability*candidate.Scores.Capability +
			weights.Price*candidate.Scores.Price +
			weights.Latency*candidate.Scores.Latency +
			weights.Trust*candidate.Scores.Trust +
			weights.Locality*candidate.Scores.Locality +
			weights.Freshness*candidate.Scores.Freshness
		candidates = append(candidates, candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Scores.Total != candidates[j].Scores.Total {
			return candidates[i].Scores.Total > candidates[j].Scores.Total
		}
		if candidates[i].HourlyMicros != candidates[j].HourlyMicros {
			return candidates[i].HourlyMicros < candidates[j].HourlyMicros
		}
		return candidates[i].OfferID < candidates[j].OfferID
	})
	if request.Limit > 0 && len(candidates) > request.Limit {
		candidates = candidates[:request.Limit]
	}
	placement.Candidates = candidates
	return placement, nil
}

// RejectCrossOrganization is returned when an offer belongs to another
// organization but the request did not opt into that. A migration must never
// silently land on a machine the requester does not own.
const RejectCrossOrganization = "CROSS_ORGANIZATION_NOT_REQUESTED"

func (scheduler *Scheduler) evaluate(request Request, offer Offer, now time.Time) (Candidate, *Rejection) {
	reject := func(code, reason string) *Rejection {
		return &Rejection{OfferID: offer.ID, MachineID: offer.MachineID, Code: code, Reason: reason}
	}
	sameOrganization := offer.OrganizationID == request.RequesterOrganization
	if containsFold(request.Constraints.ExcludeMachineIDs, offer.MachineID) {
		return Candidate{}, reject(RejectExcluded, "the request excluded this machine")
	}
	if request.SourceMachineID != "" && strings.EqualFold(offer.MachineID, request.SourceMachineID) {
		return Candidate{}, reject(RejectSelfPlacement, "a workload cannot be placed on the machine it already runs on")
	}
	if offer.Availability.Status != OfferAvailable {
		return Candidate{}, reject(RejectStatus, "offer status is "+string(offer.Availability.Status))
	}
	if from := offer.Availability.WindowFrom; from != nil && now.Before(*from) {
		return Candidate{}, reject(RejectWindow, "the availability window opens at "+from.UTC().Format(time.RFC3339))
	}
	if to := offer.Availability.WindowTo; to != nil && !now.Before(*to) {
		return Candidate{}, reject(RejectWindow, "the availability window closed at "+to.UTC().Format(time.RFC3339))
	}
	lastSeen := offer.Availability.LastSeenAt
	if lastSeen == nil {
		return Candidate{}, reject(RejectStale, "the machine has never reported presence")
	}
	if age := now.Sub(*lastSeen); age > scheduler.config.StaleAfter {
		return Candidate{}, reject(RejectStale, fmt.Sprintf("the machine last reported %s ago, beyond the %s freshness limit", age.Round(time.Second), scheduler.config.StaleAfter))
	}
	if containsFold(offer.Policy.DeniedOrganizations, request.RequesterOrganization) {
		return Candidate{}, reject(RejectOrganizationDenied, "the machine owner denied this organization")
	}
	if !sameOrganization {
		if !request.Constraints.IncludeOtherOrganizations {
			return Candidate{}, reject(RejectCrossOrganization, "the offer belongs to another organization and the request did not include other organizations")
		}
		switch offer.Policy.Visibility {
		case VisibilityPrivate:
			return Candidate{}, reject(RejectVisibility, "the offer is private to its owning organization")
		case VisibilityOrganization:
			if !containsFold(offer.Policy.AllowedOrganizations, request.RequesterOrganization) {
				return Candidate{}, reject(RejectOrganizationNotAllowed, "the offer is shared with specific organizations that do not include yours")
			}
		case VisibilityPublic:
			if !scheduler.config.PublicTradingEnabled {
				return Candidate{}, reject(RejectTradingDisabled, "public compute trading is disabled on this control plane")
			}
		}
	}
	trust := ClampTrust(offer.Trust, sameOrganization, offer.IdentityVerified)
	if !trust.AtLeast(request.Constraints.MinTrust) {
		return Candidate{}, reject(RejectTrust, "offer trust "+string(trust)+" is below the requested minimum "+string(request.Constraints.MinTrust))
	}
	if len(request.Constraints.AllowedRegions) > 0 && !containsFold(request.Constraints.AllowedRegions, offer.Geography.Region) {
		return Candidate{}, reject(RejectRegion, "region "+offer.Geography.Region+" is not in the allowed set")
	}
	if containsFold(request.Constraints.DeniedRegions, offer.Geography.Region) {
		return Candidate{}, reject(RejectRegion, "region "+offer.Geography.Region+" is denied")
	}
	if len(request.Constraints.AllowedCountries) > 0 && !containsFold(request.Constraints.AllowedCountries, offer.Geography.Country) {
		return Candidate{}, reject(RejectCountry, "country "+offer.Geography.Country+" is not in the allowed set")
	}
	if containsFold(request.Constraints.DeniedCountries, offer.Geography.Country) {
		return Candidate{}, reject(RejectCountry, "country "+offer.Geography.Country+" is denied")
	}
	if len(offer.Policy.AllowedCountries) > 0 && request.SourceGeography.Country != "" && !containsFold(offer.Policy.AllowedCountries, request.SourceGeography.Country) {
		return Candidate{}, reject(RejectResidency, "the machine owner does not accept state originating in "+request.SourceGeography.Country)
	}
	if offer.Policy.MaxStateBytes > 0 && request.StateBytes > offer.Policy.MaxStateBytes {
		return Candidate{}, reject(RejectStateSize, fmt.Sprintf("the offer accepts at most %d state bytes and the workload needs %d", offer.Policy.MaxStateBytes, request.StateBytes))
	}
	if offer.Policy.MaxDurationHours > 0 && request.Duration > time.Duration(offer.Policy.MaxDurationHours)*time.Hour {
		return Candidate{}, reject(RejectDuration, fmt.Sprintf("the offer accepts at most %d hours", offer.Policy.MaxDurationHours))
	}
	if offer.Policy.RequireEncryptedState && !request.Constraints.StateIsEncrypted {
		return Candidate{}, reject(RejectEncryption, "the machine owner requires encrypted state and the request did not declare it")
	}
	if request.Constraints.SharesGPU && !offer.Policy.GPUSharingAllowed {
		return Candidate{}, reject(RejectGPUSharing, "the machine owner does not allow shared GPU workloads")
	}
	available := offer.Available()
	if request.Requirements.CPUCount > 0 && available.CPUCount < request.Requirements.CPUCount {
		return Candidate{}, reject(RejectCPU, fmt.Sprintf("%.2f CPUs available, %.2f required", available.CPUCount, request.Requirements.CPUCount))
	}
	if request.Requirements.MemoryBytes > 0 && available.MemoryBytes < request.Requirements.MemoryBytes {
		return Candidate{}, reject(RejectMemory, fmt.Sprintf("%d memory bytes available, %d required", available.MemoryBytes, request.Requirements.MemoryBytes))
	}
	if request.Requirements.StorageBytes > 0 && available.StorageBytes < request.Requirements.StorageBytes {
		return Candidate{}, reject(RejectStorage, fmt.Sprintf("%d storage bytes available, %d required", available.StorageBytes, request.Requirements.StorageBytes))
	}
	if request.Requirements.NetworkMbps > 0 && available.NetworkMbps < request.Requirements.NetworkMbps {
		return Candidate{}, reject(RejectNetwork, fmt.Sprintf("%.2f Mbps available, %.2f required", available.NetworkMbps, request.Requirements.NetworkMbps))
	}
	pool := available.GPUs
	for _, required := range request.Requirements.GPUs {
		match := compatibility.MatchGPU(required, pool)
		if match == nil {
			return Candidate{}, reject(RejectGPU, "no available GPU satisfies "+strings.TrimSpace(required.Vendor+" "+required.Model))
		}
		if required.CheckpointRestore && !match.CheckpointRestore {
			return Candidate{}, reject(RejectGPU, "the available "+match.Vendor+" GPU does not advertise checkpoint/restore")
		}
		pool = remainingGPUs(pool, []model.GPUDevice{*match})
	}
	for _, feature := range request.Constraints.RequiredFeatures {
		if !offer.Capabilities.Features[feature] {
			return Candidate{}, reject(RejectFeature, "the machine does not report feature "+feature)
		}
	}
	currency := offer.Pricing.Currency
	hourly := offer.Pricing.HourlyRate(request.Requirements)
	if request.Constraints.Currency != "" && hourly > 0 && currency != request.Constraints.Currency {
		return Candidate{}, reject(RejectCurrency, "the offer is priced in "+currency+" and the request requires "+request.Constraints.Currency)
	}
	if request.Constraints.MaxPriceMicrosPerHour > 0 && hourly > request.Constraints.MaxPriceMicrosPerHour {
		return Candidate{}, reject(RejectPrice, fmt.Sprintf("%d micros per hour exceeds the %d micro limit", hourly, request.Constraints.MaxPriceMicrosPerHour))
	}
	latency, latencyKnown := offer.LatencyTo(request.SourceMachineID, request.SourceGeography)
	if request.Constraints.MaxLatencyMillis > 0 {
		if !latencyKnown {
			return Candidate{}, reject(RejectLatencyUnknown, "no measured round-trip time exists for this path, so a latency limit cannot be honored")
		}
		if latency > request.Constraints.MaxLatencyMillis {
			return Candidate{}, reject(RejectLatency, fmt.Sprintf("%.1f ms exceeds the %.1f ms limit", latency, request.Constraints.MaxLatencyMillis))
		}
	}
	candidate := Candidate{
		OfferID:          offer.ID,
		MachineID:        offer.MachineID,
		MachineName:      offer.MachineName,
		OrganizationID:   offer.OrganizationID,
		AgentURL:         offer.AgentURL,
		Geography:        offer.Geography,
		Trust:            trust,
		SameOrganization: sameOrganization,
		Available:        available,
		HourlyMicros:     hourly,
		EstimatedMicros:  offer.Pricing.Cost(request.Requirements, request.Duration, request.StateBytes),
		Currency:         currency,
		LatencyMillis:    latency,
		LatencyKnown:     latencyKnown,
	}
	if request.Manifest != nil {
		report := scheduler.checker.Check(*request.Manifest, offer.Capabilities)
		candidate.Compatibility = &report
		if !report.Compatible {
			return Candidate{}, reject(RejectCompatibility, firstErrorIssue(report))
		}
		for _, issue := range report.Issues {
			if issue.Severity != "error" {
				candidate.Warnings = append(candidate.Warnings, issue.Code+": "+issue.Description)
			}
		}
	}
	if !latencyKnown {
		candidate.Warnings = append(candidate.Warnings, "LATENCY_UNMEASURED: no round-trip measurement exists for this path")
	}
	return candidate, nil
}

func firstErrorIssue(report model.CompatibilityReport) string {
	for _, issue := range report.Issues {
		if issue.Severity == "error" {
			return issue.Code + ": " + issue.Description
		}
	}
	return "the destination cannot restore this state"
}

// capabilityScore rewards headroom. A destination that exactly fits scores 0
// and one with at least twice the required capacity scores 1, because a
// restored workload that immediately exhausts its host is a bad placement even
// when it technically fits.
func capabilityScore(required, available Resources) float64 {
	ratios := make([]float64, 0, 4)
	if required.CPUCount > 0 {
		ratios = append(ratios, available.CPUCount/required.CPUCount)
	}
	if required.MemoryBytes > 0 {
		ratios = append(ratios, float64(available.MemoryBytes)/float64(required.MemoryBytes))
	}
	if required.StorageBytes > 0 {
		ratios = append(ratios, float64(available.StorageBytes)/float64(required.StorageBytes))
	}
	if required.NetworkMbps > 0 {
		ratios = append(ratios, available.NetworkMbps/required.NetworkMbps)
	}
	if len(ratios) == 0 {
		return 1
	}
	total := 0.0
	for _, ratio := range ratios {
		total += clamp(ratio-1, 0, 1)
	}
	return total / float64(len(ratios))
}

// invertedScore maps a lower-is-better metric onto [0,1] relative to the range
// observed in this scheduling pass. When every candidate is identical the
// dimension cannot discriminate, so all of them score 1.
func invertedScore(value, minimum, maximum float64) float64 {
	if maximum <= minimum {
		return 1
	}
	return clamp(1-(value-minimum)/(maximum-minimum), 0, 1)
}

func localityScore(source, destination Geography) float64 {
	source = source.normalize()
	if source.Region == "" && source.Country == "" {
		return 0.5
	}
	switch {
	case source.Zone != "" && source.Zone == destination.Zone:
		return 1
	case source.Region != "" && source.Region == destination.Region:
		return 0.8
	case source.Country != "" && source.Country == destination.Country:
		return 0.6
	default:
		return 0.2
	}
}

func freshnessScore(lastSeen *time.Time, now time.Time, staleAfter time.Duration) float64 {
	if lastSeen == nil || staleAfter <= 0 {
		return 0
	}
	age := now.Sub(*lastSeen)
	if age <= staleAfter/4 {
		return 1
	}
	if age >= staleAfter {
		return 0
	}
	return clamp(1-float64(age-staleAfter/4)/float64(staleAfter-staleAfter/4), 0, 1)
}

func clamp(value, low, high float64) float64 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
