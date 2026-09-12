package network

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"shift.dev/shift/internal/model"
)

// StatusDocumentName is the file, inside the workload's state directory, that
// tells a restored application what actually happened to its network. A process
// restored by CRIU inherits the environment of the image it was checkpointed
// from — environment variables cannot be rewritten — so the status is delivered
// as a file the application (or its wrapper script) can read and, if it cares,
// act on: reconnect pools, drop stale peers, or surface the fact to its users.
const StatusDocumentName = "migration-status.json"

// Operation values recorded in the status document.
const (
	OperationMigration = "migration"
	OperationRestore   = "restore"
	OperationFork      = "fork"
	OperationClone     = "clone"
)

// Status outcome values.
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// StatusDocument is the machine-readable account of one operation's network
// outcome. Every field states what happened, never what would be nice: if
// sockets were not carried, SocketsPreserved is false, and any application
// reading the document knows its old connections are gone.
type StatusDocument struct {
	// SchemaVersion lets consumers evolve independently of the checkpoint
	// format version.
	SchemaVersion int `json:"schema_version"`
	// OperationID identifies the migration (source) or restore session
	// (destination) this document reports on.
	OperationID string `json:"operation_id"`
	// Operation is "migration" or "restore".
	Operation string `json:"operation"`
	// Outcome is "completed" or "failed".
	Outcome string `json:"outcome"`
	// Policy the workload declared.
	Policy string `json:"policy"`
	// VirtualIP is the workload's stable virtual address.
	VirtualIP string `json:"virtual_ip"`
	// SocketsPreserved reports whether kernel TCP state was restored. It is
	// set only when the restore actually carried the sockets; a preserve
	// policy whose restore did not carry them reports false here.
	SocketsPreserved bool `json:"sockets_preserved"`
	// ConnectionsDropped tells the application its pre-migration
	// connections are gone and peers must reconnect.
	ConnectionsDropped bool `json:"connections_dropped"`
	// Ports restates each listener's final state at this machine.
	Ports []model.NetworkPortStatus `json:"ports"`
	// ForwardedPorts lists host ports now served by SHIFT forwarders.
	ForwardedPorts []int `json:"forwarded_ports"`
	// RecordedAt is when the document was written.
	RecordedAt time.Time `json:"recorded_at"`
	// Detail carries the failure reason when Outcome is "failed".
	Detail string `json:"detail,omitempty"`
}

// WriteStatus writes the status document into a workload's .shift directory,
// creating it if needed, with owner-only permissions. The write is atomic: the
// document is staged and renamed, so an application can never observe a partial
// status file, and a crash mid-write leaves the previous one intact.
func WriteStatus(root string, document StatusDocument) (string, error) {
	if strings.TrimSpace(document.OperationID) == "" {
		return "", fmt.Errorf("migration status requires an operation id")
	}
	switch document.Operation {
	case OperationMigration, OperationRestore, OperationFork, OperationClone:
	default:
		return "", fmt.Errorf("migration status operation %q is not recognized", document.Operation)
	}
	if document.Outcome != StatusCompleted && document.Outcome != StatusFailed {
		return "", fmt.Errorf("migration status outcome %q is not recognized", document.Outcome)
	}
	if document.SchemaVersion == 0 {
		document.SchemaVersion = 1
	}
	if document.RecordedAt.IsZero() {
		document.RecordedAt = time.Now().UTC()
	}
	if document.Ports == nil {
		document.Ports = []model.NetworkPortStatus{}
	}
	if document.ForwardedPorts == nil {
		document.ForwardedPorts = []int{}
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode migration status: %w", err)
	}
	directory := filepath.Join(root, ".shift")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create status directory: %w", err)
	}
	path := filepath.Join(directory, StatusDocumentName)
	staged := path + ".tmp"
	if err := os.WriteFile(staged, append(encoded, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("stage migration status: %w", err)
	}
	if err := os.Rename(staged, path); err != nil {
		_ = os.Remove(staged)
		return "", fmt.Errorf("publish migration status: %w", err)
	}
	return path, nil
}

// ReadStatus loads the status document from a workload root. The error wraps
// fs.ErrNotExist when no migration has been recorded, so callers can
// distinguish "never migrated" from "status unreadable".
func ReadStatus(root string) (StatusDocument, error) {
	raw, err := os.ReadFile(filepath.Join(root, ".shift", StatusDocumentName))
	if err != nil {
		return StatusDocument{}, fmt.Errorf("read migration status: %w", err)
	}
	var document StatusDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return StatusDocument{}, fmt.Errorf("parse migration status: %w", err)
	}
	return document, nil
}
