package controlplane

import (
	"encoding/json"

	"shift.dev/shift/internal/database"
)

// This file maps database records to API shapes. Records carry JSONB as raw
// bytes; these mappers are the one place that decodes them, so a nil or
// malformed column degrades to an empty object instead of leaking a null to
// clients.

func userResponse(record database.UserRecord) User {
	return User{ID: record.ID, Email: record.Email, DisplayName: record.DisplayName, DisabledAt: record.DisabledAt, CreatedAt: record.CreatedAt}
}

func organizationResponse(record database.OrganizationRecord) Organization {
	return Organization{ID: record.ID, Name: record.Name, Role: Role(record.Role), CreatedAt: record.CreatedAt}
}

func machineResponse(record database.MachineRecord) Machine {
	var capabilities map[string]any
	_ = json.Unmarshal(record.Capabilities, &capabilities)
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	return Machine{ID: record.ID, OrganizationID: record.OrganizationID, MachineID: record.MachineID, Name: record.Name, AgentURL: record.AgentURL, Capabilities: capabilities, Status: record.Status, LastSeenAt: record.LastSeenAt, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
}

func workloadResponse(record database.WorkloadRecord) Workload {
	var spec, status map[string]any
	_ = json.Unmarshal(record.Spec, &spec)
	_ = json.Unmarshal(record.Status, &status)
	if spec == nil {
		spec = map[string]any{}
	}
	if status == nil {
		status = map[string]any{}
	}
	return Workload{ID: record.ID, OrganizationID: record.OrganizationID, MachineID: record.MachineID, Name: record.Name, Spec: spec, Status: status, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
}

func migrationResponse(record database.MigrationRecord) MigrationJob {
	var progress map[string]any
	_ = json.Unmarshal(record.Progress, &progress)
	if progress == nil {
		progress = map[string]any{}
	}
	return MigrationJob{ID: record.ID, OrganizationID: record.OrganizationID, WorkloadID: record.WorkloadID, SourceMachineID: record.SourceMachineID, DestinationMachineID: record.DestinationMachineID, Mode: record.Mode, Status: record.Status, Progress: progress, ErrorMessage: record.ErrorMessage, CreatedBy: record.CreatedBy, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, CompletedAt: record.CompletedAt}
}

func auditResponse(record database.AuditRecord) AuditEvent {
	var metadata map[string]any
	_ = json.Unmarshal(record.Metadata, &metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	return AuditEvent{ID: record.ID, OrganizationID: record.OrganizationID, ActorUserID: record.ActorUserID, Action: record.Action, ResourceType: record.ResourceType, ResourceID: record.ResourceID, Metadata: metadata, RequestID: record.RequestID, RemoteAddr: record.RemoteAddr, CreatedAt: record.CreatedAt}
}

func apiKeyResponse(record database.APIKeyRecord) APIKey {
	var scopes []string
	_ = json.Unmarshal(record.Scopes, &scopes)
	if scopes == nil {
		scopes = []string{}
	}
	return APIKey{ID: record.ID, OrganizationID: record.OrganizationID, Name: record.Name, Prefix: record.Prefix, Scopes: scopes, ExpiresAt: record.ExpiresAt, LastUsedAt: record.LastUsedAt, RevokedAt: record.RevokedAt, CreatedAt: record.CreatedAt}
}

func checkpointResponse(record database.CheckpointRecord) Checkpoint {
	var manifest map[string]any
	_ = json.Unmarshal(record.Manifest, &manifest)
	if manifest == nil {
		manifest = map[string]any{}
	}
	return Checkpoint{ID: record.ID, OrganizationID: record.OrganizationID, WorkloadID: record.WorkloadID, MachineID: record.MachineID, Kind: record.Kind, ParentID: record.ParentID, Manifest: manifest, PlainBytes: record.PlainBytes, StoredBytes: record.StoredBytes, ChunkCount: record.ChunkCount, Status: record.Status, CreatedAt: record.CreatedAt, DeletedAt: record.DeletedAt}
}
