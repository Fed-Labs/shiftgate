package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/scheduler"
)

// maxComputeReservationTTL bounds how long one reservation may pin capacity.
// Without a ceiling a single caller could hold a machine indefinitely by asking
// for an absurd expiry.
const maxComputeReservationTTL = 24 * time.Hour

// PublishComputeOfferRequest exposes one registered machine's resources to the
// scheduler. Machine name, agent URL, hardware capabilities, and presence are
// taken from the control plane's own machine registry rather than from this
// body, so an operator cannot advertise hardware a machine does not have. Trust
// is a claim: the scheduler clamps it to the evidence the control plane can
// actually produce.
type PublishComputeOfferRequest struct {
	MachineID  string                    `json:"machine_id"`
	Exposed    scheduler.Resources       `json:"exposed"`
	Pricing    scheduler.Pricing         `json:"pricing"`
	Geography  scheduler.Geography       `json:"geography"`
	Policy     scheduler.Policy          `json:"policy"`
	Trust      scheduler.TrustTier       `json:"trust,omitempty"`
	Status     scheduler.OfferStatus     `json:"status,omitempty"`
	WindowFrom *time.Time                `json:"window_from,omitempty"`
	WindowTo   *time.Time                `json:"window_to,omitempty"`
	Latency    []scheduler.LatencySample `json:"latency,omitempty"`
}

// ComputeOffer is a stored offer as the API returns it. Available is the capacity
// left after live reservations, which is the figure a client should reason about
// rather than the raw exposure.
type ComputeOffer struct {
	scheduler.Offer
	Available   scheduler.Resources `json:"available"`
	WithdrawnAt *time.Time          `json:"withdrawn_at,omitempty"`
}

// ComputeInventory is the set of offers one organization may schedule against,
// together with the state of the public-trading gate so a client never has to
// guess why another organization's machines are absent.
type ComputeInventory struct {
	TradingEnabled bool           `json:"trading_enabled"`
	Offers         []ComputeOffer `json:"offers"`
}

// PlacementRequest asks the scheduler where a workload could go. CheckpointID is
// resolved server-side into the stored manifest, so restore compatibility is
// checked against state that was actually captured rather than client claims.
type PlacementRequest struct {
	WorkloadID      string                `json:"workload_id,omitempty"`
	CheckpointID    string                `json:"checkpoint_id,omitempty"`
	SourceMachineID string                `json:"source_machine_id,omitempty"`
	SourceGeography scheduler.Geography   `json:"source_geography,omitempty"`
	Requirements    scheduler.Resources   `json:"requirements"`
	StateBytes      uint64                `json:"state_bytes,omitempty"`
	DurationSeconds int64                 `json:"duration_seconds,omitempty"`
	Constraints     scheduler.Constraints `json:"constraints,omitempty"`
	Weights         *scheduler.Weights    `json:"weights,omitempty"`
	Limit           int                   `json:"limit,omitempty"`
}

// CreateComputeReservationRequest holds capacity on one offer. The constraints
// and duration are the same ones the placement used: the reservation re-runs the
// real scheduler against the locked offer, so a hold can never be granted on
// terms a placement would have refused.
type CreateComputeReservationRequest struct {
	OfferID         string                `json:"offer_id"`
	WorkloadID      string                `json:"workload_id,omitempty"`
	MigrationID     string                `json:"migration_id,omitempty"`
	CheckpointID    string                `json:"checkpoint_id,omitempty"`
	SourceMachineID string                `json:"source_machine_id,omitempty"`
	SourceGeography scheduler.Geography   `json:"source_geography,omitempty"`
	Requested       scheduler.Resources   `json:"requested"`
	StateBytes      uint64                `json:"state_bytes,omitempty"`
	DurationSeconds int64                 `json:"duration_seconds,omitempty"`
	Constraints     scheduler.Constraints `json:"constraints,omitempty"`
	TTLSeconds      int64                 `json:"ttl_seconds,omitempty"`
}

// ComputeReservationStateRequest moves a reservation through its state machine.
type ComputeReservationStateRequest struct {
	State scheduler.ReservationState `json:"state"`
	Error string                     `json:"error,omitempty"`
}

// computeScheduler builds the scheduler this control plane places with. Public
// trading comes from configuration and is off unless an operator turned it on;
// everything else about the marketplace works either way.
func (server *Server) computeScheduler() *scheduler.Scheduler {
	configuration := scheduler.DefaultConfig()
	configuration.PublicTradingEnabled = server.config.ComputeTradingEnabled
	return scheduler.New(configuration)
}

func computeOfferResponse(offer scheduler.Offer, withdrawnAt *time.Time) ComputeOffer {
	return ComputeOffer{Offer: offer, Available: offer.Available(), WithdrawnAt: withdrawnAt}
}

// offerFromRecord rebuilds the scheduler's own offer type from storage. The
// status and visibility columns win over their JSON copies because those columns
// are what SQL filters on: trusting them keeps the visibility prefilter and the
// scheduler from ever disagreeing about what an offer is.
func offerFromRecord(record database.ComputeOfferRecord) (scheduler.Offer, error) {
	offer := scheduler.Offer{
		ID:               record.ID,
		OrganizationID:   record.OrganizationID,
		MachineID:        record.MachineID,
		MachineName:      record.MachineName,
		AgentURL:         record.AgentURL,
		Trust:            scheduler.TrustTier(record.Trust),
		IdentityVerified: record.IdentityVerified,
		CreatedAt:        record.CreatedAt,
		UpdatedAt:        record.UpdatedAt,
	}
	for _, field := range []struct {
		name string
		raw  json.RawMessage
		into any
	}{
		{"exposed", record.Exposed, &offer.Exposed},
		{"pricing", record.Pricing, &offer.Pricing},
		{"geography", record.Geography, &offer.Geography},
		{"policy", record.Policy, &offer.Policy},
		{"availability", record.Availability, &offer.Availability},
		{"capabilities", record.Capabilities, &offer.Capabilities},
		{"latency", record.Latency, &offer.Latency},
	} {
		if len(field.raw) == 0 {
			continue
		}
		if err := json.Unmarshal(field.raw, field.into); err != nil {
			return scheduler.Offer{}, fmt.Errorf("stored %s of compute offer %s is unreadable: %w", field.name, record.ID, err)
		}
	}
	offer.Availability.Status = scheduler.OfferStatus(record.Status)
	offer.Availability.LastSeenAt = record.LastSeenAt
	offer.Policy.Visibility = scheduler.Visibility(record.Visibility)
	offer.Normalize()
	return offer, nil
}

// recordFromOffer encodes a validated offer for storage. The scalar columns are
// derived from the same normalized offer as the JSON documents, so the indexed
// copies can never drift from the authoritative values. IdentityVerified is
// deliberately absent: only a verified machine identity may set it, never a
// publish request.
func recordFromOffer(offer scheduler.Offer) (database.ComputeOfferRecord, error) {
	documents := map[string]any{
		"exposed":      offer.Exposed,
		"pricing":      offer.Pricing,
		"geography":    offer.Geography,
		"policy":       offer.Policy,
		"availability": offer.Availability,
		"latency":      offer.Latency,
	}
	encoded := make(map[string]json.RawMessage, len(documents))
	for name, value := range documents {
		raw, err := json.Marshal(value)
		if err != nil {
			return database.ComputeOfferRecord{}, fmt.Errorf("encode compute offer %s: %w", name, err)
		}
		encoded[name] = raw
	}
	return database.ComputeOfferRecord{
		ID:             offer.ID,
		OrganizationID: offer.OrganizationID,
		MachineID:      offer.MachineID,
		Visibility:     string(offer.Policy.Visibility),
		Status:         string(offer.Availability.Status),
		Trust:          string(offer.Trust),
		Region:         offer.Geography.Region,
		Country:        offer.Geography.Country,
		Exposed:        encoded["exposed"],
		Pricing:        encoded["pricing"],
		Geography:      encoded["geography"],
		Policy:         encoded["policy"],
		Availability:   encoded["availability"],
		Latency:        encoded["latency"],
	}, nil
}

func reservationFromRecord(record database.ComputeReservationRecord) (scheduler.Reservation, error) {
	reservation := scheduler.Reservation{
		ID:             record.ID,
		OfferID:        record.OfferID,
		OrganizationID: record.OrganizationID,
		MachineID:      record.MachineID,
		WorkloadID:     record.WorkloadID,
		MigrationID:    record.MigrationID,
		State:          scheduler.ReservationState(record.State),
		HourlyMicros:   record.HourlyMicros,
		Currency:       record.Currency,
		ExpiresAt:      record.ExpiresAt,
		CreatedAt:      record.CreatedAt,
		UpdatedAt:      record.UpdatedAt,
		ReleasedAt:     record.ReleasedAt,
		Error:          record.ErrorMessage,
	}
	if len(record.Requested) > 0 {
		if err := json.Unmarshal(record.Requested, &reservation.Requested); err != nil {
			return scheduler.Reservation{}, fmt.Errorf("stored requested resources of reservation %s are unreadable: %w", record.ID, err)
		}
	}
	return reservation, nil
}

// computeInventory folds live reservations into a set of stored offers. It is the
// only place committed capacity is derived, so the offer list, the inventory
// view, placement, and reservation approval all see the same free capacity.
func (server *Server) computeInventory(ctx context.Context, records []database.ComputeOfferRecord, now time.Time) ([]scheduler.Offer, error) {
	if len(records) == 0 {
		return nil, nil
	}
	offerIDs := make([]string, 0, len(records))
	for _, record := range records {
		offerIDs = append(offerIDs, record.ID)
	}
	held, err := server.database.LiveComputeReservations(ctx, offerIDs, now)
	if err != nil {
		return nil, err
	}
	byOffer := make(map[string][]scheduler.Reservation, len(held))
	for _, record := range held {
		reservation, err := reservationFromRecord(record)
		if err != nil {
			return nil, err
		}
		byOffer[record.OfferID] = append(byOffer[record.OfferID], reservation)
	}
	offers := make([]scheduler.Offer, 0, len(records))
	for _, record := range records {
		offer, err := offerFromRecord(record)
		if err != nil {
			return nil, err
		}
		offer.Committed = scheduler.CommittedFrom(byOffer[record.ID], now)
		offers = append(offers, offer)
	}
	return offers, nil
}

// schedulableOffers is the inventory one organization may place onto: its own
// machines, partner offers that name it, and the public marketplace when trading
// is enabled. Lapsed holds are expired first so a crashed migration cannot pin
// capacity that placement would then refuse to use.
func (server *Server) schedulableOffers(ctx context.Context, organizationID string) ([]scheduler.Offer, error) {
	now := time.Now().UTC()
	if _, err := server.database.ExpireComputeReservations(ctx, now); err != nil {
		return nil, err
	}
	records, err := server.database.SchedulableComputeOffers(ctx, organizationID, server.config.ComputeTradingEnabled)
	if err != nil {
		return nil, err
	}
	return server.computeInventory(ctx, records, now)
}

// checkpointManifest loads a stored manifest so placement can run the same
// restore-compatibility check the migration engine runs. It also returns the
// captured state size, which is more trustworthy than a client-supplied figure.
// An empty workloadID searches every checkpoint in the organization.
func (server *Server) checkpointManifest(ctx context.Context, organizationID, workloadID, checkpointID string) (model.CheckpointManifest, int64, bool, error) {
	records, err := server.database.Checkpoints(ctx, organizationID, workloadID)
	if err != nil {
		return model.CheckpointManifest{}, 0, false, err
	}
	for _, record := range records {
		if record.ID != checkpointID {
			continue
		}
		var manifest model.CheckpointManifest
		if len(record.Manifest) > 0 {
			if err := json.Unmarshal(record.Manifest, &manifest); err != nil {
				return model.CheckpointManifest{}, 0, false, fmt.Errorf("stored manifest of checkpoint %s is unreadable: %w", record.ID, err)
			}
		}
		return manifest, record.PlainBytes, true, nil
	}
	return model.CheckpointManifest{}, 0, false, nil
}

func (server *Server) handleComputeOfferList(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	records, err := server.database.ComputeOffers(request.Context(), organizationID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "COMPUTE_OFFERS_LOOKUP_FAILED", err.Error())
		return
	}
	now := time.Now().UTC()
	offers, err := server.computeInventory(request.Context(), records, now)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "COMPUTE_OFFERS_DECODE_FAILED", err.Error())
		return
	}
	withdrawn := make(map[string]*time.Time, len(records))
	for _, record := range records {
		withdrawn[record.ID] = record.WithdrawnAt
	}
	result := make([]ComputeOffer, 0, len(offers))
	for _, offer := range offers {
		result = append(result, computeOfferResponse(offer, withdrawn[offer.ID]))
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) handleComputeInventory(writer http.ResponseWriter, request *http.Request) {
	offers, err := server.schedulableOffers(request.Context(), request.PathValue("organizationID"))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "COMPUTE_INVENTORY_FAILED", err.Error())
		return
	}
	result := make([]ComputeOffer, 0, len(offers))
	for _, offer := range offers {
		result = append(result, computeOfferResponse(offer, nil))
	}
	writeJSON(writer, http.StatusOK, ComputeInventory{TradingEnabled: server.config.ComputeTradingEnabled, Offers: result})
}

func (server *Server) handleComputeOfferPublish(writer http.ResponseWriter, request *http.Request) {
	var input PublishComputeOfferRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	organizationID := request.PathValue("organizationID")
	machineID := strings.TrimSpace(input.MachineID)
	if machineID == "" {
		writeError(writer, http.StatusBadRequest, "COMPUTE_OFFER_MACHINE_REQUIRED", "an offer must name a registered machine")
		return
	}
	if input.Policy.Visibility == scheduler.VisibilityPublic && !server.config.ComputeTradingEnabled {
		writeError(writer, http.StatusConflict, "COMPUTE_TRADING_DISABLED", "public offers require compute trading to be enabled on this control plane")
		return
	}
	machine, err := server.database.Machine(request.Context(), organizationID, machineID)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "COMPUTE_OFFER_MACHINE_NOT_FOUND", "the machine is not registered in this organization")
			return
		}
		writeError(writer, http.StatusInternalServerError, "COMPUTE_OFFER_MACHINE_LOOKUP_FAILED", err.Error())
		return
	}
	if machine.Status == "disabled" {
		writeError(writer, http.StatusConflict, "COMPUTE_OFFER_MACHINE_DISABLED", "a disabled machine cannot offer resources")
		return
	}
	offerID, err := model.NewID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "ID_GENERATION_FAILED", err.Error())
		return
	}
	offer := scheduler.Offer{
		ID:             offerID,
		OrganizationID: organizationID,
		MachineID:      machineID,
		MachineName:    machine.Name,
		AgentURL:       machine.AgentURL,
		Exposed:        input.Exposed,
		Pricing:        input.Pricing,
		Geography:      input.Geography,
		Policy:         input.Policy,
		Trust:          input.Trust,
		Latency:        input.Latency,
		Availability: scheduler.Availability{
			Status:     input.Status,
			WindowFrom: input.WindowFrom,
			WindowTo:   input.WindowTo,
			LastSeenAt: machine.LastSeenAt,
		},
	}
	if offer.Availability.Status == "" {
		// Publishing an offer means offering it. Presence is still enforced
		// separately: a machine that stops reporting is rejected as stale.
		offer.Availability.Status = scheduler.OfferAvailable
	}
	if len(machine.Capabilities) > 0 {
		if err := json.Unmarshal(machine.Capabilities, &offer.Capabilities); err != nil {
			writeError(writer, http.StatusInternalServerError, "COMPUTE_OFFER_CAPABILITIES_UNREADABLE", err.Error())
			return
		}
	}
	if err := offer.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "COMPUTE_OFFER_INVALID", err.Error())
		return
	}
	server.publishComputeOffer(writer, request, offer, machineID)
}

func (server *Server) publishComputeOffer(writer http.ResponseWriter, request *http.Request, offer scheduler.Offer, machineID string) {
	record, err := recordFromOffer(offer)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "COMPUTE_OFFER_ENCODE_FAILED", err.Error())
		return
	}
	audit := server.auditInput(request, "compute.offer.publish", "compute_offer", offer.ID, map[string]any{
		"machine_id": machineID,
		"visibility": string(offer.Policy.Visibility),
		"status":     string(offer.Availability.Status),
	})
	stored, err := server.database.UpsertComputeOffer(request.Context(), record, audit)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "COMPUTE_OFFER_MACHINE_NOT_FOUND", "the machine is not registered in this organization")
			return
		}
		writeError(writer, http.StatusBadRequest, "COMPUTE_OFFER_PUBLISH_FAILED", err.Error())
		return
	}
	published, err := offerFromRecord(stored)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "COMPUTE_OFFER_DECODE_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, computeOfferResponse(published, stored.WithdrawnAt))
}

func (server *Server) handleComputeOfferWithdraw(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	offerID := request.PathValue("offerID")
	audit := server.auditInput(request, "compute.offer.withdraw", "compute_offer", offerID, nil)
	stored, err := server.database.WithdrawComputeOffer(request.Context(), organizationID, offerID, audit)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "COMPUTE_OFFER_NOT_FOUND", "the offer does not exist in this organization")
			return
		}
		writeError(writer, http.StatusInternalServerError, "COMPUTE_OFFER_WITHDRAW_FAILED", err.Error())
		return
	}
	offer, err := offerFromRecord(stored)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "COMPUTE_OFFER_DECODE_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, computeOfferResponse(offer, stored.WithdrawnAt))
}

func (server *Server) handleComputePlacement(writer http.ResponseWriter, request *http.Request) {
	var input PlacementRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	organizationID := request.PathValue("organizationID")
	if input.DurationSeconds < 0 {
		writeError(writer, http.StatusBadRequest, "PLACEMENT_DURATION_INVALID", "placement duration cannot be negative")
		return
	}
	placement := scheduler.Request{
		WorkloadID:            input.WorkloadID,
		RequesterOrganization: organizationID,
		SourceMachineID:       input.SourceMachineID,
		SourceGeography:       input.SourceGeography,
		Requirements:          input.Requirements,
		StateBytes:            input.StateBytes,
		Duration:              time.Duration(input.DurationSeconds) * time.Second,
		Constraints:           input.Constraints,
		Weights:               input.Weights,
		Limit:                 input.Limit,
	}
	if !server.applyCheckpointToRequest(writer, request, organizationID, input.CheckpointID, &placement) {
		return
	}
	offers, err := server.schedulableOffers(request.Context(), organizationID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "PLACEMENT_INVENTORY_FAILED", err.Error())
		return
	}
	result, err := server.computeScheduler().Place(placement, offers)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "PLACEMENT_INVALID", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

// applyCheckpointToRequest attaches a stored checkpoint's manifest and captured
// size to a placement request. It writes its own error response and reports
// whether the caller may continue.
func (server *Server) applyCheckpointToRequest(writer http.ResponseWriter, request *http.Request, organizationID, checkpointID string, placement *scheduler.Request) bool {
	if strings.TrimSpace(checkpointID) == "" {
		return true
	}
	manifest, plainBytes, found, err := server.checkpointManifest(request.Context(), organizationID, placement.WorkloadID, checkpointID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "PLACEMENT_CHECKPOINT_LOOKUP_FAILED", err.Error())
		return false
	}
	if !found {
		writeError(writer, http.StatusNotFound, "PLACEMENT_CHECKPOINT_NOT_FOUND", "the checkpoint does not exist in this organization")
		return false
	}
	placement.Manifest = &manifest
	if placement.StateBytes == 0 && plainBytes > 0 {
		placement.StateBytes = uint64(plainBytes)
	}
	return true
}

// capacityRejections are the placement failures that mean "there is no room right
// now" rather than "this offer will never accept this workload". The distinction
// tells a client whether retrying or changing the request is the right response.
var capacityRejections = map[string]bool{
	scheduler.RejectCPU:     true,
	scheduler.RejectMemory:  true,
	scheduler.RejectStorage: true,
	scheduler.RejectNetwork: true,
	scheduler.RejectGPU:     true,
	scheduler.RejectStatus:  true,
	scheduler.RejectStale:   true,
	scheduler.RejectWindow:  true,
}

func (server *Server) handleComputeReservationList(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	if _, err := server.database.ExpireComputeReservations(request.Context(), time.Now().UTC()); err != nil {
		writeError(writer, http.StatusInternalServerError, "COMPUTE_RESERVATION_EXPIRY_FAILED", err.Error())
		return
	}
	records, err := server.database.ComputeReservations(request.Context(), organizationID, strings.TrimSpace(request.URL.Query().Get("offer_id")))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "COMPUTE_RESERVATIONS_LOOKUP_FAILED", err.Error())
		return
	}
	result := make([]scheduler.Reservation, 0, len(records))
	for _, record := range records {
		reservation, err := reservationFromRecord(record)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "COMPUTE_RESERVATIONS_DECODE_FAILED", err.Error())
			return
		}
		result = append(result, reservation)
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) handleComputeReservationCreate(writer http.ResponseWriter, request *http.Request) {
	var input CreateComputeReservationRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	organizationID := request.PathValue("organizationID")
	offerID := strings.TrimSpace(input.OfferID)
	if offerID == "" {
		writeError(writer, http.StatusBadRequest, "COMPUTE_RESERVATION_OFFER_REQUIRED", "a reservation must name the offer it holds")
		return
	}
	if input.DurationSeconds < 0 || input.TTLSeconds < 0 {
		writeError(writer, http.StatusBadRequest, "COMPUTE_RESERVATION_INVALID", "reservation duration and ttl cannot be negative")
		return
	}
	ttl := time.Duration(input.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = server.config.ComputeReservationTTL
	}
	if ttl > maxComputeReservationTTL {
		writeError(writer, http.StatusBadRequest, "COMPUTE_RESERVATION_TTL_TOO_LONG", "a reservation may hold capacity for at most "+maxComputeReservationTTL.String())
		return
	}
	placement := scheduler.Request{
		WorkloadID:            input.WorkloadID,
		RequesterOrganization: organizationID,
		SourceMachineID:       input.SourceMachineID,
		SourceGeography:       input.SourceGeography,
		Requirements:          input.Requested,
		StateBytes:            input.StateBytes,
		Duration:              time.Duration(input.DurationSeconds) * time.Second,
		Constraints:           input.Constraints,
	}
	if !server.applyCheckpointToRequest(writer, request, organizationID, input.CheckpointID, &placement) {
		return
	}
	now := time.Now().UTC()
	reservationID, err := model.NewID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "ID_GENERATION_FAILED", err.Error())
		return
	}
	reservation := scheduler.Reservation{
		ID:             reservationID,
		OfferID:        offerID,
		OrganizationID: organizationID,
		WorkloadID:     strings.TrimSpace(input.WorkloadID),
		MigrationID:    strings.TrimSpace(input.MigrationID),
		Requested:      input.Requested,
		State:          scheduler.ReservationPending,
		ExpiresAt:      now.Add(ttl),
	}
	if err := reservation.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "COMPUTE_RESERVATION_INVALID", err.Error())
		return
	}
	requested, err := json.Marshal(reservation.Requested)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "COMPUTE_RESERVATION_ENCODE_FAILED", err.Error())
		return
	}
	server.createComputeReservation(writer, request, database.ComputeReservationRecord{
		ID:             reservation.ID,
		OfferID:        reservation.OfferID,
		OrganizationID: reservation.OrganizationID,
		WorkloadID:     reservation.WorkloadID,
		MigrationID:    reservation.MigrationID,
		State:          string(reservation.State),
		Requested:      requested,
		ExpiresAt:      reservation.ExpiresAt,
	}, placement, now, ttl)
}

// createComputeReservation commits the hold. The capacity decision is made inside
// the store's locked transaction by the real scheduler running against the real
// stored offer, so a reservation can never be granted on terms a placement would
// have refused, and two concurrent callers can never both take the same device.
func (server *Server) createComputeReservation(writer http.ResponseWriter, request *http.Request, record database.ComputeReservationRecord, placement scheduler.Request, now time.Time, ttl time.Duration) {
	var rejection *scheduler.Rejection
	approve := func(offerRecord database.ComputeOfferRecord, live []database.ComputeReservationRecord, target *database.ComputeReservationRecord) error {
		offer, err := offerFromRecord(offerRecord)
		if err != nil {
			return err
		}
		held := make([]scheduler.Reservation, 0, len(live))
		for _, item := range live {
			existing, err := reservationFromRecord(item)
			if err != nil {
				return err
			}
			held = append(held, existing)
		}
		offer.Committed = scheduler.CommittedFrom(held, now)
		result, err := server.computeScheduler().Place(placement, []scheduler.Offer{offer})
		if err != nil {
			return err
		}
		candidate, ok := result.Best()
		if !ok {
			reason := "the offer cannot satisfy this reservation"
			if len(result.Rejected) > 0 {
				rejection = &result.Rejected[0]
				reason = rejection.Code + ": " + rejection.Reason
			}
			return fmt.Errorf("%w: %s", database.ErrComputeCapacityUnavailable, reason)
		}
		target.MachineID = candidate.MachineID
		target.HourlyMicros = candidate.HourlyMicros
		target.Currency = candidate.Currency
		return nil
	}
	audit := server.auditInput(request, "compute.reservation.create", "compute_reservation", record.ID, map[string]any{
		"offer_id":    record.OfferID,
		"workload_id": record.WorkloadID,
		"ttl_seconds": int64(ttl.Seconds()),
	})
	stored, err := server.database.CreateComputeReservation(request.Context(), record, approve, audit)
	if err != nil {
		server.writeComputeReservationError(writer, err, rejection)
		return
	}
	created, err := reservationFromRecord(stored)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "COMPUTE_RESERVATION_DECODE_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, created)
}

func (server *Server) writeComputeReservationError(writer http.ResponseWriter, err error, rejection *scheduler.Rejection) {
	switch {
	case errors.Is(err, database.ErrComputeCapacityUnavailable):
		if rejection != nil && capacityRejections[rejection.Code] {
			writeError(writer, http.StatusConflict, "COMPUTE_CAPACITY_UNAVAILABLE", err.Error())
			return
		}
		writeError(writer, http.StatusUnprocessableEntity, "COMPUTE_RESERVATION_REFUSED", err.Error())
	case database.IsNotFound(err):
		writeError(writer, http.StatusNotFound, "COMPUTE_OFFER_NOT_FOUND", "the offer does not exist or has been withdrawn")
	case database.IsConflict(err):
		writeError(writer, http.StatusConflict, "COMPUTE_RESERVATION_EXISTS", "a reservation with this id already exists")
	default:
		writeError(writer, http.StatusBadRequest, "COMPUTE_RESERVATION_FAILED", err.Error())
	}
}

func (server *Server) handleComputeReservationState(writer http.ResponseWriter, request *http.Request) {
	var input ComputeReservationStateRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	next := scheduler.ReservationState(strings.ToUpper(strings.TrimSpace(string(input.State))))
	if !next.Valid() {
		writeError(writer, http.StatusBadRequest, "COMPUTE_RESERVATION_STATE_INVALID", "unsupported reservation state")
		return
	}
	organizationID := request.PathValue("organizationID")
	reservationID := request.PathValue("reservationID")
	allow := func(current string) error {
		from := scheduler.ReservationState(current)
		if !scheduler.CanTransitionReservation(from, next) {
			return fmt.Errorf("%w: %s cannot become %s", database.ErrComputeReservationTransition, from, next)
		}
		return nil
	}
	audit := server.auditInput(request, "compute.reservation.state", "compute_reservation", reservationID, map[string]any{"state": string(next)})
	stored, err := server.database.UpdateComputeReservationState(request.Context(), organizationID, reservationID, string(next), input.Error, allow, audit)
	if err != nil {
		switch {
		case errors.Is(err, database.ErrComputeReservationTransition):
			writeError(writer, http.StatusConflict, "COMPUTE_RESERVATION_TRANSITION_INVALID", err.Error())
		case database.IsNotFound(err):
			writeError(writer, http.StatusNotFound, "COMPUTE_RESERVATION_NOT_FOUND", "the reservation does not exist in this organization")
		default:
			writeError(writer, http.StatusInternalServerError, "COMPUTE_RESERVATION_STATE_FAILED", err.Error())
		}
		return
	}
	updated, err := reservationFromRecord(stored)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "COMPUTE_RESERVATION_DECODE_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, updated)
}
