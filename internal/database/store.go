package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	schemamigrations "shift.dev/shift/database"
)

// This file holds the Store, its connection lifecycle, the record types every
// query returns, and the small helpers every query file shares (audit inserts,
// error classification, value shaping). Domain queries live beside it:
// users.go, sessions.go, organizations.go, machines.go, workloads.go,
// migrations.go, checkpoints.go, apikeys.go, entitlements.go, audit.go,
// usage.go.

type Store struct {
	pool *pgxpool.Pool
}

type UserRecord struct {
	ID           string
	Email        string
	PasswordHash string
	DisplayName  string
	DisabledAt   *time.Time
	CreatedAt    time.Time
	// ExternalID is the SCIM id the provisioning system knows this user by;
	// empty for locally registered accounts.
	ExternalID string
	// SSOIssuer and SSOSubject record which identity provider authenticated
	// this account into existence; both empty for password accounts.
	SSOIssuer  string
	SSOSubject string
}

type OrganizationRecord struct {
	ID        string
	Name      string
	Role      string
	CreatedAt time.Time
}

type SessionRecord struct {
	ID               string
	UserID           string
	Email            string
	DisplayName      string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

type MachineRecord struct {
	ID             string
	OrganizationID string
	MachineID      string
	Name           string
	AgentURL       string
	Capabilities   json.RawMessage
	Status         string
	LastSeenAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type WorkloadRecord struct {
	ID             string
	OrganizationID string
	MachineID      string
	Name           string
	Spec           json.RawMessage
	Status         json.RawMessage
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type MigrationRecord struct {
	ID                   string
	OrganizationID       string
	WorkloadID           string
	SourceMachineID      string
	DestinationMachineID string
	Mode                 string
	Status               string
	Progress             json.RawMessage
	ErrorMessage         string
	CreatedBy            string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	CompletedAt          *time.Time
}

type EntitlementRecord struct {
	OrganizationID       string
	Plan                 string
	Status               string
	MaxStorageBytes      int64
	UsedStorageBytes     int64
	MaxMachines          int
	StripeCustomerID     string
	StripeSubscriptionID string
}

type AuditRecord struct {
	ID             string
	OrganizationID string
	ActorUserID    string
	Action         string
	ResourceType   string
	ResourceID     string
	Metadata       json.RawMessage
	RequestID      string
	RemoteAddr     string
	CreatedAt      time.Time
}

type APIKeyRecord struct {
	ID             string
	OrganizationID string
	UserID         string
	Name           string
	Prefix         string
	Scopes         json.RawMessage
	ExpiresAt      *time.Time
	LastUsedAt     *time.Time
	RevokedAt      *time.Time
	CreatedAt      time.Time
}

type CheckpointRecord struct {
	ID             string
	OrganizationID string
	WorkloadID     string
	MachineID      string
	Kind           string
	ParentID       string
	Manifest       json.RawMessage
	PlainBytes     int64
	StoredBytes    int64
	ChunkCount     int
	Status         string
	CreatedAt      time.Time
	DeletedAt      *time.Time
}

type MigrationEventRecord struct {
	ID          string
	MigrationID string
	Sequence    int64
	Stage       string
	Message     string
	Progress    float64
	BytesDone   int64
	BytesTotal  int64
	CreatedAt   time.Time
}

type UsageRecord struct {
	ID             string
	OrganizationID string
	Kind           string
	Quantity       int64
	PeriodStart    time.Time
	PeriodEnd      time.Time
	Metadata       json.RawMessage
	CreatedAt      time.Time
}

type AuditInput struct {
	ID             string
	OrganizationID string
	ActorUserID    string
	Action         string
	ResourceType   string
	ResourceID     string
	Metadata       any
	RequestID      string
	RemoteAddr     string
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	configuration.MaxConns = 20
	configuration.MinConns = 1
	configuration.MaxConnIdleTime = 5 * time.Minute
	configuration.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	store := &Store{pool: pool}
	if err := store.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) Close() { store.pool.Close() }

func (store *Store) Ping(ctx context.Context) error {
	if err := store.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// Migrate applies every pending migration under database/migrations in name
// order, each inside one transaction recorded in schema_migrations. A failing
// migration rolls back and aborts startup — the control plane refuses to run
// against a schema it does not know.
func (store *Store) Migrate(ctx context.Context) error {
	if _, err := store.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(schemamigrations.Files, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		var applied bool
		if err := store.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name=$1)`, entry.Name()).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		content, err := schemamigrations.Files.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return err
		}
		failed := false
		for _, statement := range splitSQL(string(content)) {
			if _, err := tx.Exec(ctx, statement); err != nil {
				_ = tx.Rollback(ctx)
				failed = true
				return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
			}
		}
		if !failed {
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES($1)`, entry.Name()); err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
			if err := tx.Commit(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// splitSQL strips SQL comments and splits a migration file into individual
// statements — the embedded SQL runner has no multi-statement support of its
// own.
func splitSQL(content string) []string {
	lines := strings.Split(content, "\n")
	var cleaned strings.Builder
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		cleaned.WriteString(line)
		cleaned.WriteByte('\n')
	}
	parts := strings.Split(cleaned.String(), ";")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if statement := strings.TrimSpace(part); statement != "" {
			result = append(result, statement)
		}
	}
	return result
}

// insertAudit writes one audit row inside the caller's transaction, so a
// mutation and its audit trail commit or roll back together.
func insertAudit(ctx context.Context, tx pgx.Tx, input AuditInput) error {
	metadata, err := json.Marshal(input.Metadata)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(id,organization_id,actor_user_id,action,resource_type,resource_id,metadata,request_id,remote_addr)
		VALUES($1,NULLIF($2,''),NULLIF($3,''),$4,$5,$6,$7,$8,$9)`, input.ID, input.OrganizationID, input.ActorUserID,
		input.Action, input.ResourceType, input.ResourceID, metadata, input.RequestID, nullableIP(input.RemoteAddr))
	return err
}

var ErrEntitlementExceeded = errors.New("organization machine entitlement is exhausted")
var ErrStorageEntitlementExceeded = errors.New("organization checkpoint storage entitlement is exhausted")
var ErrEntitlementMissing = errors.New("organization entitlement row was not found")
var ErrOwnerRoleImmutable = errors.New("organization owner role cannot be changed through the member endpoint")
var ErrCheckpointMachineMismatch = errors.New("checkpoint machine does not match the workload assignment")
var ErrCheckpointParentInvalid = errors.New("incremental checkpoint parent is unavailable or belongs to another workload")

func IsConflict(err error) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) && databaseError.Code == "23505"
}

func IsNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func nullableIP(remoteAddr string) any {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		remoteAddr = host
	}
	if net.ParseIP(remoteAddr) == nil {
		return nil
	}
	return remoteAddr
}

func truncate(value string, length int) string {
	if len(value) <= length {
		return value
	}
	return value[:length]
}
