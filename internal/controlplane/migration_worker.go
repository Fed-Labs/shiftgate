package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/persistence"
)

const (
	migrationWorkerTick  = 2 * time.Second
	migrationWorkerLease = 45 * time.Second
	migrationRetryDelay  = 10 * time.Second
	// unmanagedSettleTick drives the settlement pass for non-managed jobs
	// (agent-command dispatch and the relay route): they have no lease the
	// managed queue would follow, so they settle on their own cadence.
	unmanagedSettleTick = 5 * time.Second
	// unmanagedJobAbandonAge settles a watched job the agent never started.
	// Past this age there is no agent-side record to reconcile and the job
	// must not sit "queued" forever. It matches the command delivery TTL.
	unmanagedJobAbandonAge = 24 * time.Hour
)

// unmanagedMigrationWatch is one non-managed migration job the settlement
// pass must follow to a terminal state.
type unmanagedMigrationWatch struct {
	OrganizationID string
	MigrationID    string
	AddedAt        time.Time
}

// watchUnmanagedMigration registers a non-managed migration job for
// settlement. Jobs are registered when they are created (dispatch), when a
// relay ticket arms them, and when their driving command is claimed, so the
// watch is re-seeded after a control-plane restart by normal agent traffic.
func (server *Server) watchUnmanagedMigration(organizationID, migrationID string) {
	if organizationID == "" || migrationID == "" {
		return
	}
	server.settleMu.Lock()
	defer server.settleMu.Unlock()
	if server.unmanagedMigrations == nil {
		server.unmanagedMigrations = map[string]unmanagedMigrationWatch{}
	}
	key := organizationID + "/" + migrationID
	if _, ok := server.unmanagedMigrations[key]; !ok {
		server.unmanagedMigrations[key] = unmanagedMigrationWatch{OrganizationID: organizationID, MigrationID: migrationID, AddedAt: time.Now().UTC()}
	}
}

// forgetUnmanagedMigration drops a settled job from the watch set.
func (server *Server) forgetUnmanagedMigration(organizationID, migrationID string) {
	server.settleMu.Lock()
	defer server.settleMu.Unlock()
	delete(server.unmanagedMigrations, organizationID+"/"+migrationID)
}

// unmanagedMigrationWatches snapshots the current watch set.
func (server *Server) unmanagedMigrationWatches() []unmanagedMigrationWatch {
	server.settleMu.Lock()
	defer server.settleMu.Unlock()
	out := make([]unmanagedMigrationWatch, 0, len(server.unmanagedMigrations))
	for _, watch := range server.unmanagedMigrations {
		out = append(out, watch)
	}
	return out
}

// registerCommandMigration adopts the migration a claimed command drives.
// The relay commands carry the job id (relay.join payload migration_id,
// relay.start payload idempotency_key), which re-seeds the watch after a
// restart even when the job was created on another replica.
func (server *Server) registerCommandMigration(organizationID string, command database.AgentCommandRecord) {
	if command.Action != "relay.start" && command.Action != "relay.join" {
		return
	}
	var payload struct {
		MigrationID    string `json:"migration_id"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		return
	}
	migrationID := payload.MigrationID
	if migrationID == "" {
		migrationID = payload.IdempotencyKey
	}
	server.watchUnmanagedMigration(organizationID, migrationID)
}

// runMigrationWorker is a small durable coordinator. The database owns the
// queue and lease; agents own the actual state transfer. If this process dies
// after an agent accepts a job, the next worker replays the same idempotency key
// or polls the persisted agent migration id instead of starting a second move.
// Execution is concurrent up to the configured bound: one long cold migration
// holds its own slot and lease, never the queue behind it.
func (server *Server) runMigrationWorker(ctx context.Context) {
	owner, err := model.NewID()
	if err != nil {
		server.logger.Error("migration worker could not create owner id", "error", err)
		return
	}
	owner = "control-worker-" + owner
	concurrency := server.config.MigrationWorkerConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	slots := make(chan struct{}, concurrency)
	var workers sync.WaitGroup
	ticker := time.NewTicker(migrationWorkerTick)
	defer ticker.Stop()
	defer workers.Wait()
	workers.Add(1)
	go func() {
		defer workers.Done()
		server.runUnmanagedSettler(ctx)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case slots <- struct{}{}:
		}
		job, ok := server.claimNextMigration(ctx, owner)
		if !ok {
			<-slots
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			continue
		}
		workers.Add(1)
		go func(job database.MigrationRecord) {
			defer workers.Done()
			defer func() { <-slots }()
			server.executeMigrationJob(ctx, owner, job)
		}(job)
	}
}

// claimNextMigration takes one lease off the durable queue.
func (server *Server) claimNextMigration(ctx context.Context, owner string) (database.MigrationRecord, bool) {
	job, err := server.database.ClaimNextManagedMigration(ctx, owner, migrationWorkerLease)
	if err != nil {
		if !errors.Is(err, persistence.ErrNotFound) && !errors.Is(err, context.Canceled) {
			server.logger.Warn("migration worker claim failed", "error", err)
		}
		return database.MigrationRecord{}, false
	}
	return job, true
}

// processOneMigration claims and runs one job synchronously. The worker loop
// runs jobs in goroutines; this entry point exists for tests and for embedded
// single-job execution.
func (server *Server) processOneMigration(ctx context.Context, owner string) bool {
	job, ok := server.claimNextMigration(ctx, owner)
	if !ok {
		return false
	}
	server.executeMigrationJob(ctx, owner, job)
	return true
}

func (server *Server) executeMigrationJob(ctx context.Context, owner string, job database.MigrationRecord) {
	sourceRecord, err := server.database.Machine(ctx, job.OrganizationID, job.SourceMachineID)
	if err != nil {
		server.retryMigrationJob(ctx, owner, job, fmt.Errorf("load source machine: %w", err))
		return
	}
	destinationRecord, err := server.database.Machine(ctx, job.OrganizationID, job.DestinationMachineID)
	if err != nil {
		server.retryMigrationJob(ctx, owner, job, fmt.Errorf("load destination machine: %w", err))
		return
	}
	source, destination := &sourceRecord, &destinationRecord
	if job.CancelRequested {
		server.finishMigrationJob(ctx, owner, job, "cancelled", model.Migration{}, "migration cancelled before dispatch")
		return
	}
	// Dispatch through the co-located Unix socket when one is configured —
	// the strongest boundary, since the socket never leaves the host — and
	// otherwise through the machine's remote agent URL, the same endpoint the
	// agent-command path already dispatches to. Remote dispatch keeps a
	// hosted control plane able to run managed migrations; it is a config
	// switch so deployments that require socket co-location can say so.
	endpoint := ""
	if server.allowedLocalAgentURL(source.LocalAgentURL) {
		endpoint = source.LocalAgentURL
	} else if server.config.AllowRemoteAgentDispatch && strings.TrimSpace(source.AgentURL) != "" {
		endpoint = strings.TrimRight(source.AgentURL, "/")
	}
	if endpoint == "" {
		if strings.TrimSpace(source.LocalAgentURL) != "" {
			server.finishMigrationJob(ctx, owner, job, "failed", model.Migration{}, "source local agent URL is outside the configured socket root and remote dispatch is disabled")
		} else {
			server.finishMigrationJob(ctx, owner, job, "failed", model.Migration{}, "source has no dispatchable agent endpoint (no local agent URL and remote dispatch is disabled or has no agent URL)")
		}
		return
	}
	client, err := server.newAgentClient(endpoint, migrationTimeout(job))
	if err != nil {
		server.finishMigrationJob(ctx, owner, job, "failed", model.Migration{}, fmt.Sprintf("source agent endpoint: %v", err))
		return
	}

	agentMigrationID := job.AgentMigrationID
	var result model.Migration
	if agentMigrationID == "" {
		result, err = client.CreateMigration(ctx, agentclient.MigrationCreateRequest{
			WorkloadID:     job.WorkloadID,
			IdempotencyKey: job.ID,
			Destination: model.Destination{
				MachineID:  destination.MachineID,
				AgentURL:   destination.AgentURL,
				ServerName: serverNameFromAgentURL(destination.AgentURL),
			},
			Mode:           migrationMode(job.Mode),
			TimeoutSeconds: job.TimeoutSeconds,
		})
		if err != nil {
			server.retryMigrationJob(ctx, owner, job, fmt.Errorf("create agent migration: %w", err))
			return
		}
		agentMigrationID = result.ID
		job.AgentMigrationID = agentMigrationID
	}

	if _, err := server.database.UpdateManagedMigration(ctx, job, "running", migrationProgressJSON(result), "", agentMigrationID, time.Now().UTC(), migrationWorkerLease); err != nil {
		server.logger.Warn("migration worker could not persist agent assignment", "migration_id", job.ID, "error", err)
		return
	}

	pollTicker := time.NewTicker(2 * time.Second)
	defer pollTicker.Stop()
	for {
		if fresh, refreshErr := server.database.Migration(ctx, job.OrganizationID, job.ID); refreshErr == nil {
			job = fresh
		}
		if job.CancelRequested {
			if err := client.CancelMigration(ctx, agentMigrationID); err != nil {
				server.retryMigrationJob(ctx, owner, job, fmt.Errorf("cancel agent migration: %w", err))
				return
			}
			server.finishMigrationJob(ctx, owner, job, "cancelled", result, "migration cancelled by operator")
			return
		}
		result, err = client.Migration(ctx, agentMigrationID)
		if err != nil {
			// Keep the lease while the agent is temporarily unavailable. A worker
			// restart can reclaim the running row after the lease expires.
			if _, updateErr := server.database.UpdateManagedMigration(ctx, job, "running", migrationProgressJSON(result), err.Error(), agentMigrationID, time.Now().UTC(), migrationWorkerLease); updateErr != nil {
				server.logger.Warn("migration worker could not renew agent lease", "migration_id", job.ID, "error", updateErr)
			}
		} else {
			server.recordMigrationEvents(ctx, job.ID, result)
			status := migrationJobStatus(result.Stage)
			if status == "completed" || status == "failed" || status == "cancelled" {
				server.finishMigrationJob(ctx, owner, job, status, result, result.FailureReason)
				return
			}
			if updated, updateErr := server.database.UpdateManagedMigration(ctx, job, "running", migrationProgressJSON(result), result.FailureReason, agentMigrationID, time.Now().UTC(), migrationWorkerLease); updateErr == nil {
				job = updated
			} else {
				server.logger.Warn("migration worker could not persist progress", "migration_id", job.ID, "error", updateErr)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
		}
	}
}

func (server *Server) allowedLocalAgentURL(endpoint string) bool {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme != "unix" || parsed.Path == "" {
		return false
	}
	root := filepath.Clean(server.config.LocalAgentSocketRoot)
	socket := filepath.Clean(parsed.Path)
	rel, err := filepath.Rel(root, socket)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "."
}

func migrationProgressJSON(result model.Migration) json.RawMessage {
	encoded, err := json.Marshal(migrationProgress(result))
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}

func migrationTimeout(job database.MigrationRecord) time.Duration {
	if job.TimeoutSeconds > 0 {
		return time.Duration(job.TimeoutSeconds) * time.Second
	}
	return 30 * time.Second
}

func migrationMode(mode string) model.MigrationMode {
	if mode == string(model.MigrationLive) {
		return model.MigrationLive
	}
	return model.MigrationCold
}

func (server *Server) retryMigrationJob(ctx context.Context, owner string, job database.MigrationRecord, cause error) {
	// The wrapped cause (agent transport errors, SQLSTATE detail) is logged,
	// not persisted: error_message is client-visible via GET /migrations.
	server.logger.Warn("migration attempt failed; scheduling retry", "migration_id", job.ID, "error", cause)
	if _, err := server.database.UpdateManagedMigration(ctx, job, "queued", job.Progress, "migration attempt failed; retrying", job.AgentMigrationID, time.Now().UTC().Add(migrationRetryDelay), migrationWorkerLease); err != nil {
		server.logger.Warn("migration worker could not schedule retry", "migration_id", job.ID, "error", err)
	}
}

func (server *Server) finishMigrationJob(ctx context.Context, owner string, job database.MigrationRecord, status string, result model.Migration, reason string) {
	if reason == "" {
		reason = result.FailureReason
	}
	server.recordMigrationEvents(ctx, job.ID, result)
	if _, err := server.database.UpdateManagedMigration(ctx, job, status, migrationProgressJSON(result), reason, result.ID, time.Now().UTC(), migrationWorkerLease); err != nil {
		server.logger.Warn("migration worker could not persist terminal state", "migration_id", job.ID, "error", err)
	}
	// Referral activation is the first successful migration (plan 1 BR-04):
	// the reward fires once per referred organization, idempotently.
	if status == "completed" && job.OrganizationID != "" {
		if reward, err := server.database.ActivateReferral(ctx, job.OrganizationID, time.Now().UTC()); err == nil && reward {
			_ = server.database.AddFunnelEvent(ctx, database.FunnelEvent{OrganizationID: job.OrganizationID, Kind: "referral_activated", Surface: "migration", Feature: "referral"})
			// The reward itself: mark the claim rewarded (once per referred
			// organization, capped at 12 per code) and credit both parties
			// their month of Pro.
			if granted, grantErr := server.database.GrantReferralReward(ctx, job.OrganizationID); grantErr == nil && granted {
				if creditErr := server.database.CreditReferralMonth(ctx, job.OrganizationID, time.Now().UTC()); creditErr != nil {
					server.logger.Warn("referral reward credit failed", "organization_id", job.OrganizationID, "error", creditErr)
				}
			}
		}
	}
}

// runUnmanagedSettler drives the settlement pass for non-managed jobs to a
// terminal state. Without it, agent-command and relay-route migrations stay
// "queued"/"running" in the API forever, even after the agent finished them.
func (server *Server) runUnmanagedSettler(ctx context.Context) {
	ticker := time.NewTicker(unmanagedSettleTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			server.settleUnmanagedMigrations(ctx)
		}
	}
}

// settleUnmanagedMigrations polls every watched non-managed job once.
func (server *Server) settleUnmanagedMigrations(ctx context.Context) {
	for _, watch := range server.unmanagedMigrationWatches() {
		if ctx.Err() != nil {
			return
		}
		server.pollUnmanagedMigration(ctx, watch)
	}
}

// pollUnmanagedMigration reconciles one non-managed job against the source
// agent and settles it when the agent's migration reached a terminal stage.
// The agent migration is correlated by the stored agent id, or — when the
// dispatch response was lost — by the job id, which is the agent's
// idempotency key for the operation.
func (server *Server) pollUnmanagedMigration(ctx context.Context, watch unmanagedMigrationWatch) {
	job, err := server.database.Migration(ctx, watch.OrganizationID, watch.MigrationID)
	if err != nil {
		if database.IsNotFound(err) {
			server.forgetUnmanagedMigration(watch.OrganizationID, watch.MigrationID)
		}
		return
	}
	if job.Managed {
		server.forgetUnmanagedMigration(watch.OrganizationID, watch.MigrationID)
		return
	}
	switch job.Status {
	case "completed", "failed", "cancelled": //nolint:misspell // persisted status vocabulary
		server.forgetUnmanagedMigration(watch.OrganizationID, watch.MigrationID)
		return
	}
	sourceRecord, err := server.database.Machine(ctx, job.OrganizationID, job.SourceMachineID)
	if err != nil || strings.TrimSpace(sourceRecord.AgentURL) == "" {
		return
	}
	client, err := server.newAgentClient(strings.TrimRight(sourceRecord.AgentURL, "/"), migrationTimeout(job))
	if err != nil {
		return
	}
	agentMigrationID := job.AgentMigrationID
	var result model.Migration
	if agentMigrationID == "" {
		listed, listErr := client.Migrations(ctx)
		if listErr != nil {
			return
		}
		for _, candidate := range listed {
			if candidate.ID == job.ID || candidate.IdempotencyKey == job.ID {
				agentMigrationID = candidate.ID
				result = candidate
				break
			}
		}
		if agentMigrationID == "" {
			// The agent has no record of this job. A relay command may still
			// be queued for an offline machine; abandon only past the command
			// delivery TTL.
			if time.Since(job.CreatedAt) > unmanagedJobAbandonAge {
				server.finishUnmanagedMigration(ctx, job.OrganizationID, job.ID, "failed", migrationStageProgress("FAILED"), "the agent never started this migration")
			}
			return
		}
	} else {
		result, err = client.Migration(ctx, agentMigrationID)
		if err != nil {
			// Transient agent trouble: the job stays watched and the next
			// pass retries.
			return
		}
	}
	if job.CancelRequested {
		if err := client.CancelMigration(ctx, agentMigrationID); err == nil {
			server.finishUnmanagedMigration(ctx, job.OrganizationID, job.ID, "cancelled", migrationStageProgress("CANCELLED"), "migration cancelled by operator")
		}
		return
	}
	server.recordMigrationEvents(ctx, job.ID, result)
	status := migrationJobStatus(result.Stage)
	progress, _ := json.Marshal(migrationProgress(result))
	if status == "completed" || status == "failed" || status == "cancelled" {
		server.finishUnmanagedMigration(ctx, job.OrganizationID, job.ID, status, progress, result.FailureReason)
		return
	}
	if _, err := server.database.UpdateMigrationStatus(ctx, job.OrganizationID, job.ID, "running", progress, result.FailureReason, database.AuditInput{ID: server.settleAuditID(job.ID, "progress"), OrganizationID: job.OrganizationID, Action: "migration.progress", ResourceType: "migration", ResourceID: job.ID, Metadata: map[string]any{"agent_migration_id": agentMigrationID, "settler": true}}); err != nil {
		server.logger.Warn("migration settler could not persist progress", "migration_id", job.ID, "error", err)
	}
}

// settleAuditID mints a unique audit id for a settlement-side write: these
// writes repeat on every pass and a deterministic id would collide on the
// second one.
func (server *Server) settleAuditID(migrationID, kind string) string {
	if id, err := model.NewID(); err == nil {
		return "settle-" + kind + "-" + id
	}
	return "settle-" + kind + "-" + migrationID + "-" + fmtInt(time.Now().UnixNano())
}

// finishUnmanagedMigration writes the terminal state for a non-managed job
// and runs its settlement side effects: issued relay tickets are reconciled
// so none outlives the migration, and a first successful migration activates
// the referring organization's reward.
func (server *Server) finishUnmanagedMigration(ctx context.Context, organizationID, migrationID, status string, progress json.RawMessage, reason string) {
	if _, err := server.database.UpdateMigrationStatus(ctx, organizationID, migrationID, status, progress, reason, database.AuditInput{ID: server.settleAuditID(migrationID, "settle"), OrganizationID: organizationID, Action: "migration.settled", ResourceType: "migration", ResourceID: migrationID, Metadata: map[string]any{"status": status, "settler": true}}); err != nil {
		server.logger.Warn("could not settle unmanaged migration", "migration_id", migrationID, "error", err)
		return
	}
	// Ticket reconciliation expires lapsed relay tickets for the job. The
	// bandwidth a relay leg moved is metered at the relay itself — exactly
	// once — so settlement records no bytes here.
	server.reconcileRelayTicketUsage(ctx, organizationID, migrationID, 0)
	if status == "completed" && organizationID != "" {
		if reward, err := server.database.ActivateReferral(ctx, organizationID, time.Now().UTC()); err == nil && reward {
			_ = server.database.AddFunnelEvent(ctx, database.FunnelEvent{OrganizationID: organizationID, Kind: "referral_activated", Surface: "migration", Feature: "referral"})
			// Same reward path as the managed finisher: mark once per referred
			// organization (12-reward cap) and credit both parties their month.
			if granted, grantErr := server.database.GrantReferralReward(ctx, organizationID); grantErr == nil && granted {
				if creditErr := server.database.CreditReferralMonth(ctx, organizationID, time.Now().UTC()); creditErr != nil {
					server.logger.Warn("referral reward credit failed", "organization_id", organizationID, "error", creditErr)
				}
			}
		}
	}
	server.forgetUnmanagedMigration(organizationID, migrationID)
}

// recordMigrationEvents appends the agent's migration events to the job's
// event stream on every observation. Rows are keyed by the agent's event
// sequence and dedupe on replay, so polling can re-persist safely.
func (server *Server) recordMigrationEvents(ctx context.Context, migrationID string, result model.Migration) {
	for index, event := range result.Events {
		sequence := int64(event.Sequence)
		if sequence <= 0 {
			sequence = int64(index) + 1
		}
		createdAt := event.Timestamp
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		if err := server.database.AddMigrationEvent(ctx, database.MigrationEventRecord{
			ID: result.ID + "-" + fmtInt(sequence), MigrationID: migrationID, Sequence: sequence,
			Stage: string(event.Stage), Message: event.Message, Progress: event.Progress,
			BytesDone: event.BytesDone, BytesTotal: event.BytesTotal, CreatedAt: createdAt,
		}); err != nil {
			server.logger.Warn("could not persist migration event", "migration_id", migrationID, "error", err)
			return
		}
	}
}
