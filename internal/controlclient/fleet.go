package controlclient

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/scheduler"
)

// fleet.go: typed wrappers over the organization endpoints the fleet CLI
// needs. The structs mirror the control plane's wire shapes exactly — the JSON
// tags are the contract — and the compute types reuse the scheduler package's
// own so an offer decodes into precisely what was serialized.

// organizationPath builds an /v1/organizations/... path. The organization id
// is escaped so an id can never smuggle path separators into the request.
func organizationPath(organizationID, suffix string) string {
	return "/v1/organizations/" + url.PathEscape(strings.TrimSpace(organizationID)) + suffix
}

// Machine is a registered machine as the fleet API reports it.
type Machine struct {
	ID             string         `json:"id"`
	OrganizationID string         `json:"organization_id"`
	MachineID      string         `json:"machine_id"`
	Name           string         `json:"name"`
	AgentURL       string         `json:"agent_url"`
	Capabilities   map[string]any `json:"capabilities"`
	Status         string         `json:"status"`
	LastSeenAt     *time.Time     `json:"last_seen_at,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// Workload is a registered workload and its last reported status.
type Workload struct {
	ID             string         `json:"id"`
	OrganizationID string         `json:"organization_id"`
	MachineID      string         `json:"machine_id,omitempty"`
	Name           string         `json:"name"`
	Spec           map[string]any `json:"spec"`
	Status         map[string]any `json:"status"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// MigrationJob is one migration as the control plane tracks it.
type MigrationJob struct {
	ID                   string         `json:"id"`
	OrganizationID       string         `json:"organization_id"`
	WorkloadID           string         `json:"workload_id"`
	SourceMachineID      string         `json:"source_machine_id"`
	DestinationMachineID string         `json:"destination_machine_id"`
	Mode                 string         `json:"mode"`
	Status               string         `json:"status"`
	Progress             map[string]any `json:"progress"`
	ErrorMessage         string         `json:"error_message,omitempty"`
	CreatedBy            string         `json:"created_by"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
	CompletedAt          *time.Time     `json:"completed_at,omitempty"`
}

// MigrationEvent is one progress event in a migration's stream.
type MigrationEvent struct {
	ID         string    `json:"id"`
	Sequence   int64     `json:"sequence"`
	Stage      string    `json:"stage"`
	Message    string    `json:"message"`
	Progress   float64   `json:"progress"`
	BytesDone  int64     `json:"bytes_done"`
	BytesTotal int64     `json:"bytes_total"`
	CreatedAt  time.Time `json:"created_at"`
}

// Entitlement is the organization's plan and its remaining allowance.
type Entitlement struct {
	OrganizationID   string `json:"organization_id"`
	Plan             string `json:"plan"`
	Status           string `json:"status"`
	MaxStorageBytes  int64  `json:"max_storage_bytes"`
	UsedStorageBytes int64  `json:"used_storage_bytes"`
	MaxMachines      int    `json:"max_machines"`
	StripeCustomerID string `json:"stripe_customer_id,omitempty"`
}

// UsageSummary is one metered kind over one period.
type UsageSummary struct {
	Kind        string `json:"kind"`
	Quantity    int64  `json:"quantity"`
	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
}

// AuditEvent is one entry from the organization's audit trail.
type AuditEvent struct {
	ID             string         `json:"id"`
	OrganizationID string         `json:"organization_id,omitempty"`
	ActorUserID    string         `json:"actor_user_id,omitempty"`
	Action         string         `json:"action"`
	ResourceType   string         `json:"resource_type"`
	ResourceID     string         `json:"resource_id,omitempty"`
	Metadata       map[string]any `json:"metadata"`
	RequestID      string         `json:"request_id,omitempty"`
	RemoteAddr     string         `json:"remote_addr,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
}

// APIKey is a stored key. The secret is never in it — only its digest is kept
// server-side, and the secret is returned exactly once at creation.
type APIKey struct {
	ID             string     `json:"id"`
	OrganizationID string     `json:"organization_id"`
	Name           string     `json:"name"`
	Prefix         string     `json:"prefix"`
	Scopes         []string   `json:"scopes"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// APIKeyCreated is the one-time response to key creation.
type APIKeyCreated struct {
	APIKey
	Secret string `json:"secret"`
}

// CreateAPIKeyInput names and scopes a new key.
type CreateAPIKeyInput struct {
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Checkpoint is one stored checkpoint of a workload.
type Checkpoint struct {
	ID             string         `json:"id"`
	OrganizationID string         `json:"organization_id"`
	WorkloadID     string         `json:"workload_id"`
	MachineID      string         `json:"machine_id"`
	Kind           string         `json:"kind"`
	ParentID       string         `json:"parent_id,omitempty"`
	Manifest       map[string]any `json:"manifest"`
	PlainBytes     int64          `json:"plain_bytes"`
	StoredBytes    int64          `json:"stored_bytes"`
	ChunkCount     int            `json:"chunk_count"`
	Status         string         `json:"status"`
	CreatedAt      time.Time      `json:"created_at"`
	DeletedAt      *time.Time     `json:"deleted_at,omitempty"`
}

// CreateMachineInput registers a machine. The agent URL must be https — peers
// and the dispatcher dial it directly.
type CreateMachineInput struct {
	MachineID    string         `json:"machine_id"`
	Name         string         `json:"name"`
	AgentURL     string         `json:"agent_url"`
	Capabilities map[string]any `json:"capabilities"`
}

// MachineHeartbeatInput refreshes a machine's presence. An empty status means
// online.
type MachineHeartbeatInput struct {
	Name         string         `json:"name"`
	AgentURL     string         `json:"agent_url"`
	Capabilities map[string]any `json:"capabilities"`
	Status       string         `json:"status"`
}

// CreateWorkloadInput registers a workload on a machine.
type CreateWorkloadInput struct {
	MachineID string         `json:"machine_id"`
	Name      string         `json:"name"`
	Spec      map[string]any `json:"spec"`
	Status    map[string]any `json:"status,omitempty"`
}

// CreateMigrationInput asks the control plane to move a workload.
type CreateMigrationInput struct {
	WorkloadID           string `json:"workload_id"`
	SourceMachineID      string `json:"source_machine_id"`
	DestinationMachineID string `json:"destination_machine_id"`
	Mode                 string `json:"mode"`
}

// CreateCheckpointInput records a checkpoint the agent has already written.
type CreateCheckpointInput struct {
	ID          string         `json:"id"`
	WorkloadID  string         `json:"workload_id"`
	MachineID   string         `json:"machine_id"`
	Kind        string         `json:"kind"`
	ParentID    string         `json:"parent_id,omitempty"`
	Manifest    map[string]any `json:"manifest"`
	PlainBytes  int64          `json:"plain_bytes"`
	StoredBytes int64          `json:"stored_bytes"`
	ChunkCount  int            `json:"chunk_count"`
}

// ComputeOffer is a stored offer. Available is the capacity left after live
// reservations — the figure to reason about, not the raw exposure.
type ComputeOffer struct {
	scheduler.Offer
	Available   scheduler.Resources `json:"available"`
	WithdrawnAt *time.Time          `json:"withdrawn_at,omitempty"`
}

// ComputeInventory is everything the organization may schedule against plus
// the public-trading gate.
type ComputeInventory struct {
	TradingEnabled bool           `json:"trading_enabled"`
	Offers         []ComputeOffer `json:"offers"`
}

// PublishOfferInput exposes one registered machine's resources. Machine
// identity, agent URL, and hardware are taken from the control plane's own
// registry, not from this body.
type PublishOfferInput struct {
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

// PlacementInput asks where a workload could go. CheckpointID is resolved
// server-side into the stored manifest, so restore compatibility is checked
// against state that was actually captured.
type PlacementInput struct {
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

// CreateReservationInput holds capacity on one offer. The hold re-runs the
// real scheduler server-side, so it can never be granted on terms a placement
// would refuse.
type CreateReservationInput struct {
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

// Machines lists the organization's registered machines.
func (session *Session) Machines(ctx context.Context, organizationID string) ([]Machine, error) {
	var result []Machine
	return result, session.call(ctx, "GET", organizationPath(organizationID, "/machines"), nil, &result)
}

// RegisterMachine adds a machine to the organization.
func (session *Session) RegisterMachine(ctx context.Context, organizationID string, input CreateMachineInput) (Machine, error) {
	var result Machine
	return result, session.call(ctx, "POST", organizationPath(organizationID, "/machines"), input, &result)
}

// MachineHeartbeat refreshes a machine's presence and status.
func (session *Session) MachineHeartbeat(ctx context.Context, organizationID, machineID string, input MachineHeartbeatInput) (Machine, error) {
	var result Machine
	return result, session.call(ctx, "POST", organizationPath(organizationID, "/machines/"+url.PathEscape(machineID)+"/heartbeat"), input, &result)
}

// MachinesWithCapability asks the fleet query the capability projection backs:
// which machines report this capability. kind is "number", "text", or
// "boolean"; minimum applies to numbers only and zero means no floor.
func (session *Session) MachinesWithCapability(ctx context.Context, organizationID, capability, kind string, minimum float64) ([]Machine, error) {
	requestPath := organizationPath(organizationID, "/machines/capability") +
		"?capability=" + url.QueryEscape(capability) + "&kind=" + url.QueryEscape(kind)
	if minimum != 0 {
		requestPath += "&minimum=" + url.QueryEscape(strconv.FormatFloat(minimum, 'f', -1, 64))
	}
	var result []Machine
	return result, session.call(ctx, "GET", requestPath, nil, &result)
}

// Workloads lists the organization's workloads.
func (session *Session) Workloads(ctx context.Context, organizationID string) ([]Workload, error) {
	var result []Workload
	return result, session.call(ctx, "GET", organizationPath(organizationID, "/workloads"), nil, &result)
}

// RegisterWorkload adds a workload to the organization.
func (session *Session) RegisterWorkload(ctx context.Context, organizationID string, input CreateWorkloadInput) (Workload, error) {
	var result Workload
	return result, session.call(ctx, "POST", organizationPath(organizationID, "/workloads"), input, &result)
}

// Migrations lists the organization's migration jobs.
func (session *Session) Migrations(ctx context.Context, organizationID string) ([]MigrationJob, error) {
	var result []MigrationJob
	return result, session.call(ctx, "GET", organizationPath(organizationID, "/migrations"), nil, &result)
}

// CreateMigration asks the control plane to move a workload. The job is
// accepted, not completed: poll Migrations or MigrationEvents for the outcome.
func (session *Session) CreateMigration(ctx context.Context, organizationID string, input CreateMigrationInput) (MigrationJob, error) {
	var result MigrationJob
	return result, session.call(ctx, "POST", organizationPath(organizationID, "/migrations"), input, &result)
}

// MigrationEvents streams one migration's progress events in order.
func (session *Session) MigrationEvents(ctx context.Context, organizationID, migrationID string) ([]MigrationEvent, error) {
	var result []MigrationEvent
	return result, session.call(ctx, "GET", organizationPath(organizationID, "/migrations/"+url.PathEscape(migrationID)+"/events"), nil, &result)
}

// CancelMigration stops an active migration. The returned job is the
// control plane's final view of it.
func (session *Session) CancelMigration(ctx context.Context, organizationID, migrationID string) (MigrationJob, error) {
	var result MigrationJob
	return result, session.call(ctx, "POST", organizationPath(organizationID, "/migrations/"+url.PathEscape(migrationID)+"/cancel"), nil, &result)
}

// Checkpoints lists the organization's checkpoints, optionally filtered to one
// workload.
func (session *Session) Checkpoints(ctx context.Context, organizationID, workloadID string) ([]Checkpoint, error) {
	requestPath := organizationPath(organizationID, "/checkpoints")
	if workloadID != "" {
		requestPath += "?workload_id=" + url.QueryEscape(workloadID)
	}
	var result []Checkpoint
	return result, session.call(ctx, "GET", requestPath, nil, &result)
}

// RegisterCheckpoint records a checkpoint the agent has already written.
func (session *Session) RegisterCheckpoint(ctx context.Context, organizationID string, input CreateCheckpointInput) (Checkpoint, error) {
	var result Checkpoint
	return result, session.call(ctx, "POST", organizationPath(organizationID, "/checkpoints"), input, &result)
}

// Entitlement is the organization's plan and its remaining allowance.
func (session *Session) Entitlement(ctx context.Context, organizationID string) (Entitlement, error) {
	var result Entitlement
	return result, session.call(ctx, "GET", organizationPath(organizationID, "/entitlement"), nil, &result)
}

// Usage summarizes metered usage over an inclusive date range. Both bounds
// are optional; the control plane defaults to the trailing month.
func (session *Session) Usage(ctx context.Context, organizationID string, from, to time.Time) ([]UsageSummary, error) {
	requestPath := organizationPath(organizationID, "/usage")
	separator := "?"
	if !from.IsZero() {
		requestPath += separator + "from=" + url.QueryEscape(from.Format("2006-01-02"))
		separator = "&"
	}
	if !to.IsZero() {
		requestPath += separator + "to=" + url.QueryEscape(to.Format("2006-01-02"))
	}
	var result []UsageSummary
	return result, session.call(ctx, "GET", requestPath, nil, &result)
}

// AuditEvents reads the organization's audit trail, most recent first. A
// limit of zero or less means the server's default.
func (session *Session) AuditEvents(ctx context.Context, organizationID string, limit int) ([]AuditEvent, error) {
	requestPath := organizationPath(organizationID, "/audit")
	if limit > 0 {
		requestPath += "?limit=" + url.QueryEscape(strconv.Itoa(limit))
	}
	var result []AuditEvent
	return result, session.call(ctx, "GET", requestPath, nil, &result)
}

// RetentionPolicy mirrors the control plane's retention wire shape exactly —
// the days fields are plain ints because zero (keep forever) is a real value,
// not an unset one.
type RetentionPolicy struct {
	OrganizationID              string    `json:"organization_id"`
	AuditRetentionDays          int       `json:"audit_retention_days"`
	CheckpointRetentionDays     int       `json:"checkpoint_retention_days"`
	DeletedStorageRetentionDays int       `json:"deleted_storage_retention_days"`
	UpdatedAt                   time.Time `json:"updated_at"`
}

// RetentionPolicyUpdate changes the windows the caller names. A nil field is
// "leave unchanged"; a pointer to zero is "keep forever" — the distinction is
// the whole reason the fields are pointers.
type RetentionPolicyUpdate struct {
	AuditRetentionDays          *int `json:"audit_retention_days,omitempty"`
	CheckpointRetentionDays     *int `json:"checkpoint_retention_days,omitempty"`
	DeletedStorageRetentionDays *int `json:"deleted_storage_retention_days,omitempty"`
}

// RetentionPolicy reads the organization's retention windows.
func (session *Session) RetentionPolicy(ctx context.Context, organizationID string) (RetentionPolicy, error) {
	var result RetentionPolicy
	return result, session.call(ctx, "GET", organizationPath(organizationID, "/retention"), nil, &result)
}

// SetRetentionPolicy applies the named windows and returns the stored policy.
func (session *Session) SetRetentionPolicy(ctx context.Context, organizationID string, update RetentionPolicyUpdate) (RetentionPolicy, error) {
	var result RetentionPolicy
	return result, session.call(ctx, "PUT", organizationPath(organizationID, "/retention"), update, &result)
}

// OrganizationSSO is the organization's single sign-on enforcement state.
type OrganizationSSO struct {
	Enforced    bool   `json:"enforced"`
	EmailDomain string `json:"email_domain"`
}

// OrganizationSSO reads the enforcement state.
func (session *Session) OrganizationSSO(ctx context.Context, organizationID string) (OrganizationSSO, error) {
	var result OrganizationSSO
	return result, session.call(ctx, "GET", organizationPath(organizationID, "/sso"), nil, &result)
}

// SetOrganizationSSO enforces SSO for an email domain — or disables
// enforcement, which clears the claim. One organization claims a domain at a
// time; a claimed domain answers 409.
func (session *Session) SetOrganizationSSO(ctx context.Context, organizationID string, enforced bool, emailDomain string) (OrganizationSSO, error) {
	var result OrganizationSSO
	return result, session.call(ctx, "PUT", organizationPath(organizationID, "/sso"), OrganizationSSO{Enforced: enforced, EmailDomain: emailDomain}, &result)
}

// APIKeys lists the organization's keys, revoked ones included.
func (session *Session) APIKeys(ctx context.Context, organizationID string) ([]APIKey, error) {
	var result []APIKey
	return result, session.call(ctx, "GET", organizationPath(organizationID, "/api-keys"), nil, &result)
}

// CreateAPIKey mints a key. The secret in the result is shown once and never
// stored server-side — the caller must surface it immediately.
func (session *Session) CreateAPIKey(ctx context.Context, organizationID string, input CreateAPIKeyInput) (APIKeyCreated, error) {
	var result APIKeyCreated
	return result, session.call(ctx, "POST", organizationPath(organizationID, "/api-keys"), input, &result)
}

// RevokeAPIKey permanently disables a key.
func (session *Session) RevokeAPIKey(ctx context.Context, organizationID, keyID string) error {
	return session.call(ctx, "DELETE", organizationPath(organizationID, "/api-keys/"+url.PathEscape(keyID)), nil, nil)
}

// Plans lists the public plan catalog. It needs no session — pricing and
// limits are public by design — but it rides the same client so one
// configuration serves both.
func (session *Session) Plans(ctx context.Context) ([]billing.Plan, error) {
	var result []billing.Plan
	return result, session.client.Plans(ctx, &result)
}

// ComputeOffers lists the offers the organization has published, including
// withdrawn ones.
func (session *Session) ComputeOffers(ctx context.Context, organizationID string) ([]ComputeOffer, error) {
	var result []ComputeOffer
	return result, session.call(ctx, "GET", organizationPath(organizationID, "/compute/offers"), nil, &result)
}

// PublishComputeOffer exposes a registered machine's resources.
func (session *Session) PublishComputeOffer(ctx context.Context, organizationID string, input PublishOfferInput) (ComputeOffer, error) {
	var result ComputeOffer
	return result, session.call(ctx, "POST", organizationPath(organizationID, "/compute/offers"), input, &result)
}

// WithdrawComputeOffer takes an offer off the market. Existing reservations
// are unaffected; new placements stop seeing it.
func (session *Session) WithdrawComputeOffer(ctx context.Context, organizationID, offerID string) (ComputeOffer, error) {
	var result ComputeOffer
	return result, session.call(ctx, "DELETE", organizationPath(organizationID, "/compute/offers/"+url.PathEscape(offerID)), nil, &result)
}

// ComputeInventory is everything the organization may schedule against right
// now, across its own machines and any the marketplace exposes to it.
func (session *Session) ComputeInventory(ctx context.Context, organizationID string) (ComputeInventory, error) {
	var result ComputeInventory
	return result, session.call(ctx, "GET", organizationPath(organizationID, "/compute/inventory"), nil, &result)
}

// PlaceCompute ranks the destinations a workload could move to. It commits
// nothing; CreateComputeReservation is what holds capacity.
func (session *Session) PlaceCompute(ctx context.Context, organizationID string, input PlacementInput) (scheduler.Placement, error) {
	var result scheduler.Placement
	return result, session.call(ctx, "POST", organizationPath(organizationID, "/compute/placements"), input, &result)
}

// ComputeReservations lists live holds, optionally filtered to one offer.
func (session *Session) ComputeReservations(ctx context.Context, organizationID, offerID string) ([]scheduler.Reservation, error) {
	requestPath := organizationPath(organizationID, "/compute/reservations")
	if offerID != "" {
		requestPath += "?offer_id=" + url.QueryEscape(offerID)
	}
	var result []scheduler.Reservation
	return result, session.call(ctx, "GET", requestPath, nil, &result)
}

// CreateComputeReservation holds capacity on an offer until it expires or is
// transitioned.
func (session *Session) CreateComputeReservation(ctx context.Context, organizationID string, input CreateReservationInput) (scheduler.Reservation, error) {
	var result scheduler.Reservation
	return result, session.call(ctx, "POST", organizationPath(organizationID, "/compute/reservations"), input, &result)
}

// SetComputeReservationState moves a reservation through its state machine —
// commit it when the migration lands, release or fail it otherwise.
func (session *Session) SetComputeReservationState(ctx context.Context, organizationID, reservationID string, state scheduler.ReservationState, failureReason string) (scheduler.Reservation, error) {
	input := struct {
		State scheduler.ReservationState `json:"state"`
		Error string                     `json:"error,omitempty"`
	}{State: state, Error: failureReason}
	var result scheduler.Reservation
	return result, session.call(ctx, "POST", organizationPath(organizationID, "/compute/reservations/"+url.PathEscape(reservationID)+"/state"), input, &result)
}
