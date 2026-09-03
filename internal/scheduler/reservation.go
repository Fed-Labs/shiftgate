package scheduler

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ReservationState is the lifecycle of a destination reservation. A reservation
// is what turns a scored candidate into committed capacity, so that two
// concurrent migrations cannot both plan for the same GPU.
type ReservationState string

const (
	ReservationPending  ReservationState = "PENDING"
	ReservationActive   ReservationState = "ACTIVE"
	ReservationReleased ReservationState = "RELEASED"
	ReservationExpired  ReservationState = "EXPIRED"
	ReservationFailed   ReservationState = "FAILED"
)

var reservationTransitions = map[ReservationState]map[ReservationState]bool{
	ReservationPending: {ReservationActive: true, ReservationReleased: true, ReservationExpired: true, ReservationFailed: true},
	ReservationActive:  {ReservationReleased: true, ReservationExpired: true, ReservationFailed: true},
	ReservationFailed:  {ReservationReleased: true},
}

// CanTransitionReservation reports whether a reservation state change is legal.
// Released and expired are terminal: capacity that was handed back is never
// silently taken again under the same reservation id.
func CanTransitionReservation(from, to ReservationState) bool {
	return reservationTransitions[from][to]
}

func (state ReservationState) Valid() bool {
	switch state {
	case ReservationPending, ReservationActive, ReservationReleased, ReservationExpired, ReservationFailed:
		return true
	default:
		return false
	}
}

// Terminal reports whether the reservation no longer holds capacity.
func (state ReservationState) Terminal() bool {
	return state == ReservationReleased || state == ReservationExpired
}

// Reservation is capacity held on one offer for one workload.
type Reservation struct {
	ID             string           `json:"id"`
	OfferID        string           `json:"offer_id"`
	OrganizationID string           `json:"organization_id"`
	MachineID      string           `json:"machine_id"`
	WorkloadID     string           `json:"workload_id,omitempty"`
	MigrationID    string           `json:"migration_id,omitempty"`
	Requested      Resources        `json:"requested"`
	State          ReservationState `json:"state"`
	HourlyMicros   int64            `json:"hourly_micros"`
	Currency       string           `json:"currency,omitempty"`
	ExpiresAt      time.Time        `json:"expires_at"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	ReleasedAt     *time.Time       `json:"released_at,omitempty"`
	Error          string           `json:"error,omitempty"`
}

// Validate checks a reservation before it is persisted.
func (reservation *Reservation) Validate() error {
	reservation.ID = strings.TrimSpace(reservation.ID)
	reservation.OfferID = strings.TrimSpace(reservation.OfferID)
	reservation.OrganizationID = strings.TrimSpace(reservation.OrganizationID)
	reservation.MachineID = strings.TrimSpace(reservation.MachineID)
	reservation.Currency = strings.ToUpper(strings.TrimSpace(reservation.Currency))
	if reservation.ID == "" || reservation.OfferID == "" || reservation.OrganizationID == "" {
		return errors.New("a reservation requires an id, an offer, and a requesting organization")
	}
	if reservation.State == "" {
		reservation.State = ReservationPending
	}
	if !reservation.State.Valid() {
		return fmt.Errorf("unsupported reservation state %q", reservation.State)
	}
	if reservation.Requested.CPUCount < 0 || reservation.Requested.NetworkMbps < 0 {
		return errors.New("reserved quantities cannot be negative")
	}
	if reservation.HourlyMicros < 0 {
		return errors.New("reservation price cannot be negative")
	}
	if reservation.ExpiresAt.IsZero() {
		return errors.New("a reservation requires an expiry so abandoned capacity is reclaimed")
	}
	return nil
}

// Expired reports whether the reservation's hold has lapsed.
func (reservation Reservation) Expired(now time.Time) bool {
	return !reservation.State.Terminal() && !now.Before(reservation.ExpiresAt)
}

// CommittedFrom sums the capacity that live reservations hold on one offer. It
// is what turns stored reservations into the Committed field the scheduler
// subtracts, and it ignores terminal and lapsed holds.
func CommittedFrom(reservations []Reservation, now time.Time) Resources {
	var committed Resources
	for _, reservation := range reservations {
		if reservation.State.Terminal() || reservation.Expired(now) {
			continue
		}
		committed.CPUCount += reservation.Requested.CPUCount
		committed.MemoryBytes += reservation.Requested.MemoryBytes
		committed.StorageBytes += reservation.Requested.StorageBytes
		committed.NetworkMbps += reservation.Requested.NetworkMbps
		committed.GPUs = append(committed.GPUs, reservation.Requested.GPUs...)
	}
	return committed
}
