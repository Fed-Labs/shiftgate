package scheduler

import (
	"testing"
	"time"

	"shift.dev/shift/internal/model"
)

var testNow = time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)

func fixedScheduler(tradingEnabled bool) *Scheduler {
	configuration := DefaultConfig()
	configuration.PublicTradingEnabled = tradingEnabled
	configuration.Now = func() time.Time { return testNow }
	return New(configuration)
}

func seenAt(secondsAgo int) *time.Time {
	seen := testNow.Add(-time.Duration(secondsAgo) * time.Second)
	return &seen
}

func baseOffer(id, organizationID, machineID string) Offer {
	return Offer{
		ID:             id,
		OrganizationID: organizationID,
		MachineID:      machineID,
		MachineName:    machineID,
		AgentURL:       "https://" + machineID + ".example:8443",
		Exposed: Resources{
			CPUCount:     8,
			MemoryBytes:  32 << 30,
			StorageBytes: 500 << 30,
			NetworkMbps:  1000,
		},
		Geography: Geography{Region: "eu-central", Country: "DE", Zone: "eu-central-a"},
		Policy: Policy{
			Visibility: VisibilityOrganization,
		},
		Availability: Availability{
			Status:     OfferAvailable,
			LastSeenAt: seenAt(10),
		},
		Trust: TrustOrganization,
		Capabilities: model.MachineCapabilities{
			MachineID:    machineID,
			Hostname:     machineID,
			OS:           "linux",
			Distribution: "debian",
			Kernel:       "6.12.38",
			Architecture: "amd64",
			CPUs:         8,
			MemoryBytes:  32 << 30,
			Storage: []model.StorageDevice{{
				Mountpoint:     "/",
				Filesystem:     "ext4",
				TotalBytes:     500 << 30,
				AvailableBytes: 400 << 30,
			}},
			CRIU:     model.CRIUCapabilities{Installed: true, Healthy: true, Version: "4.2"},
			Features: map[string]bool{"gnu_tar": true},
		},
	}
}

func baseRequest(organizationID string) Request {
	return Request{
		WorkloadID:            "workload-1",
		RequesterOrganization: organizationID,
		SourceMachineID:       "laptop",
		SourceGeography:       Geography{Region: "eu-central", Country: "DE", Zone: "eu-central-b"},
		// The requirements are deliberately an exact fit for a small offer, so the
		// scoring tests can tell real headroom apart from a machine that only
		// just holds the workload.
		Requirements: Resources{
			CPUCount:     2,
			MemoryBytes:  4 << 30,
			StorageBytes: 20 << 30,
		},
		StateBytes: 2 << 30,
		Duration:   2 * time.Hour,
	}
}

func rejectionFor(t *testing.T, placement Placement, offerID string) Rejection {
	t.Helper()
	for _, rejection := range placement.Rejected {
		if rejection.OfferID == offerID {
			return rejection
		}
	}
	t.Fatalf("no rejection for %s in %+v", offerID, placement.Rejected)
	return Rejection{}
}

func TestPublicTradingRequiresServerGate(t *testing.T) {
	offer := baseOffer("public-offer", "provider", "provider-box")
	offer.Policy.Visibility = VisibilityPublic
	request := baseRequest("consumer")
	request.Constraints.IncludeOtherOrganizations = true

	closed, err := fixedScheduler(false).Place(request, []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, closed, offer.ID); rejection.Code != RejectTradingDisabled {
		t.Fatalf("expected %s, got %s", RejectTradingDisabled, rejection.Code)
	}

	open, err := fixedScheduler(true).Place(request, []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := open.Best(); !ok {
		t.Fatalf("enabled public trading should admit the offer, rejected=%+v", open.Rejected)
	}
}

func TestCrossOrganizationPlacementRequiresExplicitOptIn(t *testing.T) {
	offer := baseOffer("partner-offer", "provider", "provider-box")
	offer.Policy.AllowedOrganizations = []string{"consumer"}
	request := baseRequest("consumer")

	placement, err := fixedScheduler(false).Place(request, []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	if rejection := rejectionFor(t, placement, offer.ID); rejection.Code != RejectCrossOrganization {
		t.Fatalf("expected %s, got %s", RejectCrossOrganization, rejection.Code)
	}
}

func TestPartnerVisibilityWorksWhilePublicTradingIsDisabled(t *testing.T) {
	offer := baseOffer("partner-offer", "provider", "provider-box")
	offer.Policy.AllowedOrganizations = []string{"consumer"}
	request := baseRequest("consumer")
	request.Constraints.IncludeOtherOrganizations = true

	placement, err := fixedScheduler(false).Place(request, []Offer{offer})
	if err != nil {
		t.Fatal(err)
	}
	candidate, ok := placement.Best()
	if !ok {
		t.Fatalf("an explicitly shared partner offer should remain usable, rejected=%+v", placement.Rejected)
	}
	if candidate.SameOrganization {
		t.Fatal("a partner offer must be marked as cross-organization")
	}
}
