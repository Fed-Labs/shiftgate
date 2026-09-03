package controlplane

import "time"

type Role string

const (
	RoleOwner    Role = "owner"
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

func (role Role) Valid() bool {
	switch role {
	case RoleOwner, RoleAdmin, RoleOperator, RoleViewer:
		return true
	default:
		return false
	}
}

func (role Role) Allows(required Role) bool {
	levels := map[Role]int{RoleViewer: 1, RoleOperator: 2, RoleAdmin: 3, RoleOwner: 4}
	return levels[role] >= levels[required] && role.Valid() && required.Valid()
}

type Principal struct {
	SessionID      string   `json:"session_id"`
	UserID         string   `json:"user_id"`
	Email          string   `json:"email"`
	DisplayName    string   `json:"display_name"`
	OrganizationID string   `json:"organization_id,omitempty"`
	Role           Role     `json:"role,omitempty"`
	APIKeyID       string   `json:"api_key_id,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
}

type User struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	DisplayName string     `json:"display_name"`
	DisabledAt  *time.Time `json:"disabled_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type Organization struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

type SessionTokens struct {
	SessionID    string    `json:"session_id"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
}

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

type Entitlement struct {
	OrganizationID   string `json:"organization_id"`
	Plan             string `json:"plan"`
	Status           string `json:"status"`
	MaxStorageBytes  int64  `json:"max_storage_bytes"`
	UsedStorageBytes int64  `json:"used_storage_bytes"`
	MaxMachines      int    `json:"max_machines"`
	StripeCustomerID string `json:"stripe_customer_id,omitempty"`
}

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

type APIKeyCreated struct {
	APIKey
	Secret string `json:"secret"`
}

type CreateAPIKeyRequest struct {
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

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

type AgentCommandRequest struct {
	Action         string         `json:"action"`
	WorkloadID     string         `json:"workload_id,omitempty"`
	CheckpointID   string         `json:"checkpoint_id,omitempty"`
	DestinationID  string         `json:"destination_id,omitempty"`
	Mode           string         `json:"mode,omitempty"`
	TimeoutSeconds int            `json:"timeout_seconds,omitempty"`
	Spec           map[string]any `json:"spec,omitempty"`
}

type AgentCommandResponse struct {
	Machine    string        `json:"machine"`
	Action     string        `json:"action"`
	Result     any           `json:"result"`
	Workload   *Workload     `json:"workload,omitempty"`
	Checkpoint *Checkpoint   `json:"checkpoint,omitempty"`
	Migration  *MigrationJob `json:"migration,omitempty"`
}

type CreateCheckpointRequest struct {
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

func (request CreateCheckpointRequest) ManifestOrEmpty() map[string]any {
	if request.Manifest == nil {
		return map[string]any{}
	}
	return request.Manifest
}

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

type UsageSummary struct {
	Kind        string `json:"kind"`
	Quantity    int64  `json:"quantity"`
	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
}

// RetentionPolicyResponse is one organization's retention windows, in days.
// Zero is a real answer — keep forever — so every field is always present.
type RetentionPolicyResponse struct {
	OrganizationID              string    `json:"organization_id"`
	AuditRetentionDays          int       `json:"audit_retention_days"`
	CheckpointRetentionDays     int       `json:"checkpoint_retention_days"`
	DeletedStorageRetentionDays int       `json:"deleted_storage_retention_days"`
	UpdatedAt                   time.Time `json:"updated_at"`
}

// SetRetentionPolicyRequest replaces an organization's retention windows.
// Each field is a pointer because omitted means "leave unchanged" while zero
// means "keep forever" — collapsing the two would make the keep-forever policy
// impossible to set without also resetting the other windows.
type SetRetentionPolicyRequest struct {
	AuditRetentionDays          *int `json:"audit_retention_days,omitempty"`
	CheckpointRetentionDays     *int `json:"checkpoint_retention_days,omitempty"`
	DeletedStorageRetentionDays *int `json:"deleted_storage_retention_days,omitempty"`
}

type RegisterRequest struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	DisplayName  string `json:"display_name"`
	Organization string `json:"organization"`
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type CreateMachineRequest struct {
	MachineID    string         `json:"machine_id"`
	Name         string         `json:"name"`
	AgentURL     string         `json:"agent_url"`
	Capabilities map[string]any `json:"capabilities"`
}

type CreateWorkloadRequest struct {
	MachineID string         `json:"machine_id"`
	Name      string         `json:"name"`
	Spec      map[string]any `json:"spec"`
	Status    map[string]any `json:"status,omitempty"`
}

type CreateMigrationRequest struct {
	WorkloadID           string `json:"workload_id"`
	SourceMachineID      string `json:"source_machine_id"`
	DestinationMachineID string `json:"destination_machine_id"`
	Mode                 string `json:"mode"`
}

type ErrorResponse struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}
