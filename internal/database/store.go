package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sort"
	"strconv"
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
	// GitHubID and GitHubLogin record the GitHub identity linked to this
	// account ("Sign in with GitHub"). GitHubID is nil until the account is
	// linked (or created through GitHub); GitHubLogin is the handle at link
	// time and exists for display and erasure, never as a key.
	GitHubID    *int64
	GitHubLogin string
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
	LocalAgentURL  string
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
	Route                string
	RelayEndpoint        string
	Status               string
	Progress             json.RawMessage
	ErrorMessage         string
	CreatedBy            string
	IdempotencyKey       string
	AttemptCount         int
	LeaseOwner           string
	LeaseExpiresAt       *time.Time
	AgentMigrationID     string
	Managed              bool
	CancelRequested      bool
	TimeoutSeconds       int
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
	// Seats is the purchased seat quantity of a per-seat subscription. It
	// bounds organization membership and mirrors the Stripe subscription
	// item quantity. Zero means "no seat purchase recorded" and enforces no
	// ceiling.
	Seats int
	// StripeItemID is the subscription item whose quantity Seats mirrors; the
	// seat sync updates that item server-side on member changes.
	StripeItemID string
	// TrialExpiresAt is set while a 14-day no-card Business trial runs (or
	// ran): reads compare it to now() so expiry needs no webhook and fails
	// closed like Active(). Nil means no trial.
	TrialExpiresAt *time.Time
	// OverageCapCents is the monthly overage spend cap (0 = plan default).
	// New storage issuance stops at the cap; nothing is ever deleted and
	// nothing is ever billed above it.
	OverageCapCents int64
	// Interval is the billing interval of the active subscription ("month"
	// or "year"), recorded at checkout for display and reconciliation.
	Interval string
	// SeatsIncluded mirrors the plan's included-seat count at webhook time.
	SeatsIncluded int
	// SourceGuard and Failover record add-on entitlements the subscription
	// carries (SourceGuard Assurance, HA Pack).
	SourceGuard bool
	Failover    bool
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

type OwnershipRecord struct {
	WorkloadID     string
	OrganizationID string
	Epoch          uint64
	OwnerMachineID string
	State          string
	LeaseExpiresAt *time.Time
	UpdatedAt      time.Time
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
	return OpenWithConfig(ctx, configuration)
}

// OpenWithConfig opens a store from an already-parsed pool configuration —
// tests use it to point at per-test databases without rebuilding URLs.
func OpenWithConfig(ctx context.Context, configuration *pgxpool.Config) (*Store, error) {
	applyPoolEnvironment(configuration)
	// Every session runs in UTC unless the caller pinned another zone
	// explicitly. usage_records stores DATE columns and window/day-bucket
	// boundaries are derived from instants, so a session TimeZone other than
	// UTC shifts month boundaries and day gauges at their edges. The value is
	// a startup parameter, so it costs no round trip; a TimeZone already
	// present in the configuration (tests proving timezone independence)
	// wins.
	if configuration.ConnConfig.RuntimeParams == nil {
		configuration.ConnConfig.RuntimeParams = map[string]string{}
	}
	if configuration.ConnConfig.RuntimeParams["TimeZone"] == "" {
		configuration.ConnConfig.RuntimeParams["TimeZone"] = "UTC"
	}
	statementTimeout, lockTimeout := queryTimeoutsFromEnvironment()
	configuration.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		// Bounds run per session: a blocked retention delete or a lock-heavy
		// checkpoint transaction can no longer pin a pool connection past the
		// configured budget. Migration sessions raise both again and restore
		// the defaults before the connection is pooled (see migrateDirection).
		if statementTimeout > 0 {
			if _, err := conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = %d", statementTimeout.Milliseconds())); err != nil {
				return err
			}
		}
		if lockTimeout > 0 {
			if _, err := conn.Exec(ctx, fmt.Sprintf("SET lock_timeout = %d", lockTimeout.Milliseconds())); err != nil {
				return err
			}
		}
		return nil
	}
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

// applyPoolEnvironment sizes the pool from SHIFT_DB_* variables so a large
// host is not held to one hardcoded connection budget. Invalid values are
// ignored in favor of the defaults rather than refusing to start.
func applyPoolEnvironment(configuration *pgxpool.Config) {
	configuration.MaxConns = 20
	configuration.MinConns = 1
	configuration.MaxConnIdleTime = 5 * time.Minute
	configuration.MaxConnLifetime = time.Hour
	if value := os.Getenv("SHIFT_DB_MAX_CONNS"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed >= 1 {
			configuration.MaxConns = int32(parsed)
		}
	}
	if value := os.Getenv("SHIFT_DB_MIN_CONNS"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed >= 0 {
			configuration.MinConns = int32(parsed)
		}
	}
	if value := os.Getenv("SHIFT_DB_MAX_CONN_LIFETIME"); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			configuration.MaxConnLifetime = parsed
		}
	}
	if value := os.Getenv("SHIFT_DB_MAX_CONN_IDLE_TIME"); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			configuration.MaxConnIdleTime = parsed
		}
	}
}

// queryTimeoutsFromEnvironment resolves the per-session statement and lock
// budgets. Defaults keep ordinary queries bounded while leaving anything
// disabled explicitly ("0") alone.
func queryTimeoutsFromEnvironment() (statement, lock time.Duration) {
	statement = 60 * time.Second
	lock = 10 * time.Second
	if value := os.Getenv("SHIFT_DB_STATEMENT_TIMEOUT"); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed >= 0 {
			statement = parsed
		}
	}
	if value := os.Getenv("SHIFT_DB_LOCK_TIMEOUT"); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed >= 0 {
			lock = parsed
		}
	}
	return statement, lock
}

// PoolStats is a point-in-time snapshot of pool saturation, exported on
// /metrics so connection exhaustion is visible before requests start failing.
type PoolStats struct {
	MaxConns        int32
	TotalConns      int32
	IdleConns       int32
	AcquireCount    int64
	AcquireDuration time.Duration
}

func (store *Store) PoolStats() PoolStats {
	stats := store.pool.Stat()
	return PoolStats{
		MaxConns:        stats.MaxConns(),
		TotalConns:      stats.TotalConns(),
		IdleConns:       stats.IdleConns(),
		AcquireCount:    stats.AcquireCount(),
		AcquireDuration: stats.AcquireDuration(),
	}
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
// migration rolls back and aborts the apply — the control plane keeps serving
// against the schema it has while readiness reports the failure, so traffic
// only stays off a stale schema when the load balancer gates on /ready.
// migrationLockKey identifies the schema-migration advisory lock. Two
// processes migrating one database at once — go test runs package binaries
// in parallel and several packages run database-backed suites against the
// same test database — would each see "not applied" before either commits,
// then race the apply into catalog collisions. The lock serializes
// migrators; the one that arrives second finds every migration applied and
// returns without touching the schema.
const migrationLockKey int64 = 0x7368696674 // "shift"

// migrationStatement is one statement lifted from a migration file, with the
// transaction policy its preceding directives asked for.
type migrationStatement struct {
	sql string
	// noTransaction marks statements that cannot run inside a transaction
	// block — CREATE INDEX CONCURRENTLY above all — so the runner executes
	// them on the migration session directly. A crash mid-statement is safe:
	// the file is only recorded as applied once every statement succeeds.
	noTransaction bool
}

func (store *Store) Migrate(ctx context.Context) error {
	return store.migrateDirection(ctx, false)
}

// MigrateDown reverts the last steps applied migrations in reverse name
// order, each using its `NNN_name.down.sql` companion file. A migration with
// no down file cannot be reverted: refusing is the only honest answer to a
// schema change whose inverse was never written.
func (store *Store) MigrateDown(ctx context.Context, steps int) error {
	if steps < 1 {
		return errors.New("migration rollback steps must be positive")
	}
	return store.migrateDirection(ctx, true, steps)
}

func (store *Store) migrateDirection(ctx context.Context, down bool, steps ...int) (retErr error) {
	// A session-level advisory lock needs one dedicated connection: taking
	// and releasing it over separate pool round-trips would try to unlock
	// from a session that never locked.
	conn, err := store.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	locked := false
	defer func() {
		// Session cleanup runs before the connection returns to the pool, on
		// a bounded context of its own — a canceled caller must not leave the
		// lock held on a pooled session, and a half-open TCP session must not
		// wedge the migrator forever. pgxpool does not reset session state on
		// release, so the timeouts this session raised for schema changes are
		// restored here or the next request routed to this connection runs
		// with no budget at all. Any cleanup failure destroys the connection
		// instead of returning it: a destroyed session drops its advisory
		// lock at disconnect and never serves request traffic with poisoned
		// state, and the failure is folded into the returned error so it is
		// logged (with cause) rather than swallowed. It is reported even when
		// the apply itself succeeded — the session could not be released
		// cleanly — and a retry is idempotent because every migration is
		// already recorded as applied.
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if locked {
			if _, unlockErr := conn.Exec(cleanupContext, `SELECT pg_advisory_unlock($1)`, migrationLockKey); unlockErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("pg_advisory_unlock: %w", unlockErr))
				conn.Conn().Close(cleanupContext)
				conn.Release()
				return
			}
		}
		if _, resetErr := conn.Exec(cleanupContext, `SET statement_timeout = DEFAULT; SET lock_timeout = DEFAULT`); resetErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("restore migration session timeouts: %w", resetErr))
			conn.Conn().Close(cleanupContext)
		}
		conn.Release()
	}()
	// Schema changes may legitimately exceed the per-session query budgets
	// that protect request traffic (a concurrent index build on a large table
	// runs for minutes), so the migration session raises both again. The
	// cleanup above restores them before the connection is pooled again.
	if _, err := conn.Exec(ctx, `SET statement_timeout = 0; SET lock_timeout = 0`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return err
	}
	locked = true
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(schemamigrations.Files, "migrations")
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || strings.HasSuffix(entry.Name(), ".down.sql") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if down {
		return store.revertMigrations(ctx, conn, names, steps[0])
	}
	for _, name := range names {
		var applied bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		content, err := schemamigrations.Files.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if err := runMigrationStatements(ctx, conn, splitSQL(string(content)), `INSERT INTO schema_migrations(name) VALUES($1)`, name); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

// runMigrationStatements executes one migration file's statements: plain
// statements run in transaction blocks, while no-transaction statements
// (CREATE INDEX CONCURRENTLY) run on the migration session between those
// blocks. A no-transaction statement commits the open block first — the DDL
// before it is part of the file and must survive — then executes outside any
// transaction. The bookkeeping statement — recording or reverting the file in
// schema_migrations — commits with the file's final block, so a failed
// migration is never marked applied and a failed revert is never marked
// reverted. Blocks are rolled back only on their own failure; idempotent
// DDL (IF NOT EXISTS and friends) makes a retry after a failed concurrent
// build safe.
func runMigrationStatements(ctx context.Context, conn *pgxpool.Conn, statements []migrationStatement, bookkeeping string, bookkeepingArgs ...any) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	open := true
	rollback := func() {
		if open {
			_ = tx.Rollback(ctx)
			open = false
		}
	}
	commitBlock := func() error {
		if open {
			if err := tx.Commit(ctx); err != nil {
				return err
			}
			open = false
		}
		return nil
	}
	for _, statement := range statements {
		if statement.noTransaction {
			// Everything before a concurrent statement is ordinary migration
			// DDL and must commit with the file: rolling the block back here
			// would silently lose it while the file is still recorded applied.
			if err := commitBlock(); err != nil {
				return err
			}
			if _, err := conn.Exec(ctx, statement.sql); err != nil {
				return err
			}
			continue
		}
		if !open {
			tx, err = conn.BeginTx(ctx, pgx.TxOptions{})
			if err != nil {
				return err
			}
			open = true
		}
		if _, err := tx.Exec(ctx, statement.sql); err != nil {
			rollback()
			return err
		}
	}
	if !open {
		tx, err = conn.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return err
		}
		open = true
	}
	if _, err := tx.Exec(ctx, bookkeeping, bookkeepingArgs...); err != nil {
		rollback()
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	open = false
	return nil
}

// revertMigrations rolls back up to steps applied migrations, newest first,
// using each file's down companion.
func (store *Store) revertMigrations(ctx context.Context, conn *pgxpool.Conn, names []string, steps int) error {
	applied := make(map[string]bool, len(names))
	rows, err := conn.Query(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		applied[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	reverted := 0
	for index := len(names) - 1; index >= 0 && reverted < steps; index-- {
		name := names[index]
		if !applied[name] {
			continue
		}
		downName := strings.TrimSuffix(name, ".sql") + ".down.sql"
		content, err := schemamigrations.Files.ReadFile("migrations/" + downName)
		if err != nil {
			return fmt.Errorf("migration %s has no down file %s: %w", name, downName, err)
		}
		if err := runMigrationStatements(ctx, conn, splitSQL(string(content)), `DELETE FROM schema_migrations WHERE name=$1`, name); err != nil {
			return fmt.Errorf("revert migration %s: %w", name, err)
		}
		reverted++
	}
	return nil
}

// MigrateStatus reports how many embedded migrations are still unapplied.
// Nothing consumes it automatically: readiness tracks the in-memory schema
// state the background apply writes, and metrics use the
// schema_migration_failed gauge. It is operator tooling for schema-lag checks
// that must not block on the apply itself — one round trip, not one per file.
func (store *Store) MigrateStatus(ctx context.Context) (pending int, err error) {
	conn, err := store.pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return 0, err
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return 0, err
	}
	entries, err := fs.ReadDir(schemamigrations.Files, "migrations")
	if err != nil {
		return 0, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || strings.HasSuffix(entry.Name(), ".down.sql") {
			continue
		}
		names = append(names, entry.Name())
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM unnest($1::text[]) AS n(name)
		WHERE NOT EXISTS (SELECT 1 FROM schema_migrations m WHERE m.name = n.name)`, names).Scan(&pending); err != nil {
		return 0, err
	}
	return pending, nil
}

// splitSQL lifts individual statements out of a migration file. It is a real
// (if small) SQL scanner: semicolons inside single-quoted strings, quoted
// identifiers, dollar-quoted bodies ($$ … $$ / $tag$ … $tag$), line comments,
// and nested block comments never split a statement, and a trailing inline
// comment after a semicolon cannot become a bogus statement of its own.
// Escape-string literals (E'…\';…') and Unicode strings (U&'…') honor
// backslash escapes, COPY … FROM stdin data blocks stay attached to their
// statement until the terminating \. line, and a $n$ token that merely looks
// like a dollar tag (a digit cannot start a tag) is plain text. A
// `-- shift:no-transaction` directive on the line(s) before a statement marks
// it for execution outside any transaction block.
func splitSQL(content string) []migrationStatement {
	var statements []migrationStatement
	var current strings.Builder
	noTransaction := false
	inCopyData := false
	runes := []rune(content)
	index := 0
	flush := func() {
		sql := strings.TrimSpace(current.String())
		current.Reset()
		if sql == "" {
			return
		}
		statements = append(statements, migrationStatement{sql: sql, noTransaction: noTransaction})
		noTransaction = false
	}
	for index < len(runes) {
		char := runes[index]
		switch {
		case inCopyData:
			// COPY data is opaque: semicolons and quotes are payload. The
			// block ends at the \. line, which joins the statement it belongs
			// to so the whole COPY executes (and splits) as one unit.
			lineStart := index
			for index < len(runes) && runes[index] != '\n' {
				index++
			}
			current.WriteString(string(runes[lineStart:index]))
			if index < len(runes) {
				current.WriteRune('\n')
				index++
			}
			line := strings.TrimSpace(strings.TrimSuffix(string(runes[lineStart:index]), "\n"))
			line = strings.TrimSuffix(line, "\r")
			if line == "\\." {
				inCopyData = false
				flush()
			}
		case char == '-' && index+1 < len(runes) && runes[index+1] == '-':
			// Line comment: scan to end of line. The directive is recognized
			// only when the statement buffer is still empty (or already holds
			// a directive-only gap), so a trailing comment cannot re-flag the
			// statement it follows.
			end := index + 2
			for end < len(runes) && runes[end] != '\n' {
				end++
			}
			comment := strings.TrimSpace(string(runes[index+2 : end]))
			if comment == "shift:no-transaction" && strings.TrimSpace(current.String()) == "" {
				noTransaction = true
			}
			index = end
		case char == '/' && index+1 < len(runes) && runes[index+1] == '*':
			depth := 1
			index += 2
			for index < len(runes) && depth > 0 {
				if runes[index] == '/' && index+1 < len(runes) && runes[index+1] == '*' {
					depth++
					index += 2
					continue
				}
				if runes[index] == '*' && index+1 < len(runes) && runes[index+1] == '/' {
					depth--
					index += 2
					continue
				}
				index++
			}
		case char == '\'':
			_, backslashEscapes := stringLiteralEscapes(runes, index)
			current.WriteRune(char)
			index++
			for index < len(runes) {
				if backslashEscapes && runes[index] == '\\' && index+1 < len(runes) {
					current.WriteRune(runes[index])
					current.WriteRune(runes[index+1])
					index += 2
					continue
				}
				current.WriteRune(runes[index])
				if runes[index] == '\'' {
					if index+1 < len(runes) && runes[index+1] == '\'' {
						current.WriteRune(runes[index+1])
						index += 2
						continue
					}
					index++
					break
				}
				index++
			}
		case char == '"':
			current.WriteRune(char)
			index++
			for index < len(runes) {
				current.WriteRune(runes[index])
				if runes[index] == '"' {
					index++
					break
				}
				index++
			}
		case char == '$':
			if tag, length, ok := dollarQuoteTag(runes[index:]); ok {
				current.WriteString(tag)
				index += length
				for index < len(runes) {
					current.WriteRune(runes[index])
					if runes[index] == '$' && index+length <= len(runes) && string(runes[index:index+length]) == tag {
						current.WriteString(tag[1:])
						index += length
						break
					}
					index++
				}
				continue
			}
			current.WriteRune(char)
			index++
		case char == ';':
			index++
			if isCopyFromStdin(current.String() + ";") {
				// The semicolon ends the COPY header, not the statement: the
				// data block follows and stays attached until its \. line.
				current.WriteRune(';')
				inCopyData = true
				continue
			}
			flush()
		default:
			current.WriteRune(char)
			index++
		}
	}
	flush()
	return statements
}

// stringLiteralEscapes reports whether the quote at runes[quoteAt] opens an
// escape-string (E'…') or Unicode (U&'…') literal, whose backslashes escape
// the character that follows (so \';…' is one string, not a split).
func stringLiteralEscapes(runes []rune, quoteAt int) (prefixLen int, ok bool) {
	if quoteAt >= 1 && (runes[quoteAt-1] == 'E' || runes[quoteAt-1] == 'e') {
		return 1, true
	}
	if quoteAt >= 2 && runes[quoteAt-1] == '&' && (runes[quoteAt-2] == 'U' || runes[quoteAt-2] == 'u') {
		return 2, true
	}
	return 0, false
}

// isCopyFromStdin reports whether the statement just ended with a semicolon
// is a COPY … FROM stdin header, whose data block follows the semicolon.
func isCopyFromStdin(statement string) bool {
	trimmed := strings.TrimSuffix(strings.TrimSpace(statement), ";")
	trimmed = strings.TrimSpace(trimmed)
	if len(trimmed) < 10 || !strings.EqualFold(trimmed[:4], "copy") {
		return false
	}
	return strings.HasSuffix(strings.ToLower(trimmed), " from stdin")
}

// dollarQuoteTag recognizes a dollar-quote opener like $$ or $body$ and
// returns the full tag and its rune length. Tags follow identifier rules — a
// letter or underscore first — so $1$2 is plain text ($1$2), never a quoted
// body that swallows the rest of the file.
func dollarQuoteTag(runes []rune) (string, int, bool) {
	if len(runes) < 2 || runes[0] != '$' {
		return "", 0, false
	}
	if runes[1] == '$' {
		return "$$", 2, true
	}
	if !isDollarTagStart(runes[1]) {
		return "", 0, false
	}
	for index := 2; index < len(runes); index++ {
		if runes[index] == '$' {
			return string(runes[:index+1]), index + 1, true
		}
		if !isDollarTagPart(runes[index]) {
			return "", 0, false
		}
	}
	return "", 0, false
}

// isDollarTagStart matches the first character of a dollar-quote tag
// (unquoted-identifier rules: a letter or underscore).
func isDollarTagStart(char rune) bool {
	return char == '_' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z'
}

// isDollarTagPart matches the continuation characters of a dollar-quote tag.
func isDollarTagPart(char rune) bool {
	return isDollarTagStart(char) || char >= '0' && char <= '9'
}

// Page bounds a list query. Every list query in this package takes one, so no
// endpoint can ask a table to materialize itself into memory: the default
// page is the documented API page, and the cap is a hard server-side limit no
// request can raise.
type Page struct {
	Limit  int
	Offset int
}

const (
	DefaultPageLimit = 100
	MaxPageLimit     = 500
)

// NormalizePage clamps caller-supplied bounds to the server's own.
func NormalizePage(limit, offset int) Page {
	if limit <= 0 {
		limit = DefaultPageLimit
	}
	if limit > MaxPageLimit {
		limit = MaxPageLimit
	}
	if offset < 0 {
		offset = 0
	}
	return Page{Limit: limit, Offset: offset}
}

// clause renders the LIMIT/OFFSET fragment. The values are ints normalized by
// NormalizePage — never caller text — so they enter SQL as literals and no
// bind-position bookkeeping leaks into list queries.
func (p Page) clause() string {
	// A zero-value Page still behaves like the documented default, so a
	// forgotten NormalizePage bounds the query instead of emptying it.
	normalized := NormalizePage(p.Limit, p.Offset)
	return fmt.Sprintf(" LIMIT %d OFFSET %d", normalized.Limit, normalized.Offset)
}

// insertAudit writes one audit row inside the caller's transaction, so a
// mutation and its audit trail commit or roll back together.
func insertAudit(ctx context.Context, tx pgx.Tx, input AuditInput) error {
	// A nameless audit row is not an audit row: callers that have nothing to
	// record pass a zero input, which must never fail the transaction it
	// rides (an empty id collides with every other empty id).
	if input.ID == "" || input.Action == "" {
		return nil
	}
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
var ErrIdempotencyConflict = errors.New("idempotency key was already used with a different operation")
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
