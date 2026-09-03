package scheduler

import (
	"testing"
	"time"

	"shift.dev/shift/internal/model"
)

func TestInsufficientResourcesAreRejectedWithTheLimitingClass(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Offer)
		expected string
	}{
		{"cpu", func(offer *Offer) { offer.Exposed.CPUCount = 1 }, RejectCPU},
		{"memory", func(offer *Offer) { offer.Exposed.MemoryBytes = 1 << 30 }, RejectMemory},
		{"storage", func(offer *Offer) { offer.Exposed.StorageBytes = 1 << 30 }, RejectStorage},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			offer := baseOffer("offer-1", "org-1", "workstation")
			testCase.mutate(&offer)
			placement, err := fixedScheduler(false).Place(baseRequest("org-1"), []Offer{offer})
			if err != nil {
				t.Fatal(err)
			}
			if rejection := rejectionFor(t, placement, "offer-1"); rejection.Code != testCase.expected {
				t.Fatalf("expected %s, got %s (%s)", testCase.expected, rejection.Code, rejection.Reason)
			}
		})
	}
}

func TestReservedCapacityIsSubtractedFromAnOffer(t *testing.T) {
	offer := baseOffer("offer-1", "org-1", "workstation")
	offer.Committed = Resources{CPUCount: 7, MemoryBytes: 30 << 30}

	placement, err := fixedScheduler(false).Place(baseRequest("org-1"), []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-1"); rejection.Code != RejectCPU {
		t.Fatalf("expected committed capacity to exhaust the offer, got %s", rejection.Code)
	}

	offer.Committed = Resources{CPUCount: 2}
	placement, err = fixedScheduler(false).Place(baseRequest("org-1"), []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	candidate, ok := placement.Best()
	if !ok {
		t.Fatalf("expected a candidate, rejected=%+v", placement.Rejected)
	}
	if candidate.Available.CPUCount != 6 {
		t.Fatalf("expected 6 available CPUs after a 2 CPU reservation, got %.2f", candidate.Available.CPUCount)
	}
}

func TestLiveReservationsProduceCommittedResources(t *testing.T) {
	expiry := testNow.Add(30 * time.Minute)
	lapsed := testNow.Add(-time.Minute)
	reservations := []Reservation{
		{ID: "r1", State: ReservationActive, Requested: Resources{CPUCount: 2, MemoryBytes: 4 << 30}, ExpiresAt: expiry},
		{ID: "r2", State: ReservationPending, Requested: Resources{CPUCount: 1}, ExpiresAt: expiry},
		{ID: "r3", State: ReservationReleased, Requested: Resources{CPUCount: 4}, ExpiresAt: expiry},
		{ID: "r4", State: ReservationActive, Requested: Resources{CPUCount: 8}, ExpiresAt: lapsed},
	}
	committed := CommittedFrom(reservations, testNow)
	if committed.CPUCount != 3 {
		t.Fatalf("expected released and lapsed reservations to be ignored, got %.2f CPUs", committed.CPUCount)
	}
	if committed.MemoryBytes != 4<<30 {
		t.Fatalf("unexpected committed memory %d", committed.MemoryBytes)
	}
}

func TestReservationTransitionsAreValidated(t *testing.T) {
	if !CanTransitionReservation(ReservationPending, ReservationActive) {
		t.Fatal("pending must be able to become active")
	}
	if CanTransitionReservation(ReservationReleased, ReservationActive) {
		t.Fatal("a released reservation must never take capacity again")
	}
	if CanTransitionReservation(ReservationExpired, ReservationActive) {
		t.Fatal("an expired reservation must never take capacity again")
	}
	if !CanTransitionReservation(ReservationFailed, ReservationReleased) {
		t.Fatal("a failed reservation must be releasable so capacity is reclaimed")
	}
}

func TestStalePresenceIsNeverACandidate(t *testing.T) {
	offer := baseOffer("offer-1", "org-1", "workstation")
	offer.Availability.LastSeenAt = seenAt(600)

	placement, err := fixedScheduler(false).Place(baseRequest("org-1"), []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-1"); rejection.Code != RejectStale {
		t.Fatalf("expected %s, got %s", RejectStale, rejection.Code)
	}

	offer.Availability.LastSeenAt = nil
	placement, err = fixedScheduler(false).Place(baseRequest("org-1"), []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-1"); rejection.Code != RejectStale {
		t.Fatalf("a machine that never reported must be rejected, got %s", rejection.Code)
	}
}

func TestAvailabilityWindowIsEnforced(t *testing.T) {
	future := testNow.Add(time.Hour)
	past := testNow.Add(-time.Hour)

	opening := baseOffer("offer-future", "org-1", "future-box")
	opening.Availability.WindowFrom = &future
	closed := baseOffer("offer-past", "org-1", "past-box")
	closed.Availability.WindowTo = &past

	placement, err := fixedScheduler(false).Place(baseRequest("org-1"), []Offer{opening, closed})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-future"); rejection.Code != RejectWindow {
		t.Fatalf("expected %s, got %s", RejectWindow, rejection.Code)
	}
	if rejection := rejectionFor(t, placement, "offer-past"); rejection.Code != RejectWindow {
		t.Fatalf("expected %s, got %s", RejectWindow, rejection.Code)
	}
}

func TestSourceMachineIsNeverItsOwnDestination(t *testing.T) {
	offer := baseOffer("offer-1", "org-1", "laptop")
	placement, err := fixedScheduler(false).Place(baseRequest("org-1"), []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-1"); rejection.Code != RejectSelfPlacement {
		t.Fatalf("expected %s, got %s", RejectSelfPlacement, rejection.Code)
	}
}

func TestUnmeasuredLatencyIsRejectedRatherThanEstimated(t *testing.T) {
	offer := baseOffer("offer-1", "org-1", "workstation")
	request := baseRequest("org-1")
	request.Constraints.MaxLatencyMillis = 20

	placement, err := fixedScheduler(false).Place(request, []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-1"); rejection.Code != RejectLatencyUnknown {
		t.Fatalf("expected %s, got %s", RejectLatencyUnknown, rejection.Code)
	}

	offer.Latency = []LatencySample{{Observer: "laptop", RoundTripMs: 42, MeasuredAt: testNow}}
	placement, err = fixedScheduler(false).Place(request, []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-1"); rejection.Code != RejectLatency {
		t.Fatalf("expected %s, got %s", RejectLatency, rejection.Code)
	}

	offer.Latency = []LatencySample{{Observer: "laptop", RoundTripMs: 8, MeasuredAt: testNow}}
	placement, err = fixedScheduler(false).Place(request, []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	candidate, ok := placement.Best()
	if !ok {
		t.Fatalf("expected a candidate, rejected=%+v", placement.Rejected)
	}
	if !candidate.LatencyKnown || candidate.LatencyMillis != 8 {
		t.Fatalf("expected the measured 8 ms path, got known=%v value=%.1f", candidate.LatencyKnown, candidate.LatencyMillis)
	}
}

func TestLatencyFallsBackFromMachineToRegionToCountry(t *testing.T) {
	offer := baseOffer("offer-1", "org-1", "workstation")
	offer.Latency = []LatencySample{
		{Observer: "country:DE", RoundTripMs: 30, MeasuredAt: testNow},
		{Observer: "region:eu-central", RoundTripMs: 12, MeasuredAt: testNow},
		{Observer: "laptop", RoundTripMs: 4, MeasuredAt: testNow},
	}
	source := Geography{Region: "eu-central", Country: "DE"}

	if value, ok := offer.LatencyTo("laptop", source); !ok || value != 4 {
		t.Fatalf("machine measurement must win, got %v %.1f", ok, value)
	}
	if value, ok := offer.LatencyTo("unknown-machine", source); !ok || value != 12 {
		t.Fatalf("region measurement must be next, got %v %.1f", ok, value)
	}
	if value, ok := offer.LatencyTo("unknown-machine", Geography{Country: "DE"}); !ok || value != 30 {
		t.Fatalf("country measurement must be last, got %v %.1f", ok, value)
	}
	if _, ok := offer.LatencyTo("unknown-machine", Geography{Country: "FR"}); ok {
		t.Fatal("an unmeasured path must report no measurement")
	}
}

func TestPriceConstraintAndOrderingUseIntegerMicros(t *testing.T) {
	cheap := baseOffer("offer-cheap", "org-1", "cheap-box")
	cheap.Pricing = Pricing{Currency: "EUR", CPUHourMicros: 10_000, MemoryGiBHourMicros: 1_000}
	expensive := baseOffer("offer-expensive", "org-1", "expensive-box")
	expensive.Pricing = Pricing{Currency: "EUR", CPUHourMicros: 100_000, MemoryGiBHourMicros: 10_000}

	request := baseRequest("org-1")
	request.Weights = &Weights{Price: 1}

	placement, err := fixedScheduler(false).Place(request, []Offer{expensive, cheap})
	if err != nil {
		t.Fatal(err)
	}
	if len(placement.Candidates) != 2 {
		t.Fatalf("expected both offers, rejected=%+v", placement.Rejected)
	}
	if placement.Candidates[0].OfferID != "offer-cheap" {
		t.Fatalf("price weighting must rank the cheaper offer first, got %s", placement.Candidates[0].OfferID)
	}
	// 2 CPUs at 10_000 plus 4 GiB at 1_000 is 24_000 micros per hour.
	if placement.Candidates[0].HourlyMicros != 24_000 {
		t.Fatalf("unexpected hourly rate %d", placement.Candidates[0].HourlyMicros)
	}
	if placement.Candidates[0].EstimatedMicros != 48_000 {
		t.Fatalf("two hours of the cheap offer must cost 48000 micros, got %d", placement.Candidates[0].EstimatedMicros)
	}

	request.Constraints.MaxPriceMicrosPerHour = 30_000
	placement, err = fixedScheduler(false).Place(request, []Offer{expensive, cheap})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-expensive"); rejection.Code != RejectPrice {
		t.Fatalf("expected %s, got %s", RejectPrice, rejection.Code)
	}
	if len(placement.Candidates) != 1 || placement.Candidates[0].OfferID != "offer-cheap" {
		t.Fatalf("expected only the cheap offer, got %+v", placement.Candidates)
	}
}

func TestBillingIncrementRoundsPartialHoursUp(t *testing.T) {
	pricing := Pricing{Currency: "USD", CPUHourMicros: 1_000_000, BillingIncrementHours: 1}
	cost := pricing.Cost(Resources{CPUCount: 1}, 90*time.Minute, 0)
	if cost != 2_000_000 {
		t.Fatalf("90 minutes billed hourly must charge two hours, got %d", cost)
	}
	minimum := Pricing{Currency: "USD", CPUHourMicros: 1, MinimumChargeMicros: 500_000}
	if charge := minimum.Cost(Resources{CPUCount: 1}, time.Minute, 0); charge != 500_000 {
		t.Fatalf("the minimum charge must apply, got %d", charge)
	}
	egress := Pricing{Currency: "USD", EgressGiBMicros: 20_000}
	if charge := egress.Cost(Resources{}, 0, 10<<30); charge != 200_000 {
		t.Fatalf("ten GiB of egress must charge 200000 micros, got %d", charge)
	}
}

func TestGPURequirementsMatchVendorMemoryAndRestoreSupport(t *testing.T) {
	offer := baseOffer("offer-1", "org-1", "gpu-box")
	offer.Exposed.GPUs = []model.GPUDevice{{Vendor: "NVIDIA", Model: "RTX 4090", MemoryBytes: 24 << 30, CheckpointRestore: true}}

	request := baseRequest("org-1")
	request.Requirements.GPUs = []model.GPUDevice{{Vendor: "NVIDIA", MemoryBytes: 16 << 30, CheckpointRestore: true}}
	placement, err := fixedScheduler(false).Place(request, []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := placement.Best(); !ok {
		t.Fatalf("a matching GPU must be a candidate, rejected=%+v", placement.Rejected)
	}

	request.Requirements.GPUs = []model.GPUDevice{{Vendor: "AMD", MemoryBytes: 8 << 30}}
	placement, err = fixedScheduler(false).Place(request, []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-1"); rejection.Code != RejectGPU {
		t.Fatalf("expected %s for a foreign vendor, got %s", RejectGPU, rejection.Code)
	}

	offer.Exposed.GPUs[0].CheckpointRestore = false
	request.Requirements.GPUs = []model.GPUDevice{{Vendor: "NVIDIA", CheckpointRestore: true}}
	placement, err = fixedScheduler(false).Place(request, []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-1"); rejection.Code != RejectGPU {
		t.Fatalf("expected %s when the driver cannot checkpoint, got %s", RejectGPU, rejection.Code)
	}
}

func TestTwoGPURequestsCannotShareOneDevice(t *testing.T) {
	offer := baseOffer("offer-1", "org-1", "gpu-box")
	offer.Exposed.GPUs = []model.GPUDevice{{Vendor: "NVIDIA", Model: "L40S", MemoryBytes: 48 << 30}}

	request := baseRequest("org-1")
	request.Requirements.GPUs = []model.GPUDevice{
		{Vendor: "NVIDIA", MemoryBytes: 16 << 30},
		{Vendor: "NVIDIA", MemoryBytes: 16 << 30},
	}
	placement, err := fixedScheduler(false).Place(request, []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-1"); rejection.Code != RejectGPU {
		t.Fatalf("one device cannot satisfy two GPU requirements, got %s", rejection.Code)
	}
}

func TestGeographyAndResidencyConstraints(t *testing.T) {
	german := baseOffer("offer-de", "org-1", "de-box")
	american := baseOffer("offer-us", "org-1", "us-box")
	american.Geography = Geography{Region: "us-east", Country: "US", Zone: "us-east-a"}

	request := baseRequest("org-1")
	request.Constraints.AllowedCountries = []string{"de"}
	placement, err := fixedScheduler(false).Place(request, []Offer{german, american})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-us"); rejection.Code != RejectCountry {
		t.Fatalf("expected %s, got %s", RejectCountry, rejection.Code)
	}
	if len(placement.Candidates) != 1 || placement.Candidates[0].OfferID != "offer-de" {
		t.Fatalf("expected the German offer only, got %+v", placement.Candidates)
	}

	restricted := baseOffer("offer-restricted", "org-1", "restricted-box")
	restricted.Policy.AllowedCountries = []string{"CH"}
	placement, err = fixedScheduler(false).Place(baseRequest("org-1"), []Offer{restricted})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-restricted"); rejection.Code != RejectResidency {
		t.Fatalf("expected %s, got %s", RejectResidency, rejection.Code)
	}
}

func TestOwnerPolicyLimitsStateSizeDurationAndEncryption(t *testing.T) {
	small := baseOffer("offer-small", "org-1", "small-box")
	small.Policy.MaxStateBytes = 1 << 30
	short := baseOffer("offer-short", "org-1", "short-box")
	short.Policy.MaxDurationHours = 1
	encrypted := baseOffer("offer-encrypted", "org-1", "encrypted-box")
	encrypted.Policy.RequireEncryptedState = true

	placement, err := fixedScheduler(false).Place(baseRequest("org-1"), []Offer{small, short, encrypted})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-small"); rejection.Code != RejectStateSize {
		t.Fatalf("expected %s, got %s", RejectStateSize, rejection.Code)
	}
	if rejection := rejectionFor(t, placement, "offer-short"); rejection.Code != RejectDuration {
		t.Fatalf("expected %s, got %s", RejectDuration, rejection.Code)
	}
	if rejection := rejectionFor(t, placement, "offer-encrypted"); rejection.Code != RejectEncryption {
		t.Fatalf("expected %s, got %s", RejectEncryption, rejection.Code)
	}

	request := baseRequest("org-1")
	request.Constraints.StateIsEncrypted = true
	placement, err = fixedScheduler(false).Place(request, []Offer{encrypted})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := placement.Best(); !ok {
		t.Fatalf("declaring encrypted state must satisfy the policy, rejected=%+v", placement.Rejected)
	}
}

func TestRequiredFeaturesAndMinimumTrustAreEnforced(t *testing.T) {
	offer := baseOffer("offer-1", "org-1", "workstation")
	request := baseRequest("org-1")
	request.Constraints.RequiredFeatures = []string{"cpu.avx512f"}

	placement, err := fixedScheduler(false).Place(request, []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-1"); rejection.Code != RejectFeature {
		t.Fatalf("expected %s, got %s", RejectFeature, rejection.Code)
	}

	untrusted := baseOffer("offer-untrusted", "org-1", "untrusted-box")
	untrusted.Trust = TrustUnverified
	trustRequest := baseRequest("org-1")
	trustRequest.Constraints.MinTrust = TrustOrganization
	placement, err = fixedScheduler(false).Place(trustRequest, []Offer{untrusted})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-untrusted"); rejection.Code != RejectTrust {
		t.Fatalf("expected %s, got %s", RejectTrust, rejection.Code)
	}
}

func TestClaimedTrustIsClampedToAvailableEvidence(t *testing.T) {
	if tier := ClampTrust(TrustVerified, false, true); tier != TrustCommunity {
		t.Fatalf("a foreign organization cannot exceed community trust, got %s", tier)
	}
	if tier := ClampTrust(TrustVerified, true, false); tier != TrustOrganization {
		t.Fatalf("an unverified identity cannot reach verified trust, got %s", tier)
	}
	if tier := ClampTrust(TrustVerified, true, true); tier != TrustVerified {
		t.Fatalf("a verified same-organization machine must keep verified trust, got %s", tier)
	}
	if tier := ClampTrust("nonsense", true, true); tier != TrustUnverified {
		t.Fatalf("an unknown tier must fall back to unverified, got %s", tier)
	}
}

func TestRestoreCompatibilityIsCheckedWhenAManifestIsSupplied(t *testing.T) {
	offer := baseOffer("offer-1", "org-1", "workstation")
	offer.Capabilities.Kernel = "6.12.38"
	offer.Capabilities.Distribution = "debian"
	offer.Capabilities.CRIU = model.CRIUCapabilities{Installed: true, Healthy: true, Version: "4.2"}
	offer.Capabilities.Storage = []model.StorageDevice{{Mountpoint: "/", Filesystem: "ext4", TotalBytes: 512 << 30, AvailableBytes: 400 << 30}}

	incompatible := baseOffer("offer-arm", "org-1", "arm-box")
	incompatible.Capabilities = offer.Capabilities
	incompatible.Capabilities.MachineID = "arm-box"
	incompatible.Capabilities.Architecture = "arm64"

	manifest := model.CheckpointManifest{
		Format:        model.StateFormatName,
		FormatVersion: model.StateFormatVersion,
		Workload:      model.WorkloadSpec{ID: "workload-1", RootPath: "/srv/app", Resources: model.ResourceRequirements{MemoryBytes: 4 << 30, StorageBytes: 20 << 30}},
		SourceMachine: model.MachineCapabilities{
			OS:           "linux",
			Architecture: "amd64",
			Kernel:       "6.12.38",
			Distribution: "debian",
			Features:     map[string]bool{"gnu_tar": true},
		},
	}
	request := baseRequest("org-1")
	request.Manifest = &manifest

	placement, err := fixedScheduler(false).Place(request, []Offer{offer, incompatible})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, "offer-arm"); rejection.Code != RejectCompatibility {
		t.Fatalf("expected %s, got %s (%s)", RejectCompatibility, rejection.Code, rejection.Reason)
	}
	candidate, ok := placement.Best()
	if !ok {
		t.Fatalf("the matching machine must remain a candidate, rejected=%+v", placement.Rejected)
	}
	if candidate.Compatibility == nil || !candidate.Compatibility.Compatible {
		t.Fatal("a candidate checked against a manifest must carry its compatibility report")
	}
}

func TestLocalityAndCapabilityScoresRankCloserRoomierDestinations(t *testing.T) {
	near := baseOffer("offer-near", "org-1", "near-box")
	far := baseOffer("offer-far", "org-1", "far-box")
	far.Geography = Geography{Region: "us-east", Country: "US", Zone: "us-east-a"}

	request := baseRequest("org-1")
	request.Weights = &Weights{Locality: 1}
	placement, err := fixedScheduler(false).Place(request, []Offer{far, near})
	if err != nil {
		t.Fatal(err)
	}
	if placement.Candidates[0].OfferID != "offer-near" {
		t.Fatalf("locality weighting must prefer the same region, got %s", placement.Candidates[0].OfferID)
	}
	if placement.Candidates[0].Scores.Locality <= placement.Candidates[1].Scores.Locality {
		t.Fatal("the same-region candidate must score higher on locality")
	}

	roomy := baseOffer("offer-roomy", "org-1", "roomy-box")
	tight := baseOffer("offer-tight", "org-1", "tight-box")
	tight.Exposed = Resources{CPUCount: 2, MemoryBytes: 4 << 30, StorageBytes: 20 << 30}
	tight.Capabilities.CPUs = 2
	tight.Capabilities.MemoryBytes = 4 << 30

	request = baseRequest("org-1")
	request.Weights = &Weights{Capability: 1}
	placement, err = fixedScheduler(false).Place(request, []Offer{tight, roomy})
	if err != nil {
		t.Fatal(err)
	}
	if placement.Candidates[0].OfferID != "offer-roomy" {
		t.Fatalf("capability weighting must prefer headroom, got %s", placement.Candidates[0].OfferID)
	}
	if placement.Candidates[1].Scores.Capability != 0 {
		t.Fatalf("an exact fit must score zero headroom, got %.3f", placement.Candidates[1].Scores.Capability)
	}
}

func TestPlacementIsDeterministicAndRespectsTheLimit(t *testing.T) {
	offers := []Offer{
		baseOffer("offer-c", "org-1", "box-c"),
		baseOffer("offer-a", "org-1", "box-a"),
		baseOffer("offer-b", "org-1", "box-b"),
	}
	request := baseRequest("org-1")
	request.Limit = 2

	first, err := fixedScheduler(false).Place(request, offers)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Candidates) != 2 {
		t.Fatalf("expected the limit to apply, got %d candidates", len(first.Candidates))
	}
	if first.Candidates[0].OfferID != "offer-a" || first.Candidates[1].OfferID != "offer-b" {
		t.Fatalf("identical offers must tie-break by id, got %s then %s", first.Candidates[0].OfferID, first.Candidates[1].OfferID)
	}
	second, err := fixedScheduler(false).Place(request, []Offer{offers[2], offers[0], offers[1]})
	if err != nil {
		t.Fatal(err)
	}
	if second.Candidates[0].OfferID != first.Candidates[0].OfferID || second.Candidates[1].OfferID != first.Candidates[1].OfferID {
		t.Fatal("placement must not depend on the order offers are supplied in")
	}
	if first.Evaluated != 3 {
		t.Fatalf("expected three evaluated offers, got %d", first.Evaluated)
	}
}

func TestPlacementRequiresARequestingOrganization(t *testing.T) {
	if _, err := fixedScheduler(false).Place(Request{}, nil); err == nil {
		t.Fatal("a placement request without an organization must be rejected")
	}
	request := baseRequest("org-1")
	request.Constraints.MinTrust = "platinum"
	if _, err := fixedScheduler(false).Place(request, nil); err == nil {
		t.Fatal("an unknown trust tier must be rejected")
	}
	request = baseRequest("org-1")
	request.Duration = -time.Hour
	if _, err := fixedScheduler(false).Place(request, nil); err == nil {
		t.Fatal("a negative duration must be rejected")
	}
}

func TestOfferValidationRejectsUnschedulableInventory(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Offer)
	}{
		{"no id", func(offer *Offer) { offer.ID = "" }},
		{"no organization", func(offer *Offer) { offer.OrganizationID = "" }},
		{"no machine", func(offer *Offer) { offer.MachineID = "" }},
		{"nothing exposed", func(offer *Offer) { offer.Exposed = Resources{} }},
		{"more cpu than the machine has", func(offer *Offer) { offer.Exposed.CPUCount = 64 }},
		{"more memory than the machine has", func(offer *Offer) { offer.Exposed.MemoryBytes = 1 << 50 }},
		{"priced without a currency", func(offer *Offer) { offer.Pricing = Pricing{CPUHourMicros: 1000} }},
		{"negative price", func(offer *Offer) { offer.Pricing = Pricing{Currency: "EUR", CPUHourMicros: -1} }},
		{"invalid country", func(offer *Offer) { offer.Geography.Country = "DEU" }},
		{"invalid status", func(offer *Offer) { offer.Availability.Status = "sleeping" }},
		{"inverted window", func(offer *Offer) {
			from := testNow
			to := testNow.Add(-time.Hour)
			offer.Availability.WindowFrom = &from
			offer.Availability.WindowTo = &to
		}},
		{"unnamed latency observer", func(offer *Offer) {
			offer.Latency = []LatencySample{{Observer: " ", RoundTripMs: 5}}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			offer := baseOffer("offer-1", "org-1", "workstation")
			testCase.mutate(&offer)
			if err := offer.Validate(); err == nil {
				t.Fatal("expected validation to fail")
			}
		})
	}
	valid := baseOffer("offer-1", "org-1", "workstation")
	if err := valid.Validate(); err != nil {
		t.Fatalf("the reference offer must validate: %v", err)
	}
}

func TestOfferNormalizationDefaultsAreSafe(t *testing.T) {
	offer := Offer{
		ID:             " offer-1 ",
		OrganizationID: " org-1 ",
		MachineID:      " workstation ",
		AgentURL:       "https://box.example:8443/",
		Exposed:        Resources{CPUCount: 1},
		Geography:      Geography{Region: " EU-Central ", Country: " de ", Zone: " EU-Central-A "},
	}
	offer.Normalize()
	if offer.ID != "offer-1" || offer.OrganizationID != "org-1" || offer.MachineID != "workstation" {
		t.Fatalf("identifiers must be trimmed, got %+v", offer)
	}
	if offer.AgentURL != "https://box.example:8443" {
		t.Fatalf("agent URL must lose its trailing slash, got %s", offer.AgentURL)
	}
	if offer.Geography.Region != "eu-central" || offer.Geography.Country != "DE" || offer.Geography.Zone != "eu-central-a" {
		t.Fatalf("geography must be canonicalized, got %+v", offer.Geography)
	}
	if offer.Policy.Visibility != VisibilityOrganization {
		t.Fatalf("an unspecified offer must default to organization visibility, got %s", offer.Policy.Visibility)
	}
	if offer.Availability.Status != OfferOffline {
		t.Fatalf("an unspecified offer must default to offline, got %s", offer.Availability.Status)
	}
	if offer.Trust != TrustUnverified {
		t.Fatalf("an unspecified offer must default to unverified trust, got %s", offer.Trust)
	}
}

func TestReservationValidationRequiresAnExpiry(t *testing.T) {
	reservation := Reservation{ID: "r1", OfferID: "offer-1", OrganizationID: "org-1"}
	if err := reservation.Validate(); err == nil {
		t.Fatal("a reservation without an expiry must be rejected")
	}
	reservation.ExpiresAt = testNow.Add(time.Hour)
	if err := reservation.Validate(); err != nil {
		t.Fatal(err)
	}
	if reservation.State != ReservationPending {
		t.Fatalf("a new reservation must default to pending, got %s", reservation.State)
	}
	if reservation.Expired(testNow) {
		t.Fatal("a future expiry must not read as expired")
	}
	if !reservation.Expired(testNow.Add(2 * time.Hour)) {
		t.Fatal("a lapsed expiry must read as expired")
	}
}

func TestExhaustedInventoryReturnsAnEmptyCandidateList(t *testing.T) {
	placement, err := fixedScheduler(false).Place(baseRequest("org-1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if placement.Candidates == nil {
		t.Fatal("an empty placement must return an empty list rather than nil")
	}
	if _, ok := placement.Best(); ok {
		t.Fatal("an empty placement has no best candidate")
	}
}
