package controlplane

import (
	"errors"
	"net/http"
	"time"

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
	entitlement, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "ENTITLEMENT_NOT_FOUND", "organization entitlement was not found")
			return
		}
		writeError(writer, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err.Error())
		return
	}
	status := storageStatusFrom(entitlement)
	status.Enabled = true
	status.Endpoint = server.storage.configuration.AgentEndpoint()
	status.Bucket = server.storage.configuration.Bucket
	status.Prefix = storagePrefix(organizationID) + "/"
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
			status.Prefix = storagePrefix(organizationID) + "/"
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
// for an agent. This is the quota gate for hosted mirroring: an organization
// past its storage entitlement gets no credentials, its mirror fails, and the
// local checkpoint reports "persisted locally but mirror failed" — local
// checkpointing itself never stops. Exactly-at-quota organizations still
// receive credentials: restores must keep working when the bucket is merely
// full.
func (server *Server) handleStorageCredentials(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	if server.storage == nil {
		writeError(writer, http.StatusServiceUnavailable, "STORAGE_NOT_CONFIGURED", "this control plane does not host checkpoint storage")
		return
	}
	entitlement, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "ENTITLEMENT_NOT_FOUND", "organization entitlement was not found")
			return
		}
		writeError(writer, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err.Error())
		return
	}
	if entitlement.MaxStorageBytes >= 0 && entitlement.UsedStorageBytes > entitlement.MaxStorageBytes {
		writeJSON(writer, http.StatusForbidden, map[string]any{
			"code":               "STORAGE_QUOTA_EXCEEDED",
			"message":            "organization checkpoint storage quota is exceeded; free space or upgrade the plan to resume mirroring",
			"used_storage_bytes": entitlement.UsedStorageBytes,
			"max_storage_bytes":  entitlement.MaxStorageBytes,
		})
		return
	}
	policy := server.storage.sessionPolicy(organizationID)
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
	// credentials themselves.
	principal := principalFrom(request.Context())
	actor := principal.UserID
	if principal.APIKeyID != "" {
		actor = "apikey:" + principal.APIKeyID
	}
	_ = server.database.RecordAudit(request.Context(), server.auditInput(request, "storage.credentials_issued", "organization", organizationID, map[string]string{"principal": actor, "expiration": credentials.Expiration.Format(time.RFC3339)}))
	writeJSON(writer, http.StatusOK, StorageCredentials{
		Endpoint:        server.storage.configuration.AgentEndpoint(),
		Region:          server.storage.configuration.Region,
		Bucket:          server.storage.configuration.Bucket,
		Prefix:          storagePrefix(organizationID),
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
	if _, err := server.database.Entitlement(request.Context(), organizationID); err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "ENTITLEMENT_NOT_FOUND", "organization entitlement was not found")
			return
		}
		writeError(writer, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err.Error())
		return
	}
	seen, err := server.storage.reconcileOrganization(request.Context(), organizationID)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "STORAGE_RECONCILE_FAILED", err.Error())
		return
	}
	if err := server.database.UpdateStorageUsage(request.Context(), organizationID, seen.bytes); err != nil && !errors.Is(err, database.ErrEntitlementMissing) {
		writeError(writer, http.StatusInternalServerError, "STORAGE_RECONCILE_FAILED", err.Error())
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
		writeError(writer, http.StatusInternalServerError, "STORAGE_RECONCILE_FAILED", err.Error())
		return
	}
	entitlement, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err.Error())
		return
	}
	status := storageStatusFrom(entitlement)
	status.Enabled = true
	status.Endpoint = server.storage.configuration.AgentEndpoint()
	status.Bucket = server.storage.configuration.Bucket
	status.Prefix = storagePrefix(organizationID) + "/"
	status.LastReconciledAt = &seen.at
	writeJSON(writer, http.StatusOK, status)
}
