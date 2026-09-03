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
}

type ReserveRequest struct {
	ID              string    `json:"id"`
	SourceMachineID string    `json:"source_machine_id"`
	WorkloadID      string    `json:"workload_id"`
	EstimatedBytes  int64     `json:"estimated_bytes"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
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

func NewServer(sessions *Sessions, keys *securestore.Manager, chunks *chunkstore.Store, checkpoints *checkpoint.Repository, restorer *checkpoint.Restorer, inventory *linuxplatform.Inventory, authenticate Authenticator, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		sessions: sessions, keys: keys, chunks: chunks, checkpoints: checkpoints,
		restorer: restorer, inventory: inventory, authenticate: authenticate,
		logger: logger, maxBodyBytes: 16 << 20,
	}
}

// SetDiagnostics attaches the recorder for the destination side's transfer
// series. Incoming chunk volume is counted here — the source's own counter
// only sees what left, which can differ on retries and deduplication.
func (s *Server) SetDiagnostics(diagnostics *observability.Diagnostics) {
	s.diagnostics = diagnostics
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
	mux.HandleFunc("GET /v1/peer/transfers/{id}", s.get)
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
		writeError(writer, http.StatusInternalServerError, "INVENTORY_FAILED", err.Error())
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
	if input.EstimatedBytes < 0 {
		writeError(writer, http.StatusBadRequest, "ESTIMATE_INVALID", "estimated bytes cannot be negative")
		return
	}
	capabilities, err := s.inventory.Inspect(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "INVENTORY_FAILED", err.Error())
		return
	}
	var largestAvailable uint64
	for _, storage := range capabilities.Storage {
		if storage.AvailableBytes > largestAvailable {
			largestAvailable = storage.AvailableBytes
		}
	}
	if input.EstimatedBytes > 0 && uint64(input.EstimatedBytes) > largestAvailable {
		writeError(writer, http.StatusInsufficientStorage, "STORAGE_INSUFFICIENT", "destination has insufficient free storage for the reservation")
		return
	}
	session, err := s.sessions.Reserve(Session{
		ID: input.ID, SourceMachineID: source, WorkloadID: input.WorkloadID,
		EstimatedBytes: input.EstimatedBytes, ExpiresAt: input.ExpiresAt,
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
		writeError(writer, http.StatusInternalServerError, "CHUNK_INVENTORY_FAILED", err.Error())
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
		writeError(writer, http.StatusInternalServerError, "TRANSFER_PERSIST_FAILED", err.Error())
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
	if err := requireState(&session, SessionManifestReady); err != nil {
		writeError(writer, http.StatusConflict, "TRANSFER_STATE_INVALID", err.Error())
		return
	}
	version, err := strconv.ParseUint(request.PathValue("version"), 10, 32)
	if err != nil || uint32(version) != session.KeyVersion {
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
	if err := json.Unmarshal(encoded, &ref); err != nil || ref.Address != request.PathValue("address") || ref.KeyVersion != uint32(version) {
		writeError(writer, http.StatusBadRequest, "CHUNK_REF_INVALID", "chunk reference does not match request path")
		return
	}
	manifest, err := s.checkpoints.Load(session.CheckpointID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "MANIFEST_LOAD_FAILED", err.Error())
		return
	}
	if !manifestContainsChunk(manifest, ref) {
		writeError(writer, http.StatusForbidden, "CHUNK_NOT_AUTHORIZED", "chunk is not referenced by this transfer manifest")
		return
	}
	if session.EstimatedBytes > 0 && session.ReceivedBytes+ref.StoredSize > session.EstimatedBytes+(session.EstimatedBytes/20) {
		writeError(writer, http.StatusInsufficientStorage, "TRANSFER_QUOTA_EXCEEDED", "incoming transfer exceeded its reserved size")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, ref.StoredSize+1)
	inserted, err := s.chunks.ImportChunkResult(request.Context(), session.WorkloadID, ref, request.Body)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "CHUNK_IMPORT_FAILED", err.Error())
		return
	}
	if inserted {
		session, _ = s.sessions.Update(session.ID, source, func(value *Session) error {
			value.ReceivedBytes += ref.StoredSize
			return nil
		})
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
	manifest, err := s.checkpoints.Load(session.CheckpointID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "MANIFEST_LOAD_FAILED", err.Error())
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
		writeError(writer, http.StatusInternalServerError, "TRANSFER_PERSIST_FAILED", err.Error())
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
			writeError(writer, http.StatusInternalServerError, "RESTORE_STATE_MISSING", getErr.Error())
			return
		}
		writeJSON(writer, http.StatusOK, record)
		return
	}
	if err := requireState(&session, SessionVerified); err != nil {
		writeError(writer, http.StatusConflict, "TRANSFER_STATE_INVALID", err.Error())
		return
	}
	timeout := time.Duration(input.TimeoutSeconds) * time.Second
	record, err := s.restorer.Prepare(request.Context(), session.CheckpointID, timeout)
	if err != nil {
		_, _ = s.sessions.Update(session.ID, source, func(value *Session) error {
			value.State = SessionFailed
			value.FailureReason = err.Error()
			return nil
		})
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
		writeError(writer, http.StatusInternalServerError, "TRANSFER_PERSIST_FAILED", err.Error())
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
		writeError(writer, http.StatusInternalServerError, "RESTORE_COMMIT_FAILED", err.Error())
		return
	}
	session, err = s.sessions.Update(session.ID, source, func(value *Session) error {
		value.State = SessionCommitted
		return nil
	})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "TRANSFER_PERSIST_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, session)
}

func (s *Server) rollback(writer http.ResponseWriter, request *http.Request) {
	source := peerMachine(request.Context())
	session, err := s.sessions.Get(request.PathValue("id"), source)
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
			writeError(writer, http.StatusInternalServerError, "RESTORE_ROLLBACK_FAILED", err.Error())
			return
		}
	}
	session, err = s.sessions.Update(session.ID, source, func(value *Session) error {
		value.State = SessionRolledBack
		return nil
	})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "TRANSFER_PERSIST_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, session)
}

func (s *Server) get(writer http.ResponseWriter, request *http.Request) {
	session, err := s.sessions.Get(request.PathValue("id"), peerMachine(request.Context()))
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

func parseInt64Header(request *http.Request, name string) (int64, error) {
	value := strings.TrimSpace(request.Header.Get(name))
	if value == "" {
		return 0, nil
	}
	return strconv.ParseInt(value, 10, 64)
}
