package agent

// standby_test.go pins the supervisor's decision table. Automatic failover
// needs two agreeing signals — a stale or offline control-plane record AND a
// source peer listener that fails to answer — and every other combination
// leaves the duty alone: a fresh record, a missing record, a source that
// answers, a probe that cannot even run. A failing restore backs off instead
// of hammering the machine every tick, and the held/withdrawn callbacks keep
// the duty records honest about who the recorded source is.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/controlplane"
	"shift.dev/shift/internal/fencing"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/persistence"
	"shift.dev/shift/internal/transfer"
)

// The presence query's organization and machines-scope key. The key is a
// test dummy, but it travels the same bearer header the real one does and
// the fake control plane checks it, so a regression that drops or mislabels
// it fails here.
const (
	standbyTestOrganization = "org-standby-test"
	standbyTestAPIKey       = "standby-test-api-key-0123456789"
	// unreachablePeer is an https URL nothing will ever answer: port 1 on
	// loopback refuses the connection immediately.
	unreachablePeer = "https://127.0.0.1:1"
)

// standbyPresence answers the supervisor's org-wide presence query the way
// the real control plane does: a bare JSON array of machine records behind
// the machines-scope bearer key.
type standbyPresence struct {
	mu       sync.Mutex
	machines []controlplane.Machine
	hits     int
	server   *httptest.Server
}

func newStandbyPresence(t *testing.T) *standbyPresence {
	t.Helper()
	fake := &standbyPresence{}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *standbyPresence) handle(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer "+standbyTestAPIKey {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	if request.Method == http.MethodPost && request.URL.Path == "/v1/organizations/"+standbyTestOrganization+"/ownership/acquire" {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"epoch": 1, "token": "test-ownership-token"})
		return
	}
	if request.Method == http.MethodPost && request.URL.Path == "/v1/organizations/"+standbyTestOrganization+"/ownership/fence" {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"state": "fenced", "epoch": 1})
		return
	}
	if request.Method != http.MethodGet || request.URL.Path != "/v1/organizations/"+standbyTestOrganization+"/machines" {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	f.mu.Lock()
	f.hits++
	machines := append([]controlplane.Machine(nil), f.machines...)
	f.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(machines)
}

func (f *standbyPresence) set(machines ...controlplane.Machine) {
	f.mu.Lock()
	f.machines = append([]controlplane.Machine(nil), machines...)
	f.mu.Unlock()
}

func (f *standbyPresence) queries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// sourcePeer is a source's peer listener: everything these tests need it to
// do is answer the machine probe, and count that it was asked.
type sourcePeer struct {
	mu     sync.Mutex
	hits   int
	server *httptest.Server
}

func newSourcePeer(t *testing.T) *sourcePeer {
	t.Helper()
	fake := &sourcePeer{}
	fake.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/peer/machine" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		fake.mu.Lock()
		fake.hits++
		fake.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte("{}"))
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *sourcePeer) probes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// seenAt builds an online machine record with the given last-seen time.
func seenAt(machineID string, lastSeen time.Time) controlplane.Machine {
	return controlplane.Machine{MachineID: machineID, Status: "online", LastSeenAt: &lastSeen}
}

// openStandbyTestService opens a development agent with the identity, chunk
// store, and checkpoint repository a standby needs, pointed at the given
// control plane. An empty controlURL leaves the agent unable to confirm any
// source's death — the state `standby list` then reports honestly.
func openStandbyTestService(t *testing.T, controlURL string) *Service {
	t.Helper()
	configuration := config.DefaultAgent()
	configuration.StateDir = t.TempDir()
	configuration.Listen = "unix://" + configuration.StateDir + "/agent.sock"
	// A remote listener is what makes Open provision the development TLS
	// identity the probe client dials with.
	configuration.RemoteListen = "tcp://127.0.0.1:0"
	configuration.InsecureDevelopment = true
	// Fencing commands must live in an allow-listed directory; give each test
	// its own directory with a no-op fence in it and aim the allow-list there.
	fenceDir := t.TempDir()
	fencePath := filepath.Join(fenceDir, "fence.sh")
	if err := os.WriteFile(fencePath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	previousFencingDirs := fencing.DefaultFencingDirs
	fencing.DefaultFencingDirs = []string{fenceDir}
	t.Cleanup(func() { fencing.DefaultFencingDirs = previousFencingDirs })
	configuration.Fencing.Provider = "command"
	configuration.Fencing.Command = fencePath
	configuration.Fencing.StatusCommand = fencePath
	if controlURL != "" {
		configuration.ControlPlane.URL = controlURL
		configuration.ControlPlane.OrganizationID = standbyTestOrganization
		configuration.ControlPlane.MachineID = "machine-standby"
		configuration.ControlPlane.APIKey = standbyTestAPIKey
	}
	service, err := Open(configuration, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// armedDuty is a duty the supervisor should be watching, holding a
// checkpoint id the repository does not have — so any restore attempt the
// supervisor should not make fails loudly and lands on the duty.
func armedDuty(workloadID, source, agentURL, checkpointID string) model.StandbyDuty {
	held := time.Now().UTC().Add(-time.Hour)
	return model.StandbyDuty{
		WorkloadID: workloadID, SourceMachineID: source, SourceAgentURL: agentURL,
		LastCheckpointID: checkpointID, State: model.StandbyArmed,
		HeldAt: held, LastCheckpointAt: held, UpdatedAt: held,
	}
}

// duty reads a duty back the way the tick loop sees it.
func duty(t *testing.T, service *Service, workloadID string) model.StandbyDuty {
	t.Helper()
	record, err := service.standby.duties.Get(workloadID)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// assertNoAttempt pins that a tick left an armed duty untouched: no failure
// recorded, no retry scheduled.
func assertNoAttempt(t *testing.T, service *Service, workloadID string) {
	t.Helper()
	record := duty(t, service, workloadID)
	if record.LastFailoverError != "" || !record.NextAttemptAt.IsZero() {
		t.Fatalf("duty %s must stay untouched, got error %q next attempt %s",
			workloadID, record.LastFailoverError, record.NextAttemptAt)
	}
}

// fabricateCheckpoint stores a real, signed checkpoint and its chunks in the
// service's own stores, the way a local checkpoint (or an imported
// replication push) leaves them. A non-empty parentID marks it incremental.
func fabricateCheckpoint(t *testing.T, service *Service, workload model.WorkloadSpec, checkpointID, parentID string) model.CheckpointManifest {
	t.Helper()
	plaintext := bytes.Repeat([]byte("standby duty payload\n"), 200)
	asset, err := service.chunks.PutAsset(context.Background(), workload.ID, "memory",
		"application/octet-stream", bytes.NewReader(plaintext), true, 1)
	if err != nil {
		t.Fatal(err)
	}
	manifest := model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		ID: checkpointID, Kind: model.CheckpointFull, ParentID: parentID, CreatedAt: time.Now().UTC(),
		Workload: workload,
		Assets:   []model.AssetManifest{asset.Asset},
		Metrics: model.CheckpointMetrics{
			PlainBytes: asset.Asset.PlainSize, StoredBytes: asset.Asset.StoredSize,
			ChunkCount: len(asset.Asset.Chunks),
		},
		Security: model.SecurityEnvelope{KeyVersion: asset.Asset.Chunks[0].KeyVersion},
	}
	if err := checkpoint.SignManifest(&manifest, service.identity); err != nil {
		t.Fatal(err)
	}
	if err := service.repository.Save(manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

// TestStandbyTickTrustsAFreshRecord: a source whose heartbeat is current is
// alive as far as the supervisor is concerned — and its peer listener is
// never even probed.
func TestStandbyTickTrustsAFreshRecord(t *testing.T) {
	control := newStandbyPresence(t)
	service := openStandbyTestService(t, control.server.URL)
	source := newSourcePeer(t)
	control.set(seenAt("machine-source", time.Now()))
	if err := service.standby.duties.Put("workload-1", armedDuty("workload-1", "machine-source", source.server.URL, "checkpoint-missing")); err != nil {
		t.Fatal(err)
	}
	service.standby.tick(context.Background())
	assertNoAttempt(t, service, "workload-1")
	if source.probes() != 0 {
		t.Fatalf("a fresh record must not be probed, saw %d probes", source.probes())
	}
	if control.queries() != 1 {
		t.Fatalf("one tick means one presence query, saw %d", control.queries())
	}
}

// TestStandbyTickNeedsARecordOfTheSource: a source the control plane has no
// record of cannot be confirmed dead — a missing record is not a death, and
// the duty waits.
func TestStandbyTickNeedsARecordOfTheSource(t *testing.T) {
	control := newStandbyPresence(t)
	service := openStandbyTestService(t, control.server.URL)
	control.set(seenAt("machine-other", time.Now()))
	if err := service.standby.duties.Put("workload-1", armedDuty("workload-1", "machine-source", unreachablePeer, "checkpoint-missing")); err != nil {
		t.Fatal(err)
	}
	service.standby.tick(context.Background())
	assertNoAttempt(t, service, "workload-1")
	if control.queries() != 1 {
		t.Fatalf("expected one presence query, saw %d", control.queries())
	}
}

// TestStandbyTickPrefersTheSourceWhenItAnswers: a stale record plus a source
// that still answers its peer listener is a partitioned control plane, not a
// dead source. The probe wins and the duty waits.
func TestStandbyTickPrefersTheSourceWhenItAnswers(t *testing.T) {
	control := newStandbyPresence(t)
	service := openStandbyTestService(t, control.server.URL)
	source := newSourcePeer(t)
	control.set(seenAt("machine-source", time.Now().Add(-5*time.Minute)))
	if err := service.standby.duties.Put("workload-1", armedDuty("workload-1", "machine-source", source.server.URL, "checkpoint-missing")); err != nil {
		t.Fatal(err)
	}
	service.standby.tick(context.Background())
	assertNoAttempt(t, service, "workload-1")
	if source.probes() != 1 {
		t.Fatalf("a stale record must be probed exactly once, saw %d", source.probes())
	}
}

// TestStandbyTickFailsOverOnTwoSignals: a stale heartbeat and an unreachable
// peer listener — sustained across consecutive passes, the supervisor
// attempts the restore. Each duty names a checkpoint no repository holds, so
// the attempts fail and land on the duties where `standby list` shows them.
// Both a stale heartbeat and an announced offline count as the control
// plane's signal.
func TestStandbyTickFailsOverOnTwoSignals(t *testing.T) {
	control := newStandbyPresence(t)
	service := openStandbyTestService(t, control.server.URL)
	fresh := time.Now()
	offline := controlplane.Machine{MachineID: "machine-announced", Status: "offline", LastSeenAt: &fresh}
	control.set(seenAt("machine-source", fresh.Add(-5*time.Minute)), offline)
	duties := map[string]string{
		"workload-stale":     "machine-source",
		"workload-announced": "machine-announced",
	}
	for workload, source := range duties {
		if err := service.standby.duties.Put(workload, armedDuty(workload, source, unreachablePeer, "checkpoint-missing")); err != nil {
			t.Fatal(err)
		}
	}
	// Death is a sustained observation: the signals must hold for
	// deathSampleTarget consecutive passes before anything is attempted.
	for pass := 0; pass < deathSampleTarget; pass++ {
		service.standby.tick(context.Background())
	}
	for workload := range duties {
		record := duty(t, service, workload)
		if !strings.Contains(record.LastFailoverError, "restore prepare") {
			t.Fatalf("%s: the attempt must be recorded with its failure, got %q", workload, record.LastFailoverError)
		}
		if !record.NextAttemptAt.After(time.Now()) || record.State != model.StandbyArmed {
			t.Fatalf("%s: a failed attempt must stay armed with a retry scheduled: %+v", workload, record)
		}
	}
}

// TestStandbyTickWaitsForSustainedDeath: one failed probe is a blip, not a
// death. A transient stall of a slow-but-alive source must never reach the
// fence — only a source that stays unreachable across consecutive passes
// (here: deathSampleTarget of them) does.
func TestStandbyTickWaitsForSustainedDeath(t *testing.T) {
	control := newStandbyPresence(t)
	service := openStandbyTestService(t, control.server.URL)
	control.set(seenAt("machine-source", time.Now().Add(-5*time.Minute)))
	if err := service.standby.duties.Put("workload-1", armedDuty("workload-1", "machine-source", unreachablePeer, "checkpoint-missing")); err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass < deathSampleTarget; pass++ {
		service.standby.tick(context.Background())
		assertNoAttempt(t, service, "workload-1")
	}
	service.standby.tick(context.Background())
	if record := duty(t, service, "workload-1"); record.LastFailoverError == "" {
		t.Fatal("the sustained observation must lead to the fenced attempt")
	}
}

// TestStandbyDeathCountResetsOnContraryEvidence: a source that answers its
// peer listener again (or resumes heartbeating) erases whatever death
// observation had accumulated — the next stall starts from zero.
func TestStandbyDeathCountResetsOnContraryEvidence(t *testing.T) {
	control := newStandbyPresence(t)
	service := openStandbyTestService(t, control.server.URL)
	source := newSourcePeer(t)
	control.set(seenAt("machine-source", time.Now().Add(-5*time.Minute)))
	if err := service.standby.duties.Put("workload-1", armedDuty("workload-1", "machine-source", source.server.URL, "checkpoint-missing")); err != nil {
		t.Fatal(err)
	}
	// The source answers: probe success resets the count every pass, so no
	// number of passes can accumulate a death observation.
	for pass := 0; pass < deathSampleTarget+1; pass++ {
		service.standby.tick(context.Background())
		assertNoAttempt(t, service, "workload-1")
	}
	if samples := service.standby.deathSamples["workload-1"]; samples != 0 {
		t.Fatalf("a source that answers must leave no death observation, got %d", samples)
	}
	// A fresh heartbeat record likewise clears the count.
	control.set(seenAt("machine-source", time.Now()))
	service.standby.deathSamples["workload-1"] = 2
	service.standby.tick(context.Background())
	if samples := service.standby.deathSamples["workload-1"]; samples != 0 {
		t.Fatalf("a heartbeating source must clear the death observation, got %d", samples)
	}
}

// TestStandbyTickBacksOffAfterAFailedAttempt: an attempt that failed is not
// retried on the next tick — the duty's next-attempt time holds.
func TestStandbyTickBacksOffAfterAFailedAttempt(t *testing.T) {
	control := newStandbyPresence(t)
	service := openStandbyTestService(t, control.server.URL)
	control.set(seenAt("machine-source", time.Now().Add(-5*time.Minute)))
	if err := service.standby.duties.Put("workload-1", armedDuty("workload-1", "machine-source", unreachablePeer, "checkpoint-missing")); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < deathSampleTarget; pass++ {
		service.standby.tick(context.Background())
	}
	first := duty(t, service, "workload-1")
	if first.NextAttemptAt.IsZero() {
		t.Fatal("the first failed attempt must schedule a retry")
	}
	service.standby.tick(context.Background())
	second := duty(t, service, "workload-1")
	if second.LastFailoverError != first.LastFailoverError {
		t.Fatalf("backoff must keep the original failure, got %q then %q",
			first.LastFailoverError, second.LastFailoverError)
	}
	if pushed := second.NextAttemptAt.Sub(first.NextAttemptAt); pushed > time.Second {
		t.Fatalf("an immediate re-tick must not re-attempt; the retry was pushed %s", pushed)
	}
}

// TestStandbyWithoutAControlPlaneNeverActsAlone: with no control plane there
// is no presence signal at all, so the supervisor cannot confirm a death and
// leaves every duty alone — no matter how dead the source looks.
func TestStandbyWithoutAControlPlaneNeverActsAlone(t *testing.T) {
	service := openStandbyTestService(t, "")
	if service.standby.automaticFailover() {
		t.Fatal("automatic failover must be impossible without a control plane")
	}
	if err := service.standby.duties.Put("workload-1", armedDuty("workload-1", "machine-source", unreachablePeer, "checkpoint-missing")); err != nil {
		t.Fatal(err)
	}
	service.standby.tick(context.Background())
	assertNoAttempt(t, service, "workload-1")
}

// TestStandbyTickIgnoresAProbeItCannotRun: a standby that cannot even build
// a probe client has learned nothing about the source — its own
// misconfiguration must never fail a workload over.
func TestStandbyTickIgnoresAProbeItCannotRun(t *testing.T) {
	control := newStandbyPresence(t)
	service := openStandbyTestService(t, control.server.URL)
	control.set(seenAt("machine-source", time.Now().Add(-5*time.Minute)))
	service.standby.peerClient = func(string) (*transfer.Client, error) {
		return nil, errors.New("no client identity on this standby")
	}
	if err := service.standby.duties.Put("workload-1", armedDuty("workload-1", "machine-source", unreachablePeer, "checkpoint-missing")); err != nil {
		t.Fatal(err)
	}
	service.standby.tick(context.Background())
	assertNoAttempt(t, service, "workload-1")
}

// TestReplicationHeldBecomesAnArmedDuty: a held session carrying a real
// checkpoint becomes the duty with the facts an operator reads — name, uid,
// retention, and where the source lives.
func TestReplicationHeldBecomesAnArmedDuty(t *testing.T) {
	service := openStandbyTestService(t, "")
	workload := model.WorkloadSpec{ID: "workload-1", Name: "protected", RootPath: "/srv/protected", UID: 4242}
	manifest := fabricateCheckpoint(t, service, workload, "checkpoint-held", "")
	service.standby.ReplicationHeld(transfer.Session{
		ID: "session-1", WorkloadID: "workload-1", CheckpointID: manifest.ID,
		SourceMachineID: "machine-source", SourceAgentURL: "https://source:9443",
		Purpose: transfer.PurposeReplication, State: transfer.SessionHeld, KeepLast: 3,
		UpdatedAt: time.Now().UTC(),
	})
	record := duty(t, service, "workload-1")
	if record.State != model.StandbyArmed || record.LastCheckpointID != manifest.ID {
		t.Fatalf("duty after hold: %+v", record)
	}
	if record.WorkloadName != "protected" || record.WorkloadUID != 4242 {
		t.Fatalf("duty must carry the workload's name and uid, got %q uid %d",
			record.WorkloadName, record.WorkloadUID)
	}
	if record.KeepLast != 3 || record.SourceAgentURL != "https://source:9443" || record.SourceMachineID != "machine-source" {
		t.Fatalf("duty facts after hold: %+v", record)
	}
	// Sessions that are not held replications must not touch the duty.
	for _, session := range []transfer.Session{
		{ID: "s2", WorkloadID: "workload-1", CheckpointID: "checkpoint-other",
			Purpose: transfer.PurposeReplication, State: transfer.SessionVerified, UpdatedAt: time.Now().UTC()},
		{ID: "s3", WorkloadID: "workload-1", CheckpointID: "checkpoint-other",
			Purpose: "", State: transfer.SessionHeld, UpdatedAt: time.Now().UTC()},
	} {
		service.standby.ReplicationHeld(session)
	}
	if after := duty(t, service, "workload-1"); after.LastCheckpointID != manifest.ID {
		t.Fatalf("non-replication holds must not move the duty, got checkpoint %s", after.LastCheckpointID)
	}
}

// TestReplicationHeldKeepsAFailedOverDuty: a source pushing state for a
// workload that already failed over here is alive again after the death its
// own failover assumed — the duty is history and must not be silently
// re-armed.
func TestReplicationHeldKeepsAFailedOverDuty(t *testing.T) {
	service := openStandbyTestService(t, "")
	workload := model.WorkloadSpec{ID: "workload-1", Name: "protected", RootPath: "/srv/protected"}
	manifest := fabricateCheckpoint(t, service, workload, "checkpoint-arrived", "")
	failed := armedDuty("workload-1", "machine-source", "https://source:9443", "checkpoint-old")
	failed.State = model.StandbyFailedOver
	failed.FailoverAt = time.Now().UTC().Add(-time.Minute)
	failed.FailoverReason = "operator command"
	if err := service.standby.duties.Put("workload-1", failed); err != nil {
		t.Fatal(err)
	}
	service.standby.ReplicationHeld(transfer.Session{
		ID: "session-1", WorkloadID: "workload-1", CheckpointID: manifest.ID,
		SourceMachineID: "machine-source", Purpose: transfer.PurposeReplication,
		State: transfer.SessionHeld, UpdatedAt: time.Now().UTC(),
	})
	record := duty(t, service, "workload-1")
	if record.State != model.StandbyFailedOver || record.LastCheckpointID != "checkpoint-old" {
		t.Fatalf("a failed-over duty must keep its history, got %+v", record)
	}
}

// TestReplicationWithdrawnHonorsTheSource: only the recorded source's
// withdrawal drops an armed duty — a stranger's is ignored, and a workload
// that already failed over keeps its duty as the failover's history.
func TestReplicationWithdrawnHonorsTheSource(t *testing.T) {
	service := openStandbyTestService(t, "")
	for workload, state := range map[string]string{
		"workload-stranger": model.StandbyArmed,
		"workload-armed":    model.StandbyArmed,
		"workload-history":  model.StandbyFailedOver,
	} {
		record := armedDuty(workload, "machine-source", "https://source:9443", "checkpoint-1")
		record.State = state
		if err := service.standby.duties.Put(workload, record); err != nil {
			t.Fatal(err)
		}
	}
	service.standby.ReplicationWithdrawn("machine-stranger", "workload-stranger")
	if _, err := service.standby.duties.Get("workload-stranger"); err != nil {
		t.Fatalf("a stranger's withdrawal must be ignored: %v", err)
	}
	service.standby.ReplicationWithdrawn("machine-source", "workload-armed")
	if _, err := service.standby.duties.Get("workload-armed"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("the source's withdrawal must drop the duty, got %v", err)
	}
	service.standby.ReplicationWithdrawn("machine-source", "workload-history")
	if record, err := service.standby.duties.Get("workload-history"); err != nil || record.State != model.StandbyFailedOver {
		t.Fatalf("a failed-over duty must survive withdrawal, got %v %+v", err, record)
	}
	// Withdrawing a duty that does not exist is a no-op, not an error.
	service.standby.ReplicationWithdrawn("machine-source", "workload-unknown")
}

// TestStandbyTriggerNeedsADuty: the explicit trigger refuses only what does
// not exist — everything else about failing over is the operator's call.
func TestStandbyTriggerNeedsADuty(t *testing.T) {
	service := openStandbyTestService(t, "")
	_, err := service.standby.Trigger(context.Background(), "workload-unknown", "", false, "operator command")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("trigger on an unknown duty must fail with not-found, got %v", err)
	}
}

// TestPresenceJudgments pins the offline test itself: an announced offline, a
// stale heartbeat, a current heartbeat, and no heartbeat at all.
func TestPresenceJudgments(t *testing.T) {
	fresh := time.Now()
	stale := fresh.Add(-5 * time.Minute)
	offline := controlplane.Machine{Status: "offline", LastSeenAt: &fresh}
	if !presenceOffline(offline) || presenceDescription(offline) != "offline" {
		t.Fatal("a machine that announced offline must count as offline")
	}
	staleSeen := controlplane.Machine{Status: "online", LastSeenAt: &stale}
	if !presenceOffline(staleSeen) || !strings.Contains(presenceDescription(staleSeen), "online but last seen") {
		t.Fatalf("a stale heartbeat must count as offline: %q", presenceDescription(staleSeen))
	}
	if presenceOffline(controlplane.Machine{Status: "online", LastSeenAt: &fresh}) {
		t.Fatal("a current heartbeat must count as online")
	}
	noHeartbeat := controlplane.Machine{Status: "online"}
	if presenceOffline(noHeartbeat) {
		t.Fatal("a machine with no last-seen timestamp must not be assumed offline")
	}
	if presenceDescription(noHeartbeat) != "online with no last-seen timestamp" {
		t.Fatalf("description for a machine without a heartbeat: %q", presenceDescription(noHeartbeat))
	}
}

// failingDutyStore makes duty persistence fail on demand — the seam for
// "a hold whose duty cannot be persisted must not be answered as durable".
type failingDutyStore struct {
	dutyStore
}

func (failingDutyStore) Put(string, model.StandbyDuty) error {
	return errors.New("duty store is unavailable")
}

// TestStandbyFailureBookkeepingCannotClobberConcurrentRecords: every duty
// write is a read-modify-write against the freshly stored record. A failure
// write from a stale tick snapshot must not revert a concurrent hold's newer
// checkpoint, and must never overwrite a completed failover's history.
func TestStandbyFailureBookkeepingCannotClobberConcurrentRecords(t *testing.T) {
	service := openStandbyTestService(t, "")
	workload := model.WorkloadSpec{ID: "workload-1", Name: "protected", RootPath: "/srv/protected"}
	oldCheckpoint := fabricateCheckpoint(t, service, workload, "checkpoint-old", "")
	newCheckpoint := fabricateCheckpoint(t, service, workload, "checkpoint-new", "")
	if err := service.standby.duties.Put("workload-1", armedDuty("workload-1", "machine-source", unreachablePeer, oldCheckpoint.ID)); err != nil {
		t.Fatal(err)
	}
	// A hold for a newer checkpoint lands between the tick's snapshot and
	// the failure write...
	service.standby.ReplicationHeld(transfer.Session{
		ID: "session-1", WorkloadID: "workload-1", CheckpointID: newCheckpoint.ID,
		SourceMachineID: "machine-source", SourceAgentURL: "https://source:9443",
		Purpose: transfer.PurposeReplication, State: transfer.SessionHeld, UpdatedAt: time.Now().UTC(),
	})
	// ...and the stale failure write must not revert it.
	service.standby.recordAttemptFailure("workload-1", errors.New("restore prepare: boom"))
	record := duty(t, service, "workload-1")
	if record.LastCheckpointID != newCheckpoint.ID {
		t.Fatalf("a stale failure write reverted the hold to %s", record.LastCheckpointID)
	}
	if record.LastFailoverError == "" {
		t.Fatal("the failure must still be recorded on the fresh record")
	}

	// A completed failover is history: a late failure write must not
	// resurrect the duty or replace the operator's reason.
	if _, err := service.standby.markFailedOver("workload-1", "restore-1", "operator commanded failover"); err != nil {
		t.Fatal(err)
	}
	service.standby.recordAttemptFailure("workload-1", errors.New("stale"))
	record = duty(t, service, "workload-1")
	if record.State != model.StandbyFailedOver || record.FailoverReason != "operator commanded failover" || record.FailoverRestoreID != "restore-1" {
		t.Fatalf("a stale failure write clobbered the failover history: %+v", record)
	}
	// The "workload already running locally" path is a second failover of
	// the same duty — it must not rewrite the first one's history either.
	if _, err := service.standby.markFailedOver("workload-1", "", "workload already running locally"); err != nil {
		t.Fatal(err)
	}
	record = duty(t, service, "workload-1")
	if record.FailoverReason != "operator commanded failover" {
		t.Fatalf("a later failover path overwrote the recorded reason: %+v", record)
	}
}

// TestFailoverRefusesACheckpointFromAnotherWorkload: the restore runs under
// the checkpoint manifest's workload, so a trigger naming another workload's
// checkpoint must be refused before anything is prepared — otherwise it
// restores and starts the other workload while this duty claims success.
func TestFailoverRefusesACheckpointFromAnotherWorkload(t *testing.T) {
	service := openStandbyTestService(t, "")
	own := model.WorkloadSpec{ID: "workload-1", Name: "protected", RootPath: "/srv/protected"}
	other := model.WorkloadSpec{ID: "workload-2", Name: "intruder", RootPath: "/srv/intruder"}
	ownCheckpoint := fabricateCheckpoint(t, service, own, "checkpoint-own", "")
	foreignCheckpoint := fabricateCheckpoint(t, service, other, "checkpoint-foreign", "")
	if err := service.standby.duties.Put("workload-1", armedDuty("workload-1", "machine-source", unreachablePeer, ownCheckpoint.ID)); err != nil {
		t.Fatal(err)
	}
	_, err := service.standby.Trigger(context.Background(), "workload-1", foreignCheckpoint.ID, false, "operator commanded failover")
	if !errors.Is(err, errForeignCheckpoint) {
		t.Fatalf("a foreign checkpoint must be refused, got %v", err)
	}
	record := duty(t, service, "workload-1")
	if record.State != model.StandbyArmed || record.FailoverReason != "" {
		t.Fatalf("a refused trigger must leave the duty armed, got %+v", record)
	}
}

// TestFailoverKeepsFencingEvidence: the ownership epoch, token, and fence
// result are duty history and survive the success path too — the STONITH
// record of a real automatic failover must not drop to zero values.
func TestFailoverKeepsFencingEvidence(t *testing.T) {
	service := openStandbyTestService(t, "")
	workload := model.WorkloadSpec{ID: "workload-1", Name: "protected", RootPath: "/srv/protected"}
	manifest := fabricateCheckpoint(t, service, workload, "checkpoint-1", "")
	if err := service.standby.duties.Put("workload-1", armedDuty("workload-1", "machine-source", unreachablePeer, manifest.ID)); err != nil {
		t.Fatal(err)
	}
	service.standby.recordFencingEvidence("workload-1", ownershipAcquireResponse{Epoch: 7, Token: "ownership-token"}, "fenced")
	service.standby.restore = func(context.Context, string, bool) (checkpoint.RestoreRecord, error) {
		return checkpoint.RestoreRecord{ID: "restore-1"}, nil
	}
	if _, err := service.standby.Trigger(context.Background(), "workload-1", "", false, "automatic: fenced"); err != nil {
		t.Fatal(err)
	}
	record := duty(t, service, "workload-1")
	if record.OwnershipEpoch != 7 || record.OwnershipToken != "ownership-token" || record.FenceResult != "fenced" {
		t.Fatalf("the failover dropped the fencing evidence: %+v", record)
	}
	if record.FailoverRestoreID != "restore-1" || record.State != model.StandbyFailedOver {
		t.Fatalf("the failover record: %+v", record)
	}
}

// TestFailoverDoesNotBlockReplicationCallbacks: the restore is the slow half
// of a failover and runs with no duty lock held, so a live source's hold and
// withdraw callbacks keep flowing while it works — the lock discipline the
// ReplicationWatcher contract promises.
func TestFailoverDoesNotBlockReplicationCallbacks(t *testing.T) {
	service := openStandbyTestService(t, "")
	workload := model.WorkloadSpec{ID: "workload-1", Name: "protected", RootPath: "/srv/protected"}
	manifest := fabricateCheckpoint(t, service, workload, "checkpoint-1", "")
	if err := service.standby.duties.Put("workload-1", armedDuty("workload-1", "machine-source", unreachablePeer, manifest.ID)); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	service.standby.restore = func(context.Context, string, bool) (checkpoint.RestoreRecord, error) {
		close(started)
		<-release
		return checkpoint.RestoreRecord{ID: "restore-1"}, nil
	}
	triggered := make(chan struct{})
	go func() {
		defer close(triggered)
		_, _ = service.standby.Trigger(context.Background(), "workload-1", "", false, "operator commanded failover")
	}()
	<-started
	flowed := make(chan struct{})
	go func() {
		defer close(flowed)
		service.standby.ReplicationHeld(transfer.Session{
			ID: "session-1", WorkloadID: "workload-1", CheckpointID: manifest.ID,
			SourceMachineID: "machine-source", Purpose: transfer.PurposeReplication,
			State: transfer.SessionHeld, UpdatedAt: time.Now().UTC(),
		})
	}()
	select {
	case <-flowed:
	case <-time.After(2 * time.Second):
		t.Fatal("the hold callback blocked behind a running failover")
	}
	close(release)
	<-triggered
}

// TestReplicationHoldDegradesWhenTheDutyCannotBePersisted: the duty record
// is what makes a hold real — the death watch and retention reason from it.
// When it cannot be persisted the hold must not be answered as durable: the
// session is degraded so the source's confirmation records the failure and
// retries, instead of believing a protection that does not exist.
func TestReplicationHoldDegradesWhenTheDutyCannotBePersisted(t *testing.T) {
	service := openStandbyTestService(t, "")
	workload := model.WorkloadSpec{ID: "workload-1", Name: "protected", RootPath: "/srv/protected"}
	manifest := fabricateCheckpoint(t, service, workload, "checkpoint-held", "")
	if _, err := service.sessions.Reserve(transfer.Session{
		ID: "session-1", SourceMachineID: "machine-source", WorkloadID: "workload-1",
		Purpose: transfer.PurposeReplication, EstimatedBytes: 1,
	}); err != nil {
		t.Fatal(err)
	}
	service.standby.duties = failingDutyStore{dutyStore: service.standby.duties}
	service.standby.ReplicationHeld(transfer.Session{
		ID: "session-1", WorkloadID: "workload-1", CheckpointID: manifest.ID,
		SourceMachineID: "machine-source", Purpose: transfer.PurposeReplication,
		State: transfer.SessionHeld, UpdatedAt: time.Now().UTC(),
	})
	if _, err := service.standby.duties.Get("workload-1"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("a failed duty persist must leave no duty behind, got %v", err)
	}
	session, err := service.sessions.Get("session-1", "machine-source")
	if err != nil {
		t.Fatal(err)
	}
	if session.State != transfer.SessionFailed {
		t.Fatalf("an undurable hold must be degraded, session is %s", session.State)
	}
	if session.FailureReason == "" {
		t.Fatal("the degraded hold must say why it failed")
	}
}
