package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/observability"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	"shift.dev/shift/internal/securestore"
)

type Authenticator func(*http.Request) (string, error)

// TransferMeter records delivered bandwidth for billing. *database.Store
// implements it (RecordTransferBytes); the agent wires its own accumulator so
// the increments reach the control plane's usage_records. The channel
// vocabulary mirrors internal/database/usage.go's TransferChannel*.
type TransferMeter interface {
	RecordTransferBytes(ctx context.Context, organizationID, channel string, bytes int64) error
}

// Metering channels for sessions this server completes. Migration delivery is
// egress from the origin's perspective; a held replication push is the
// restorable copy the standby keeps.
const (
	TransferChannelEgress  = "egress"
	TransferChannelRestore = "restore"
)

// ReplicationWatcher is the agent-side counterpart of a held replication
// session. The peer server calls it after the session is durably persisted,
// so the agent's standby supervisor can keep its duty records current; a
// watcher is notified on the server's handler goroutine and must not block.
type ReplicationWatcher interface {
	// ReplicationHeld reports a session that just entered (or already sat in)
	// the HELD state: its checkpoint is imported, verified, and restorable.
	ReplicationHeld(session Session)
	// ReplicationWithdrawn reports that the authenticated source machine
	// withdrew its standby duty for the workload. The watcher matches the
	// duty by source and workload; a withdrawal naming a duty that never
	// existed is a no-op.
	ReplicationWithdrawn(sourceMachineID, workloadID string)
}

type Server struct {
	sessions     *Sessions
	keys         *securestore.Manager
	chunks       *chunkstore.Store
	checkpoints  *checkpoint.Repository
	restorer     *checkpoint.Restorer
	inventory    *linuxplatform.Inventory
	diagnostics  *observability.Diagnostics
	authenticate Authenticator
	logger       *slog.Logger
	maxBodyBytes int64
	replication  ReplicationWatcher
	// importSlots caps concurrent chunk verification memory: each in-flight
	// import buffers a whole encoded chunk plus its decompressed plaintext,
	// and the peer controls how many uploads run at once. See
	// maxConcurrentChunkImports.
	importSlots chan struct{}
	// meter and organizationID carry cost-side bandwidth metering: on session
	// completion the delivered bytes are recorded for the organization this
	// agent serves. Both are optional; without a meter nothing is recorded.
	meter          TransferMeter
	organizationID string
}

// maxConcurrentChunkImports bounds how many chunk uploads verify at once,
// matching the uploader's own worker fanout: full-buffer verification of one
// maximum chunk costs up to ~160 MiB, so the worst case is a fixed few
// hundred MiB no matter how many parallel PUTs a peer opens.
const maxConcurrentChunkImports = 4

type ReserveRequest struct {
	ID              string    `json:"id"`
	SourceMachineID string    `json:"source_machine_id"`
	WorkloadID      string    `json:"workload_id"`
	EstimatedBytes  int64     `json:"estimated_bytes"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	// Purpose marks a replication reserve. Empty means a migration.
	Purpose string `json:"purpose,omitempty"`
}

type KeyRequest struct {
	WorkloadID string `json:"workload_id"`
	KeyVersion uint32 `json:"key_version"`
	Key        string `json:"key"`
}

type MissingResponse struct {
	Missing           []model.ChunkRef `json:"missing"`
	DeduplicatedBytes int64            `json:"deduplicated_bytes"`
}

type RestoreRequest struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// HoldRequest carries the standby-duty facts with a replication hold: the
// retention the source asks the standby to keep, and the source's advertised
// peer listener the standby should probe before ever failing over on its own.
type HoldRequest struct {
	KeepLast        int    `json:"keep_last"`
	SourceAgentURL  string `json:"source_agent_url,omitempty"`
	FencingProvider string `json:"fencing_provider,omitempty"`
}

// WithdrawRequest names the workload whose standby duty the source withdraws.
type WithdrawRequest struct {
	WorkloadID string `json:"workload_id"`
}

func NewServer(sessions *Sessions, keys *securestore.Manager, chunks *chunkstore.Store, checkpoints *checkpoint.Repository, restorer *checkpoint.Restorer, inventory *linuxplatform.Inventory, authenticate Authenticator, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	sessions.SetLogger(logger)
	server := &Server{
		sessions: sessions, keys: keys, chunks: chunks, checkpoints: checkpoints,
		restorer: restorer, inventory: inventory, authenticate: authenticate,
		logger: logger, maxBodyBytes: 16 << 20,
		importSlots: make(chan struct{}, maxConcurrentChunkImports),
	}
	// A reaped session that had restored a workload must not leave the
	// restored copy running: the reservation is gone, and the source may
	// still be live. Boot sweeps once through the same path later
	// reservations do.
	sessions.SetReapRestoreHandler(server.rollbackReapedRestore)
	sessions.ReapExpired()
	return server
}

// rollbackReapedRestore rolls back the restore an expired session left
// behind. Failures are logged — the session is already retired — and the
// operator runbook owns the manual cleanup.
func (s *Server) rollbackReapedRestore(session Session) {
	if s.restorer == nil || session.RestoreID == "" {
		return
	}
	if _, err := s.restorer.Rollback(context.Background(), session.RestoreID, "transfer reservation expired before commit"); err != nil {
		s.logger.Warn("could not roll back restore of an expired transfer session",
			"session_id", session.ID, "restore_id", session.RestoreID, "error", err)
	}
}

// SetDiagnostics attaches the recorder for the destination side's transfer
// series. Incoming chunk volume is counted here — the source's own counter
// only sees what left, which can differ on retries and deduplication.
func (s *Server) SetDiagnostics(diagnostics *observability.Diagnostics) {
	s.diagnostics = diagnostics
}

// SetReplicationWatcher attaches the standby supervisor's duty recorder.
// Without one, hold and withdraw answer 503: those endpoints exist to reach
// the agent's duty store, and silently accepting a duty no one records
// would tell the source its state is protected when it is not.
func (s *Server) SetReplicationWatcher(watcher ReplicationWatcher) {
	s.replication = watcher
}

// SetTransferMeter attaches the bandwidth meter and the organization the
// agent serves. Sessions record the organization at reservation time so the
// completion increment carries "org from the session", and both are optional:
// without a meter the server simply records nothing.
func (s *Server) SetTransferMeter(organizationID string, meter TransferMeter) {
	s.organizationID = organizationID
	s.meter = meter
}

// meterCompletion records one completed session's delivered bytes exactly
// once — at the state transition that ends the transfer — so a retried
// completion cannot double-bill.
func (s *Server) meterCompletion(ctx context.Context, session Session, channel string) {
	if s.meter == nil || session.OrganizationID == "" || session.ReceivedBytes <= 0 {
		return
	}
	if err := s.meter.RecordTransferBytes(ctx, session.OrganizationID, channel, session.ReceivedBytes); err != nil {
		s.logger.Warn("could not record transfer usage", "session_id", session.ID, "error", err)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/peer/machine", s.machine)
	mux.HandleFunc("POST /v1/peer/transfers", s.reserve)
	mux.HandleFunc("PUT /v1/peer/transfers/{id}/key", s.importKey)
	mux.HandleFunc("POST /v1/peer/transfers/{id}/manifest", s.importManifest)
	mux.HandleFunc("PUT /v1/peer/transfers/{id}/chunks/{version}/{address}", s.importChunk)
	mux.HandleFunc("POST /v1/peer/transfers/{id}/verify", s.verify)
	mux.HandleFunc("POST /v1/peer/transfers/{id}/restore", s.restore)
	mux.HandleFunc("POST /v1/peer/transfers/{id}/commit", s.commit)
	mux.HandleFunc("POST /v1/peer/transfers/{id}/rollback", s.rollback)
	mux.HandleFunc("POST /v1/peer/transfers/{id}/hold", s.hold)
	mux.HandleFunc("POST /v1/peer/standby/withdraw", s.withdraw)
	mux.HandleFunc("GET /v1/peer/transfers/{id}", s.get)
	mux.HandleFunc("GET /v1/peer/transfers/{id}/cleanup", s.getCleanup)
	return s.withAuth(mux)
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		machineID, err := s.authenticate(request)
		if err != nil || machineID == "" {
			writeError(writer, http.StatusUnauthorized, "PEER_AUTHENTICATION_FAILED", "mutual TLS peer authentication failed")
			return
		}
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), peerMachineKey{}, machineID)))
	})
}

func (s *Server) machine(writer http.ResponseWriter, request *http.Request) {
	capabilities, err := s.inventory.Inspect(request.Context())
	if err != nil {
		s.writeInternalError(writer, http.StatusInternalServerError, "INVENTORY_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, capabilities)
}

func (s *Server) reserve(writer http.ResponseWriter, request *http.Request) {
	var input ReserveRequest
	if !decodeJSON(writer, request, s.maxBodyBytes, &input) {
		return
	}
	source := peerMachine(request.Context())
	if input.SourceMachineID != source {
		writeError(writer, http.StatusForbidden, "SOURCE_IDENTITY_MISMATCH", "request source does not match the authenticated machine")
		return
	}
	if !validPurpose(input.Purpose) {
		writeError(writer, http.StatusBadRequest, "PURPOSE_INVALID", "transfer purpose must be empty (migration) or \"replication\"")
		return
	}
	if input.EstimatedBytes < 0 {
		writeError(writer, http.StatusBadRequest, "ESTIMATE_INVALID", "estimated bytes cannot be negative")
		return
	}
	s.sessions.ReapExpired()
	capabilities, err := s.inventory.Inspect(request.Context())
	if err != nil {
		s.writeInternalError(writer, http.StatusInternalServerError, "INVENTORY_FAILED", err)
		return
	}
	var largestAvailable uint64
	for _, storage := range capabilities.Storage {
		if storage.AvailableBytes > largestAvailable {
			largestAvailable = storage.AvailableBytes
		}
	}
	// An explicit estimate is a claim this destination can check up front.
	// A missing estimate no longer disables anything: Reserve assigns the
	// default reservation, and ingest is bounded per chunk by the quota and
	// the free-space floor below.
	if input.EstimatedBytes > 0 && uint64(input.EstimatedBytes) > largestAvailable {
		writeError(writer, http.StatusInsufficientStorage, "STORAGE_INSUFFICIENT", "destination has insufficient free storage for the reservation")
		return
	}
	session, err := s.sessions.Reserve(Session{
		ID: input.ID, SourceMachineID: source, WorkloadID: input.WorkloadID,
		OrganizationID: s.organizationID,
		EstimatedBytes: input.EstimatedBytes, ExpiresAt: input.ExpiresAt,
		Purpose: input.Purpose,
	})
	if err != nil {
		writeError(writer, http.StatusConflict, "RESERVATION_REJECTED", err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, session)
}

func (s *Server) importKey(writer http.ResponseWriter, request *http.Request) {
	var input KeyRequest
	if !decodeJSON(writer, request, s.maxBodyBytes, &input) {
		return
	}
	source := peerMachine(request.Context())
	session, err := s.sessions.Get(request.PathValue("id"), source)
	if err != nil {
		writeError(writer, http.StatusNotFound, "TRANSFER_NOT_FOUND", err.Error())
		return
	}
	if session.WorkloadID != input.WorkloadID {
		writeError(writer, http.StatusConflict, "WORKLOAD_MISMATCH", "key belongs to another workload")
		return
	}
	key, err := base64.RawStdEncoding.DecodeString(input.Key)
	if err != nil || len(key) != 32 {
		writeError(writer, http.StatusBadRequest, "INVALID_KEY", "workload key must be 32 bytes")
		return
	}
	if err := s.keys.ImportWorkloadKey(input.WorkloadID, input.KeyVersion, key); err != nil {
		writeError(writer, http.StatusConflict, "KEY_IMPORT_FAILED", err.Error())
		return
	}
	session, err = s.sessions.Update(session.ID, source, func(value *Session) error {
		if err := requireState(value, SessionReserved, SessionKeyReady); err != nil {
			return err
		}
		value.KeyVersion = input.KeyVersion
		value.State = SessionKeyReady
		return nil
	})
	if err != nil {
		writeError(writer, http.StatusConflict, "TRANSFER_STATE_INVALID", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, session)
}

func (s *Server) importManifest(writer http.ResponseWriter, request *http.Request) {
	var manifest model.CheckpointManifest
	if !decodeJSON(writer, request, s.maxBodyBytes, &manifest) {
		return
	}
	source := peerMachine(request.Context())
	session, err := s.sessions.Get(request.PathValue("id"), source)
	if err != nil {
		writeError(writer, http.StatusNotFound, "TRANSFER_NOT_FOUND", err.Error())
		return
	}
	if err := requireState(&session, SessionKeyReady, SessionManifestReady); err != nil {
		writeError(writer, http.StatusConflict, "TRANSFER_STATE_INVALID", err.Error())
		return
	}
	if manifest.SourceIdentity.ID != source || manifest.Workload.ID != session.WorkloadID || manifest.Security.KeyVersion != session.KeyVersion {
		writeError(writer, http.StatusForbidden, "MANIFEST_IDENTITY_MISMATCH", "manifest is not bound to this transfer")
		return
	}
	// The manifest ID is about to be joined into repository paths as
	// root — a peer must never choose a path. Refuse anything that is not a
	// flat identifier before any save, load, or delete can see it.
	if !validCheckpointID(manifest.ID) {
		writeError(writer, http.StatusBadRequest, "MANIFEST_INVALID", "checkpoint id is not a safe identifier")
		return
	}
	// A manifest may be re-posted identically (idempotent retry), but a
	// different one can never replace it: the checkpoint binding and the
	// chunk authorization are scoped to the manifest that verification
	// planned against.
	if session.CheckpointID != "" && session.CheckpointID != manifest.ID {
		writeError(writer, http.StatusConflict, "MANIFEST_REPLACED", "a different manifest is already bound to this transfer")
		return
	}
	if err := checkpoint.VerifyManifest(manifest); err != nil {
		writeError(writer, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
		return
	}
	if err := s.checkpoints.Save(manifest); err != nil {
		writeError(writer, http.StatusConflict, "MANIFEST_IMPORT_FAILED", err.Error())
		return
	}
	refs := UniqueChunks(manifest.Assets)
	missing, err := s.chunks.Missing(refs)
	if err != nil {
		s.writeInternalError(writer, http.StatusInternalServerError, "CHUNK_INVENTORY_FAILED", err)
		return
	}
	missingAddresses := make(map[string]bool, len(missing))
	for _, ref := range missing {
		missingAddresses[ref.Address] = true
	}
	var deduplicated int64
	for _, ref := range refs {
		if !missingAddresses[ref.Address] {
			deduplicated += ref.PlainSize
		}
	}
	session, err = s.sessions.Update(session.ID, source, func(value *Session) error {
		value.CheckpointID = manifest.ID
		value.State = SessionManifestReady
		return nil
	})
	if err != nil {
		s.writeInternalError(writer, http.StatusInternalServerError, "TRANSFER_PERSIST_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, MissingResponse{Missing: missing, DeduplicatedBytes: deduplicated})
}

func (s *Server) importChunk(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	source := peerMachine(request.Context())
	session, err := s.sessions.Get(request.PathValue("id"), source)
	if err != nil {
		writeError(writer, http.StatusNotFound, "TRANSFER_NOT_FOUND", err.Error())
		return
	}
	// A live migration uploads its pre-copy passes' chunks in KEY_READY,
	// before the final checkpoint exists: interleaving each pass's transfer
	// with the pass loop is what keeps the later freeze down to the final
	// delta. Before a manifest is imported the session itself is the
	// authorization — the request carries the peer's verified machine
	// identity, the chunk is AEAD ciphertext under the workload key ImportKey
	// already bound to this session, the content address is verified when the
	// chunk is stored, and the reservation bounds the bytes. Once a manifest
	// is imported the stricter rule applies: only chunks it references.
	if err := requireState(&session, SessionKeyReady, SessionManifestReady); err != nil {
		writeError(writer, http.StatusConflict, "TRANSFER_STATE_INVALID", err.Error())
		return
	}
	version, err := strconv.ParseUint(request.PathValue("version"), 10, 32)
	if err != nil || version != uint64(session.KeyVersion) {
		writeError(writer, http.StatusBadRequest, "KEY_VERSION_INVALID", "chunk key version does not match the transfer")
		return
	}
	header := request.Header.Get("X-Shift-Chunk-Ref")
	encoded, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "CHUNK_REF_INVALID", "chunk reference header is invalid")
		return
	}
	var ref model.ChunkRef
	if err := json.Unmarshal(encoded, &ref); err != nil || ref.Address != request.PathValue("address") || uint64(ref.KeyVersion) != version {
		writeError(writer, http.StatusBadRequest, "CHUNK_REF_INVALID", "chunk reference does not match request path")
		return
	}
	if session.CheckpointID != "" {
		manifest, err := s.loadSessionManifest(session)
		if err != nil {
			s.writeInternalError(writer, http.StatusInternalServerError, "MANIFEST_LOAD_FAILED", err)
			return
		}
		if !manifestContainsChunk(manifest, ref) {
			writeError(writer, http.StatusForbidden, "CHUNK_NOT_AUTHORIZED", "chunk is not referenced by this transfer manifest")
			return
		}
	}
	if free, freeErr := s.chunks.FreeBytes(); freeErr == nil && free < freeSpaceFloor {
		writeError(writer, http.StatusInsufficientStorage, "STORAGE_INSUFFICIENT", "destination is below its free-space floor; chunk ingest is paused")
		return
	}
	// Chunk verification buffers a whole encoded chunk plus its decompressed
	// plaintext, and the peer decides how many uploads to run at once. A
	// verification slot is held from before the quota gate until the import
	// finishes, so N parallel PUTs can pin at most maxConcurrentChunkImports
	// chunk-sized buffers instead of N of them.
	select {
	case s.importSlots <- struct{}{}:
		defer func() { <-s.importSlots }()
	case <-request.Context().Done():
		writeError(writer, http.StatusServiceUnavailable, "CHUNK_IMPORT_BUSY", "chunk ingest stopped waiting for a verification slot")
		return
	}
	// The quota gate and the attempted-byte ceiling are accounted before a
	// single byte is stored: reservations with a zero estimate still carry
	// the default reservation, retries and deduplicated uploads still count,
	// and concurrent imports cannot race past the ceiling by reading stale
	// counters. The receive bytes are reserved in the same serialized gate
	// that checks them — two concurrent imports can no longer both pass the
	// check and both land past the reservation — and released below when a
	// chunk turns out to be a dedup hit or the import fails.
	session, err = s.sessions.Update(session.ID, source, func(value *Session) error {
		if value.ReceivedBytes+ref.StoredSize > quotaLimit(value.EstimatedBytes) {
			return errTransferQuota
		}
		if value.AttemptedBytes+ref.StoredSize > attemptLimit(value.EstimatedBytes) {
			return errTransferQuota
		}
		value.AttemptedBytes += ref.StoredSize
		value.ReceivedBytes += ref.StoredSize
		return nil
	})
	if err != nil {
		if errors.Is(err, errTransferQuota) {
			writeError(writer, http.StatusInsufficientStorage, "TRANSFER_QUOTA_EXCEEDED", "incoming transfer exceeded its reserved size")
			return
		}
		s.writeInternalError(writer, http.StatusInternalServerError, "TRANSFER_PERSIST_FAILED", err)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, ref.StoredSize+1)
	inserted, importErr := s.chunks.ImportChunkResult(request.Context(), session.WorkloadID, ref, request.Body)
	if importErr != nil || !inserted {
		if updated, updateErr := s.sessions.Update(session.ID, source, func(value *Session) error {
			value.ReceivedBytes -= ref.StoredSize
			return nil
		}); updateErr != nil {
			s.logger.Warn("could not release reserved transfer bytes", "session_id", session.ID, "error", updateErr)
		} else {
			session = updated
		}
	}
	if importErr != nil {
		writeError(writer, http.StatusBadRequest, "CHUNK_IMPORT_FAILED", importErr.Error())
		return
	}
	if inserted {
		if s.diagnostics != nil {
			// Wall time from request start through receive, decrypt, and store:
			// the destination-side counterpart of the uploader's send duration.
			s.diagnostics.Transferred("download", ref.StoredSize, time.Since(started))
		}
	}
	writeJSON(writer, http.StatusOK, session)
}

func (s *Server) verify(writer http.ResponseWriter, request *http.Request) {
	source := peerMachine(request.Context())
	session, err := s.sessions.Get(request.PathValue("id"), source)
	if err != nil {
		writeError(writer, http.StatusNotFound, "TRANSFER_NOT_FOUND", err.Error())
		return
	}
	if err := requireState(&session, SessionManifestReady, SessionVerified); err != nil {
		writeError(writer, http.StatusConflict, "TRANSFER_STATE_INVALID", err.Error())
		return
	}
	manifest, err := s.loadSessionManifest(session)
	if err != nil {
		s.writeInternalError(writer, http.StatusInternalServerError, "MANIFEST_LOAD_FAILED", err)
		return
	}
	for _, asset := range manifest.Assets {
		if err := s.chunks.ValidateAsset(request.Context(), session.WorkloadID, asset); err != nil {
			writeError(writer, http.StatusUnprocessableEntity, "CHECKPOINT_CORRUPT", err.Error())
			return
		}
	}
	session, err = s.sessions.Update(session.ID, source, func(value *Session) error {
		value.State = SessionVerified
		return nil
	})
	if err != nil {
		s.writeInternalError(writer, http.StatusInternalServerError, "TRANSFER_PERSIST_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, session)
}

func (s *Server) restore(writer http.ResponseWriter, request *http.Request) {
	var input RestoreRequest
	if request.ContentLength != 0 && !decodeJSON(writer, request, s.maxBodyBytes, &input) {
		return
	}
	source := peerMachine(request.Context())
	session, err := s.sessions.Get(request.PathValue("id"), source)
	if err != nil {
		writeError(writer, http.StatusNotFound, "TRANSFER_NOT_FOUND", err.Error())
		return
	}
	if session.State == SessionRestored && session.RestoreID != "" {
		record, getErr := s.restorer.Get(session.RestoreID)
		if getErr != nil {
			s.writeInternalError(writer, http.StatusInternalServerError, "RESTORE_STATE_MISSING", getErr)
			return
		}
		writeJSON(writer, http.StatusOK, record)
		return
	}
	if err := requireState(&session, SessionVerified); err != nil {
		writeError(writer, http.StatusConflict, "TRANSFER_STATE_INVALID", err.Error())
		return
	}
	if !validCheckpointID(session.CheckpointID) {
		writeError(writer, http.StatusConflict, "TRANSFER_STATE_INVALID", "session checkpoint id is not a safe identifier")
		return
	}
	timeout := time.Duration(input.TimeoutSeconds) * time.Second
	// A migration restore is always eager: the source is frozen waiting for
	// the destination's answer, and the migration's downtime metric must mean
	// the process is fully there — a lazy start would report a resume the
	// memory had not caught up with.
	record, err := s.restorer.Prepare(request.Context(), session.CheckpointID, checkpoint.PrepareOptions{Timeout: timeout})
	if err != nil {
		if _, updateErr := s.sessions.UpdateCleanup(session.ID, source, func(value *Session) error {
			value.State = SessionFailed
			value.FailureReason = err.Error()
			return nil
		}); updateErr != nil {
			s.logger.Warn("could not mark transfer session failed after restore error", "session_id", session.ID, "error", updateErr)
		}
		writeError(writer, http.StatusUnprocessableEntity, "RESTORE_FAILED", err.Error())
		return
	}
	session, err = s.sessions.Update(session.ID, source, func(value *Session) error {
		value.State = SessionRestored
		value.RestoreID = record.ID
		return nil
	})
	if err != nil {
		_, _ = s.restorer.Rollback(request.Context(), record.ID, "could not persist incoming transfer restore state")
		s.writeInternalError(writer, http.StatusInternalServerError, "TRANSFER_PERSIST_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, record)
}

func (s *Server) commit(writer http.ResponseWriter, request *http.Request) {
	source := peerMachine(request.Context())
	session, err := s.sessions.Get(request.PathValue("id"), source)
	if err != nil {
		writeError(writer, http.StatusNotFound, "TRANSFER_NOT_FOUND", err.Error())
		return
	}
	if session.State == SessionCommitted {
		writeJSON(writer, http.StatusOK, session)
		return
	}
	if err := requireState(&session, SessionRestored); err != nil {
		writeError(writer, http.StatusConflict, "TRANSFER_STATE_INVALID", err.Error())
		return
	}
	if _, err := s.restorer.Commit(session.RestoreID); err != nil {
		s.writeInternalError(writer, http.StatusInternalServerError, "RESTORE_COMMIT_FAILED", err)
		return
	}
	sessionID := session.ID
	updated, updateErr := s.sessions.Update(sessionID, source, func(value *Session) error {
		// The terminal-state check belongs inside the serialized update:
		// two racing commits must not both perform the transition, and only
		// the request that actually transitions may meter completion.
		if value.State == SessionCommitted {
			return errAlreadyCommitted
		}
		if err := requireState(value, SessionRestored); err != nil {
			return fmt.Errorf("%w: %v", errTransferStateInvalid, err)
		}
		value.State = SessionCommitted
		return nil
	})
	switch {
	case errors.Is(updateErr, errAlreadyCommitted):
		if current, getErr := s.sessions.Get(sessionID, source); getErr == nil {
			session = current
		}
		writeJSON(writer, http.StatusOK, session)
		return
	case errors.Is(updateErr, errTransferStateInvalid):
		writeError(writer, http.StatusConflict, "TRANSFER_STATE_INVALID", updateErr.Error())
		return
	case updateErr != nil:
		s.writeInternalError(writer, http.StatusInternalServerError, "TRANSFER_PERSIST_FAILED", updateErr)
		return
	}
	session = updated
	s.meterCompletion(request.Context(), session, TransferChannelEgress)
	writeJSON(writer, http.StatusOK, session)
}

func (s *Server) rollback(writer http.ResponseWriter, request *http.Request) {
	source := peerMachine(request.Context())
	session, err := s.sessions.GetForCleanup(request.PathValue("id"), source)
	if err != nil {
		writeError(writer, http.StatusNotFound, "TRANSFER_NOT_FOUND", err.Error())
		return
	}
	if session.State == SessionCommitted {
		writeError(writer, http.StatusConflict, "TRANSFER_ALREADY_COMMITTED", "committed destination state cannot be rolled back")
		return
	}
	if session.RestoreID != "" {
		if _, err := s.restorer.Rollback(request.Context(), session.RestoreID, "source requested migration rollback"); err != nil {
			s.writeInternalError(writer, http.StatusInternalServerError, "RESTORE_ROLLBACK_FAILED", err)
			return
		}
	}
	session, err = s.sessions.UpdateCleanup(session.ID, source, func(value *Session) error {
		value.State = SessionRolledBack
		return nil
	})
	if err != nil {
		s.writeInternalError(writer, http.StatusInternalServerError, "TRANSFER_PERSIST_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, session)
}

// hold parks a verified replication session as the standby's restorable copy
// and records the duty facts the source sends with it. Only a replication
// session can be held — a migration's session ends in restore and commit —
// and only after Verify proved every referenced chunk present and intact.
// Holding is idempotent: a session already HELD is returned unchanged so a
// source retrying after a lost response cannot corrupt the standby's record.
func (s *Server) hold(writer http.ResponseWriter, request *http.Request) {
	if s.replication == nil {
		writeError(writer, http.StatusServiceUnavailable, "STANDBY_NOT_AVAILABLE", "this agent does not run a standby supervisor")
		return
	}
	var input HoldRequest
	if !decodeJSON(writer, request, s.maxBodyBytes, &input) {
		return
	}
	if input.KeepLast < 0 {
		writeError(writer, http.StatusBadRequest, "HOLD_INVALID", "keep_last cannot be negative")
		return
	}
	if input.SourceAgentURL != "" {
		parsed, err := url.Parse(input.SourceAgentURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			writeError(writer, http.StatusBadRequest, "HOLD_INVALID", "source agent URL must be a plain https URL")
			return
		}
	}
	source := peerMachine(request.Context())
	session, err := s.sessions.Get(request.PathValue("id"), source)
	if err != nil {
		writeError(writer, http.StatusNotFound, "TRANSFER_NOT_FOUND", err.Error())
		return
	}
	if session.State != SessionHeld {
		if session.Purpose != PurposeReplication {
			writeError(writer, http.StatusConflict, "TRANSFER_STATE_INVALID", "only a replication transfer can be held")
			return
		}
		if err := requireState(&session, SessionVerified); err != nil {
			writeError(writer, http.StatusConflict, "TRANSFER_STATE_INVALID", err.Error())
			return
		}
		sessionID := session.ID
		updated, updateErr := s.sessions.Update(sessionID, source, func(value *Session) error {
			// The terminal-state check belongs inside the serialized update:
			// two racing holds must not both perform the transition, and only
			// the request that actually transitions may meter completion.
			if value.State == SessionHeld {
				return errAlreadyHeld
			}
			if err := requireState(value, SessionVerified); err != nil {
				return fmt.Errorf("%w: %v", errTransferStateInvalid, err)
			}
			value.State = SessionHeld
			value.KeepLast = input.KeepLast
			value.SourceAgentURL = input.SourceAgentURL
			value.FencingProvider = input.FencingProvider
			return nil
		})
		switch {
		case errors.Is(updateErr, errAlreadyHeld):
			if current, getErr := s.sessions.Get(sessionID, source); getErr == nil {
				session = current
			}
		case errors.Is(updateErr, errTransferStateInvalid):
			writeError(writer, http.StatusConflict, "TRANSFER_STATE_INVALID", updateErr.Error())
			return
		case updateErr != nil:
			s.writeInternalError(writer, http.StatusInternalServerError, "TRANSFER_PERSIST_FAILED", updateErr)
			return
		default:
			session = updated
			s.meterCompletion(request.Context(), session, TransferChannelRestore)
		}
	}
	s.replication.ReplicationHeld(session)
	writeJSON(writer, http.StatusOK, session)
}

// withdraw releases the source's standby duty for a workload. The duty this
// reaches belongs to the authenticated source machine only; a peer cannot
// withdraw another machine's duty. Withdrawal removes the duty — the
// replicated checkpoints already on the standby stay: deleting state on
// another machine's say-so is not a withdrawal's job, and the standby's
// operator can prune what a withdrawn duty left behind.
func (s *Server) withdraw(writer http.ResponseWriter, request *http.Request) {
	if s.replication == nil {
		writeError(writer, http.StatusServiceUnavailable, "STANDBY_NOT_AVAILABLE", "this agent does not run a standby supervisor")
		return
	}
	var input WithdrawRequest
	if !decodeJSON(writer, request, s.maxBodyBytes, &input) {
		return
	}
	if strings.TrimSpace(input.WorkloadID) == "" {
		writeError(writer, http.StatusBadRequest, "WITHDRAW_INVALID", "workload_id is required")
		return
	}
	s.replication.ReplicationWithdrawn(peerMachine(request.Context()), input.WorkloadID)
	writeJSON(writer, http.StatusOK, map[string]string{"status": "withdrawn"})
}

func (s *Server) get(writer http.ResponseWriter, request *http.Request) {
	session, err := s.sessions.Get(request.PathValue("id"), peerMachine(request.Context()))
	if err != nil {
		writeError(writer, http.StatusNotFound, "TRANSFER_NOT_FOUND", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, session)
}

// getCleanup reads a session for rollback and recovery bookkeeping. Unlike get
// it ignores the reservation expiry — tearing down what an abandoned session
// left behind, and probing what a crashed migration may still have on the
// destination, must stay possible after the clock runs out. The source-machine
// binding still applies.
func (s *Server) getCleanup(writer http.ResponseWriter, request *http.Request) {
	session, err := s.sessions.GetForCleanup(request.PathValue("id"), peerMachine(request.Context()))
	if err != nil {
		writeError(writer, http.StatusNotFound, "TRANSFER_NOT_FOUND", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, session)
}

func UniqueChunks(assets []model.AssetManifest) []model.ChunkRef {
	seen := make(map[string]bool)
	refs := make([]model.ChunkRef, 0)
	for _, asset := range assets {
		for _, ref := range asset.Chunks {
			key := fmt.Sprintf("%d:%s", ref.KeyVersion, ref.Address)
			if !seen[key] {
				seen[key] = true
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

// validCheckpointID reports whether a checkpoint ID may be joined into a
// filesystem path. The manifest ID is peer-controlled input, and the
// repository builds paths as Join(root, id, ...): an ID carrying separators
// or dot segments would let a peer choose the path a root-owned agent writes
// or deletes. The only IDs SHIFT mints are model.NewID() shapes; anything
// outside the flat identifier charset is refused at this boundary before it
// ever reaches a path join.
func validCheckpointID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for index := 0; index < len(id); index++ {
		char := id[index]
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '-', char == '_':
		default:
			return false
		}
	}
	return true
}

// loadSessionManifest loads the manifest a session binds, refusing a
// checkpoint ID that fails the flat-component check even if one reached the
// session record by a legacy path. Every consumer that hands the ID to a
// repository or restorer path join goes through here first.
func (s *Server) loadSessionManifest(session Session) (model.CheckpointManifest, error) {
	if !validCheckpointID(session.CheckpointID) {
		return model.CheckpointManifest{}, errors.New("session checkpoint id is not a safe identifier")
	}
	return s.checkpoints.Load(session.CheckpointID)
}

func manifestContainsChunk(manifest model.CheckpointManifest, candidate model.ChunkRef) bool {
	for _, ref := range UniqueChunks(manifest.Assets) {
		if ref.Address == candidate.Address && ref.KeyVersion == candidate.KeyVersion && ref.StoredSize == candidate.StoredSize && ref.CipherSHA256 == candidate.CipherSHA256 && ref.PlainSHA256 == candidate.PlainSHA256 {
			return true
		}
	}
	return false
}

type peerMachineKey struct{}

func peerMachine(ctx context.Context) string {
	value, _ := ctx.Value(peerMachineKey{}).(string)
	return value
}

func CertificateMachineID(request *http.Request) (string, error) {
	if request.TLS == nil || len(request.TLS.PeerCertificates) == 0 || len(request.TLS.VerifiedChains) == 0 {
		return "", errors.New("verified client certificate is required")
	}
	digest := sha256.Sum256(request.TLS.PeerCertificates[0].RawSubjectPublicKeyInfo)
	return hex.EncodeToString(digest[:]), nil
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, limit int64, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, limit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(writer, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "INVALID_JSON", "request contains trailing data")
		return false
	}
	return true
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, model.ErrorResponse{Code: code, Message: message})
}

// writeInternalError reports a server-side failure without echoing internals
// to the peer: the cause is logged here, and the response carries only a
// stable code and message. Client-visible detail belongs in 4xx validation
// responses, never in a 5xx body.
func (s *Server) writeInternalError(writer http.ResponseWriter, status int, code string, err error) {
	s.logger.Error("peer transfer request failed", "code", code, "error", err)
	writeError(writer, status, code, "internal server error")
}

// freeSpaceFloor is the host free-space floor below which chunk ingest is
// paused. Below it every workload on the host is one dump or restore away
// from ENOSPC, so incoming chunks are refused while there is still room to
// operate.
const freeSpaceFloor = 512 << 20

var (
	errTransferQuota = errors.New("incoming transfer exceeded its reserved size")
	// errAlreadyCommitted and errAlreadyHeld mark a terminal-state race
	// inside the serialized session update: the request that loses the race
	// must neither repeat the transition nor its completion metering.
	errAlreadyCommitted = errors.New("transfer session is already committed")
	errAlreadyHeld      = errors.New("transfer session is already held")
	// errTransferStateInvalid wraps a state-machine refusal raised inside an
	// update callback so the handler can answer 409 rather than 500.
	errTransferStateInvalid = errors.New("transfer state invalid")
)
