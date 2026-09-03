// Billing data — plans, prices, limits — comes from the control plane's plan
// catalog (GET /v1/plans), never from constants here. The catalog is the same
// document the server enforces entitlements from, so the UI and the backend
// cannot disagree about what a plan includes.

export const MIGRATION_STAGES = [
  { key: "CREATED", label: "Initialize" },
  { key: "DISCOVER", label: "Discover" },
  { key: "VALIDATE", label: "Validate" },
  { key: "SNAPSHOT", label: "Snapshot" },
  { key: "PREPARE", label: "Prepare" },
  { key: "TRANSFER", label: "Transfer" },
  { key: "VERIFY", label: "Verify" },
  { key: "RESTORE", label: "Restore" },
  { key: "POST_VALIDATE", label: "Verify" },
  { key: "SWITCH", label: "Switch" },
  { key: "COMMIT", label: "Commit" },
  { key: "CLEANUP", label: "Cleanup" },
  { key: "COMPLETED", label: "Complete" },
] as const;

export const STATUS_SERVICES = [
  { key: "control_plane", name: "Control Plane" },
  { key: "api", name: "API" },
  { key: "storage", name: "Storage" },
  { key: "realtime", name: "Realtime" },
  { key: "compute", name: "Compute" },
] as const;
