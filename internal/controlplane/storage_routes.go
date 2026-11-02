package controlplane

import (
	"errors"
	"net/http"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
)

// storage_routes.go: the hosted-storage HTTP surface. Status is what the
// dashboard renders; credentials is what agents poll with their machines-scope
// API key; reconcile lets an administrator force a recount instead of waiting
// for the interval.

// handleStorageStatus reports the organization's hosted-storage state. When
// hosting is off the answer is enabled:false and nothing else — usage numbers
// for that deployment come from the entitlement and usage endpoints, not from
// a quota this control plane does not operate. When hosting is on and the
// cached count is stale, the read refreshes it synchronously — bounded by the
// request context — so the dashboard reflects the bucket, not a snapshot from
// before the last upload.
func (server *Server) handleStorageStatus(writer http.ResponseWriter, request *http.Request) {
	if server.storage == nil {
		writeJSON(writer, http.StatusOK, StorageStatus{Enabled: false})
		return
	}
	organizationID := request.PathValue("organizationID")
	prefix, err := storagePrefix(organizationID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "STORAGE_ORGANIZATION_INVALID", "organization id cannot be used in a storage prefix")
		return
	}
	entitlement, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "ENTITLEMENT_NOT_FOUND", "organization entitlement was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err)
		return
	}
	status := storageStatusFrom(entitlement)
	status.Enabled = true
	status.Endpoint = server.storage.configuration.AgentEndpoint()
	status.Bucket = server.storage.configuration.Bucket
	status.Prefix = prefix + "/"
	if server.storage.stale(organizationID) {
		seen, err := server.storage.reconcileOrganization(request.Context(), organizationID)
		if err != nil {
			// The read still answers with the last persisted numbers; a
			// refresh failure must not turn a status read into an error.
			server.logger.Warn("storage status refresh failed", "organization_id", organizationID, "error", err)
		} else if err := server.database.UpdateStorageUsage(request.Context(), organizationID, seen.bytes); err != nil {
			server.logger.Warn("storage status could not persist usage", "organization_id", organizationID, "error", err)
		} else {
			entitlement.UsedStorageBytes = seen.bytes
			status = storageStatusFrom(entitlement)
			status.Enabled = true
			status.Endpoint = server.storage.configuration.AgentEndpoint()
			status.Bucket = server.storage.configuration.Bucket
			status.Prefix = prefix + "/"
			status.LastReconciledAt = &seen.at
			writeJSON(writer, http.StatusOK, status)
			return
		}
	}
	if seen, ok := server.storage.snapshot(organizationID); ok {
		at := seen.at
		status.LastReconciledAt = &at
	}
	writeJSON(writer, http.StatusOK, status)
}

// storageStatusFrom shapes the entitlement's quota numbers into the status
// response. A negative maximum is the unlimited tier: never over quota.
func storageStatusFrom(entitlement database.EntitlementRecord) StorageStatus {
	return StorageStatus{
		UsedStorageBytes: entitlement.UsedStorageBytes,
		MaxStorageBytes:  entitlement.MaxStorageBytes,
		OverQuota:        entitlement.MaxStorageBytes >= 0 && entitlement.UsedStorageBytes > entitlement.MaxStorageBytes,
	}
}

// handleStorageCredentials mints one short-lived, org-scoped credential set
// for an agent. Storage quota gates what the credential may DO, never whether
// one exists: an organization at or under quota gets a full read/write
// credential, while an organization past its quota gets a read/delete-only
// credential with s3:PutObject removed from the session policy. Every write is
// refused by the credential itself, so the mirror cannot push the org further
// over quota — but restores keep working and DeleteObject stays available,
// because a lockout that also blocks deletion can never end: the same
// credentials are the only way to reach the objects that need freeing. Local
// checkpointing itself never stops either way.
func (server *Server) handleStorageCredentials(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	if server.storage == nil {
		writeError(writer, http.StatusServiceUnavailable, "STORAGE_NOT_CONFIGURED", "this control plane does not host checkpoint storage")
		return
	}
	prefix, err := storagePrefix(organizationID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "STORAGE_ORGANIZATION_INVALID", "organization id cannot be used in a storage prefix")
		return
	}
	// Fair-use transfer throttle (plan 4 BR-01): restore/egress credential
	// issuance is the second enforcement point. Writes are governed by the
	// storage quota check below instead.
	if !server.enforceTransferBudget(writer, request, organizationID) {
		server.recordRefusalEvent(request.Context(), organizationID, "storage_credentials", billing.TransferFairUseExceededCode)
		return
	}
	entitlement, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "ENTITLEMENT_NOT_FOUND", "organization entitlement was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err)
		return
	}
	overQuota := entitlement.MaxStorageBytes >= 0 && entitlement.UsedStorageBytes > entitlement.MaxStorageBytes
	policy := server.storage.sessionPolicy(organizationID, !overQuota)
	if policy == "" {
		writeError(writer, http.StatusInternalServerError, "STORAGE_CREDENTIALS_FAILED", "session policy could not be built")
		return
	}
	credentials, err := server.storage.assumeRole(request.Context(), policy, server.storage.configuration.CredentialTTL)
	if err != nil {
		server.logger.Error("storage credential issuance failed", "organization_id", organizationID, "error", err)
		writeError(writer, http.StatusBadGateway, "STORAGE_CREDENTIALS_FAILED", "the storage service did not issue credentials")
		return
	}
	// The audit record carries who received credentials and when — never the
	// credentials themselves. The mode shows whether the issuance was
	// write-capable, so an operator can see the quota gate doing its job.
	principal := principalFrom(request.Context())
	actor := principal.UserID
	if principal.APIKeyID != "" {
		actor = "apikey:" + principal.APIKeyID
	}
	mode := "full"
	if overQuota {
		mode = "read-delete"
	}
	_ = server.database.RecordAudit(request.Context(), server.auditInput(request, "storage.credentials_issued", "organization", organizationID, map[string]string{"principal": actor, "expiration": credentials.Expiration.Format(time.RFC3339), "mode": mode}))
	writeJSON(writer, http.StatusOK, StorageCredentials{
		Endpoint:        server.storage.configuration.AgentEndpoint(),
		Region:          server.storage.configuration.Region,
		Bucket:          server.storage.configuration.Bucket,
		Prefix:          prefix,
		AccessKeyID:     credentials.AccessKeyID,
		SecretAccessKey: credentials.SecretAccessKey,
		SessionToken:    credentials.SessionToken,
		Expiration:      credentials.Expiration,
		ForcePathStyle:  true,
	})
}

// handleStorageReconcile forces a usage recount for the organization and
// answers with the refreshed status.
func (server *Server) handleStorageReconcile(writer http.ResponseWriter, request *http.Request) {
	if server.storage == nil {
		writeError(writer, http.StatusServiceUnavailable, "STORAGE_NOT_CONFIGURED", "this control plane does not host checkpoint storage")
		return
	}
	organizationID := request.PathValue("organizationID")
	prefix, err := storagePrefix(organizationID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "STORAGE_ORGANIZATION_INVALID", "organization id cannot be used in a storage prefix")
		return
	}
	if _, err := server.database.Entitlement(request.Context(), organizationID); err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "ENTITLEMENT_NOT_FOUND", "organization entitlement was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err)
		return
	}
	seen, err := server.storage.reconcileOrganization(request.Context(), organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusBadGateway, "STORAGE_RECONCILE_FAILED", err)
		return
	}
	if err := server.database.UpdateStorageUsage(request.Context(), organizationID, seen.bytes); err != nil && !errors.Is(err, database.ErrEntitlementMissing) {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "STORAGE_RECONCILE_FAILED", err)
		return
	}
	day := seen.at.UTC().Truncate(24 * time.Hour)
	if err := server.database.UpsertDailyUsage(request.Context(), database.UsageRecord{
		OrganizationID: organizationID,
		Kind:           "checkpoint_storage",
		Quantity:       seen.bytes,
		PeriodStart:    day,
		PeriodEnd:      day,
	}); err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "STORAGE_RECONCILE_FAILED", err)
		return
	}
	entitlement, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err)
		return
	}
	status := storageStatusFrom(entitlement)
	status.Enabled = true
	status.Endpoint = server.storage.configuration.AgentEndpoint()
	status.Bucket = server.storage.configuration.Bucket
	status.Prefix = prefix + "/"
	status.LastReconciledAt = &seen.at
	writeJSON(writer, http.StatusOK, status)
}
