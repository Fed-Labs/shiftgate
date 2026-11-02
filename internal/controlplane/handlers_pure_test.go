package controlplane

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/scheduler"
)

// TestServerNameFromAgentURL covers the TLS server-name derivation used when
// a migration destination is addressed by IP or alias.
func TestServerNameFromAgentURL(t *testing.T) {
	cases := map[string]string{
		"https://agent.example:8443": "agent.example",
		"https://10.0.0.5:8443":      "10.0.0.5",
		"https://agent.example":      "agent.example",
		"":                           "",
		"://bad":                     "",
		"agent.example":              "",
	}
	for input, want := range cases {
		if got := serverNameFromAgentURL(input); got != want {
			t.Fatalf("serverNameFromAgentURL(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestMigrationJobStatus maps the agent's persisted stage vocabulary onto the
// control-plane job status the dashboard unions expect.
func TestMigrationJobStatus(t *testing.T) {
	cases := map[model.MigrationStage]string{
		model.MigrationCompleted:  "completed",
		model.MigrationFailed:     "failed",
		model.MigrationRolledBack: "failed",
		model.MigrationCancelled:  "cancelled",
		model.MigrationCreated:    "running",
		model.MigrationTransfer:   "running",
		model.MigrationCleanup:    "running",
	}
	for stage, want := range cases {
		if got := migrationJobStatus(stage); got != want {
			t.Fatalf("migrationJobStatus(%q) = %q, want %q", stage, got, want)
		}
	}
}

// TestMustJSONNeverPanics: the helper is called on paths that must not fail
// the request, so it substitutes an error document instead of throwing.
func TestMustJSONNeverPanics(t *testing.T) {
	encoded := mustJSON(map[string]string{"a": "b"})
	var decoded map[string]string
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded["a"] != "b" {
		t.Fatalf("mustJSON round trip = %s, err=%v", encoded, err)
	}
	broken := mustJSON(make(chan int))
	if !strings.Contains(string(broken), "error") {
		t.Fatalf("mustJSON on unmarshalable value = %s", broken)
	}
}

// TestOfferFromRecordColumnsWinOverJSON is the compute scheduler's data
// invariant: the SQL status/visibility columns are what filtering trusts, so
// they must override whatever stale JSON the row carries.
func TestOfferFromRecordColumnsWinOverJSON(t *testing.T) {
	record := database.ComputeOfferRecord{
		ID:             "offer-1",
		OrganizationID: "org-1",
		MachineID:      "mach-1",
		MachineName:    "node",
		AgentURL:       "https://node.example:8443",
		Trust:          "trusted",
		Status:         "online",
		Visibility:     "public",
		LastSeenAt:     func() *time.Time { now := time.Now().UTC(); return &now }(),
		Exposed:        json.RawMessage(`{"cpu_count":8,"memory_bytes":1024}`),
		Policy:         json.RawMessage(`{"visibility":"private","max_price_cents":5}`),
		Availability:   json.RawMessage(`{"status":"offline"}`),
	}
	offer, err := offerFromRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(offer.Availability.Status) != "online" {
		t.Fatalf("column status lost to JSON copy: %q", offer.Availability.Status)
	}
	if string(offer.Policy.Visibility) != "public" {
		t.Fatalf("column visibility lost to JSON copy: %q", offer.Policy.Visibility)
	}
	if offer.Exposed.CPUCount != 8 || offer.Exposed.MemoryBytes != 1024 {
		t.Fatalf("exposed resources not decoded: %+v", offer.Exposed)
	}
	// Unreadable stored JSON is a hard error, never a silently empty offer.
	record.Policy = json.RawMessage(`{not-json`)
	if _, err := offerFromRecord(record); err == nil {
		t.Fatal("unreadable policy JSON accepted")
	}
}

// TestComputeOfferResponseCapacity: the API's available capacity derives
// from exposed minus committed, so the dashboard cannot offer more than the
// machine still has.
func TestComputeOfferResponseCapacity(t *testing.T) {
	offer := scheduler.Offer{ID: "offer-2"}
	offer.Exposed = scheduler.Resources{CPUCount: 8, MemoryBytes: 1024}
	offer.Committed = scheduler.Resources{CPUCount: 3}
	response := computeOfferResponse(offer, nil, "")
	if response.Available.CPUCount != 5 {
		t.Fatalf("available cpu = %v, want 5", response.Available.CPUCount)
	}
	withdrawn := time.Now().UTC()
	response = computeOfferResponse(offer, &withdrawn, "")
	if response.WithdrawnAt == nil {
		t.Fatal("withdrawal timestamp lost")
	}
}

// TestDecodeJSONRejectsMalformedBodies: the shared request decoder is the
// input boundary for every JSON handler.
func TestDecodeJSONRejectsMalformedBodies(t *testing.T) {
	writer := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/v1/test", strings.NewReader("{not json"))
	var target map[string]any
	if decodeJSON(writer, request, &target) {
		t.Fatal("malformed body accepted")
	}
	if writer.Code != 400 {
		t.Fatalf("status = %d, want 400", writer.Code)
	}
	writer = httptest.NewRecorder()
	request = httptest.NewRequest("POST", "/v1/test", strings.NewReader(`{"a":1}`))
	if !decodeJSON(writer, request, &target) {
		t.Fatalf("valid body rejected: %d %s", writer.Code, writer.Body.String())
	}
}
