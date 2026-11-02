package database

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// Regression cover for the per-seat oversubscription race (ship finding 03
// blocker 2): the ceiling check must serialize with the insert. Concurrent
// adds of different users used to read the same member count and all pass,
// committing more members than the organization purchased. PostgreSQL-gated.
func TestAddMemberParallelAddsRespectSeatCeiling(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "seat-race")

	const seats = 2
	if err := store.SetEntitlementSeats(ctx, organizationID, seats, "si_seat_race"); err != nil {
		t.Fatal(err)
	}

	// The registrant's owner row already occupies one seat; one seat is
	// left. Fire more concurrent adds than the ceiling allows and demand
	// that exactly one wins while the rest are refused.
	const contenders = 8
	emails := make([]string, contenders)
	for i := range emails {
		emails[i] = registerSeatUser(t, store, fmt.Sprintf("race-%d", i))
	}

	var wait sync.WaitGroup
	results := make([]error, contenders)
	for i := range emails {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index] = store.AddMember(ctx, organizationID, emails[index], "viewer", seats, testAudit(organizationID, "organization.member_upsert", emails[index]))
		}(i)
	}
	wait.Wait()

	admitted := 0
	for _, err := range results {
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrSeatCeilingExceeded):
		default:
			t.Fatalf("unexpected add-member error: %v", err)
		}
	}
	if admitted != 1 {
		t.Fatalf("%d concurrent adds passed a %d-seat ceiling with 1 seat free; the ceiling check is not serialized", admitted, seats)
	}
	members, err := store.CountMembers(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if members > seats {
		t.Fatalf("organization holds %d members on %d seats", members, seats)
	}
}

// The serialization must not deadlock or over-reject role changes on
// existing members under concurrency: a ceiling fully consumed by existing
// members still accepts parallel role changes (they consume no seats).
func TestAddMemberParallelRoleChangesNeverConsumeSeats(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "seat-race-roles")

	const seats = 2
	if err := store.SetEntitlementSeats(ctx, organizationID, seats, "si_seat_roles"); err != nil {
		t.Fatal(err)
	}
	alice := registerSeatUser(t, store, "role-alice")
	if err := store.AddMember(ctx, organizationID, alice, "viewer", seats, testAudit(organizationID, "organization.member_upsert", alice)); err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	results := make([]error, 4)
	for i := range results {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			role := []string{"viewer", "admin", "operator", "viewer"}[index]
			results[index] = store.AddMember(ctx, organizationID, alice, role, seats, testAudit(organizationID, "organization.member_upsert", alice))
		}(i)
	}
	wait.Wait()
	for _, err := range results {
		if err != nil {
			t.Fatalf("parallel role change rejected: %v", err)
		}
	}
}
