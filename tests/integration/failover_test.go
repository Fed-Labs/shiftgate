// Warm-standby failover end to end, against real agents and real CRIU: a
// source agent replicates its policy checkpoints to a standby over the
// mutually authenticated peer channel, the source machine dies, and the
// workload is restored on the standby — once by an explicit operator
// command, and once entirely on the standby's own two-signal judgment (dead
// peer listener plus a stale control-plane presence record).
//
// The operator-command scenario runs with the standard e2e gate:
//
//	SHIFT_TEST_E2E=1 go test ./tests/integration/ -run TestE2EWarmStandby -v
//
// The automatic scenario waits out the presence staleness bound (90s after
// the source's last heartbeat), so it carries its own gate on top:
//
//	SHIFT_TEST_E2E=1 SHIFT_TEST_FAILOVER=1 go test ./tests/integration/ -run TestE2EAutomaticFailover -v
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/controlplane"
	"shift.dev/shift/internal/model"
)

// fakeFleetControl is the presence half of a control plane, and only that
// half: machine registration, heartbeats, and the organization machine list.
// It answers with the wire shapes shift-control answers — a bare JSON array
// of machine records, heartbeats rejected for machines that were never
// registered, and registrations that start offline until the first
// heartbeat — so the standby's death judgment runs against the same evidence
// production gives it. Credentials are checked, not assumed: an agent with
// the wrong key fails loudly instead of quietly passing.
type fakeFleetControl struct {
	server   *httptest.Server
	orgID    string
	apiKey   string
	mu       sync.Mutex
	machines map[string]controlplane.Machine
}

func newFakeFleetControl(t *testing.T, orgID, apiKey string) *fakeFleetControl {
	t.Helper()
	fake := &fakeFleetControl{orgID: orgID, apiKey: apiKey, machines: map[string]controlplane.Machine{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/organizations/{organizationID}/machines", fake.handleRegister)
	mux.HandleFunc("POST /v1/organizations/{organizationID}/machines/{machineID}/heartbeat", fake.handleHeartbeat)
	mux.HandleFunc("GET /v1/organizations/{organizationID}/machines", fake.handleList)
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeFleetControl) url() string { return fake.server.URL }

// authorize enforces the one credential this fake issues and the one
// organization it serves.
func (fake *fakeFleetControl) authorize(writer http.ResponseWriter, request *http.Request) bool {
	if request.Header.Get("Authorization") != "Bearer "+fake.apiKey {
		http.Error(writer, "bad credentials", http.StatusUnauthorized)
		return false
	}
	if request.PathValue("organizationID") != fake.orgID {
		http.Error(writer, "unknown organization", http.StatusNotFound)
		return false
	}
	return true
}

// handleRegister mirrors machine registration: a stable machine id, a name,
// and an https agent URL — the URL peers dial, so a plain-http value would
// silently break them. A registered machine starts offline: presence is
// earned by heartbeats, not by registration.
func (fake *fakeFleetControl) handleRegister(writer http.ResponseWriter, request *http.Request) {
	if !fake.authorize(writer, request) {
		return
	}
	var input struct {
		MachineID string `json:"machine_id"`
		Name      string `json:"name"`
		AgentURL  string `json:"agent_url"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		http.Error(writer, "bad request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(input.MachineID) == "" || strings.TrimSpace(input.Name) == "" || !strings.HasPrefix(input.AgentURL, "https://") {
		http.Error(writer, "machine id, name, and an https agent URL are required", http.StatusBadRequest)
		return
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, exists := fake.machines[input.MachineID]; exists {
		http.Error(writer, "machine is already registered", http.StatusConflict)
		return
	}
	now := time.Now().UTC()
	fake.machines[input.MachineID] = controlplane.Machine{
		ID: input.MachineID, OrganizationID: fake.orgID, MachineID: input.MachineID,
		Name: input.Name, AgentURL: strings.TrimRight(input.AgentURL, "/"),
		Status: "offline", CreatedAt: now, UpdatedAt: now,
	}
	writeControlJSON(writer, http.StatusCreated, fake.machines[input.MachineID])
}

// registerMachine registers a machine with the fake over the same HTTP
// surface a real operator uses.
func (fake *fakeFleetControl) registerMachine(t *testing.T, machineID, name, agentURL string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"machine_id": machineID, "name": name, "agent_url": agentURL})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, fake.url()+"/v1/organizations/"+fake.orgID+"/machines", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+fake.apiKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("register machine %s: %v", machineID, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("register machine %s returned %d", machineID, response.StatusCode)
	}
}

// handleHeartbeat refreshes a registered machine's presence. An unknown
// machine is a 404 — the real control plane updates rather than upserts —
// so registration must precede heartbeats, exactly as in production.
func (fake *fakeFleetControl) handleHeartbeat(writer http.ResponseWriter, request *http.Request) {
	if !fake.authorize(writer, request) {
		return
	}
	var input struct {
		Name         string         `json:"name"`
		AgentURL     string         `json:"agent_url"`
		Capabilities map[string]any `json:"capabilities"`
		Status       string         `json:"status"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		http.Error(writer, "bad request body", http.StatusBadRequest)
		return
	}
	if input.Status == "" {
		input.Status = "online"
	}
	if input.Status != "online" && input.Status != "offline" && input.Status != "draining" {
		http.Error(writer, "unsupported machine status", http.StatusBadRequest)
		return
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	record, found := fake.machines[request.PathValue("machineID")]
	if !found {
		http.Error(writer, "machine was not found", http.StatusNotFound)
		return
	}
	now := time.Now().UTC()
	if strings.TrimSpace(input.Name) != "" {
		record.Name = strings.TrimSpace(input.Name)
	}
	if strings.TrimSpace(input.AgentURL) != "" {
		record.AgentURL = strings.TrimRight(strings.TrimSpace(input.AgentURL), "/")
	}
	if input.Capabilities != nil {
		record.Capabilities = input.Capabilities
	}
	record.Status = input.Status
	record.LastSeenAt = &now
	record.UpdatedAt = now
	fake.machines[record.MachineID] = record
	writeControlJSON(writer, http.StatusOK, record)
}

// handleList answers the organization's machine records as a bare JSON
// array, ordered by name, the way the standby's presence query reads them.
func (fake *fakeFleetControl) handleList(writer http.ResponseWriter, request *http.Request) {
	if !fake.authorize(writer, request) {
		return
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	records := make([]controlplane.Machine, 0, len(fake.machines))
	for _, record := range fake.machines {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	writeControlJSON(writer, http.StatusOK, records)
}

// lastSeen reports when a machine last heartbeated, so a test can prove the
// fake is receiving heartbeats before it starts depending on their absence.
func (fake *fakeFleetControl) lastSeen(machineID string) (time.Time, bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	record, found := fake.machines[machineID]
	if !found || record.LastSeenAt == nil {
		return time.Time{}, false
	}
	return *record.LastSeenAt, true
}

func writeControlJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

// advertiseAgentURL points the agent's advertised peer URL at its own remote
// listener: the URL a standby records from a replication session and probes
// before believing the source dead. The harness chooses the peer port before
// mutate runs, so the URL is derived from the configuration itself.
func advertiseAgentURL(configuration *config.Agent) {
	configuration.ControlPlane.AgentURL = strings.Replace(configuration.RemoteListen, "tcp://", "https://", 1)
}

// standbyDuty finds one workload's duty in the standby's own view.
func standbyDuty(status agentclient.StandbyStatus, workloadID string) (model.StandbyDuty, bool) {
	for _, duty := range status.Duties {
		if duty.WorkloadID == workloadID {
			return duty, true
		}
	}
	return model.StandbyDuty{}, false
}

// protectWorkload installs the policy pair on the source: a checkpoint every
// 10 seconds (the floor the model allows) and the standby that should hold
// the copies, pinned to the standby's machine identity.
func protectWorkload(t testing.TB, source, standby *agentProcess, workloadID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := source.client.SetCheckpointPolicy(ctx, workloadID, 10, 0); err != nil {
		t.Fatalf("install checkpoint policy: %v", err)
	}
	if _, err := source.client.SetFailoverPolicy(ctx, workloadID, standby.peerURL(), standby.machineID(t), 2); err != nil {
		t.Fatalf("install failover policy: %v", err)
	}
}

// waitReplicated waits until the standby holds the workload's state: an
// armed duty with a checkpoint it could restore. The failure detail carries
// the source's replication ledger, which is where a stuck push explains
// itself.
func waitReplicated(t testing.TB, source, standby *agentProcess, workloadID string) model.StandbyDuty {
	t.Helper()
	var armed model.StandbyDuty
	waitUntil(t, 90*time.Second, "the standby to hold a replicated checkpoint", func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		status, err := standby.client.StandbyStatus(ctx)
		if err != nil {
			return false, err.Error()
		}
		duty, found := standbyDuty(status, workloadID)
		if found && duty.LastCheckpointID != "" {
			armed = duty
			return true, ""
		}
		detail := "no duty yet"
		if found {
			detail = fmt.Sprintf("duty state %s holds no checkpoint", duty.State)
		}
		if entries, ledgerErr := source.client.ReplicationEntries(ctx); ledgerErr == nil {
			for _, entry := range entries {
				if entry.WorkloadID == workloadID {
					detail = fmt.Sprintf("ledger pushed %q error %q", entry.LastCheckpointID, entry.LastError)
				}
			}
		}
		return false, detail
	})
	return armed
}

// killSourceWorkload stops the source's workload without its agent's help —
// the agent is already dead, which is the whole point — then waits for the
// counter to stop moving: a stability window twice the script's write
// period, so a live writer cannot pass for a dead one. The kill is SIGKILL
// because the workload is PID 1 of its own PID namespace (a privileged agent
// always launches it that way) and the kernel drops catchable signals to a
// namespace's init — SIGKILL is what reaches it, the same signal the agent's
// own stop path uses. The returned count is the workload's final output on
// the source machine.
func killSourceWorkload(t testing.TB, scriptPath, logPath string) int {
	t.Helper()
	if err := exec.Command("pkill", "-9", "-f", scriptPath).Run(); err != nil {
		t.Logf("pkill -9 %s: %v (proceeding; the quiet-counter check below is the real gate)", scriptPath, err)
	}
	var final int
	waitUntil(t, 30*time.Second, "the workload's counter to stop", func() (bool, string) {
		before := fileLineCount(t, logPath)
		time.Sleep(2 * time.Second)
		final = fileLineCount(t, logPath)
		return before == final, fmt.Sprintf("lines %d -> %d", before, final)
	})
	return final
}

// assertFailoverOutcome pins what a completed failover leaves behind: the
// workload running here, its standby designation dropped (this machine is
// its home now; a standby replicating to itself is a loop), and its
// checkpoint schedule intact.
func assertFailoverOutcome(t testing.TB, standby *agentProcess, workloadID string) {
	t.Helper()
	waitForWorkload(t, standby.client, workloadID, model.WorkloadRunning)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	workload, err := standby.client.Workload(ctx, workloadID)
	if err != nil {
		t.Fatalf("read the restored workload: %v", err)
	}
	if workload.Spec.FailoverPolicy != nil {
		t.Fatalf("the restored workload must not keep pointing at a standby: %+v", workload.Spec.FailoverPolicy)
	}
	if workload.Spec.CheckpointPolicy == nil {
		t.Fatal("the restored workload must keep its checkpoint schedule")
	}
}

// waitCounterResumes waits for the restored writer to append past the count
// the dead source left — the proof the restored process is the same workload
// continuing, not a fresh start.
func waitCounterResumes(t testing.TB, logPath string, quiet int) {
	t.Helper()
	waitUntil(t, 60*time.Second, "the restored counter to make progress", func() (bool, string) {
		lines := fileLineCount(t, logPath)
		return lines > quiet, fmt.Sprintf("lines=%d (was %d when the source died)", lines, quiet)
	})
}

// TestE2EWarmStandbyFailover: a protected workload's source machine dies —
// agent and workload process both — and a standby holding its replicated
// checkpoints does nothing on its own (it has no control plane to confirm
// the death), then restores the workload on the operator's explicit command.
// The restored workload resumes the same counter log, keeps its checkpoint
// schedule, and no longer points at a standby.
func TestE2EWarmStandbyFailover(t *testing.T) {
	requireE2E(t)
	standby := startAgent(t, "failover-standby")
	source := startAgentWith(t, "failover-source", t.TempDir(), t.TempDir(), func(configuration *config.Agent) {
		advertiseAgentURL(configuration)
	})
	if standby.machineID(t) == source.machineID(t) {
		t.Fatal("two agents on one machine must not share a machine identity")
	}

	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	workload := createWorkload(t, source.client, "failover-counter", root, script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})

	protectWorkload(t, source, standby, workload.Spec.ID)
	duty := waitReplicated(t, source, standby, workload.Spec.ID)
	if duty.SourceMachineID != source.machineID(t) {
		t.Fatalf("the duty records source %s, want this source's identity", duty.SourceMachineID)
	}
	if duty.SourceAgentURL != source.peerURL() {
		t.Fatalf("the duty records source URL %s, want %s", duty.SourceAgentURL, source.peerURL())
	}

	// The source machine dies: agent first, then the workload it leaves
	// behind (a workload outlives its agent).
	source.stop(t)
	quiet := killSourceWorkload(t, script, logPath)

	// A standby with no control plane never acts alone, however dead the
	// source: two full supervision ticks pass with the duty still armed and
	// no workload here.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		status, statusErr := standby.client.StandbyStatus(ctx)
		workloadHere, workloadErr := standby.client.Workload(ctx, workload.Spec.ID)
		cancel()
		if statusErr != nil {
			t.Fatalf("read standby status: %v", statusErr)
		}
		if status.AutomaticFailover {
			t.Fatal("a standby without a control plane must report automatic failover as impossible")
		}
		held, found := standbyDuty(status, workload.Spec.ID)
		if !found || held.State != model.StandbyArmed {
			t.Fatalf("the duty must stay armed until an operator commands it, got %+v (found=%v)", held, found)
		}
		if workloadErr == nil {
			t.Fatalf("the workload must not exist on the standby before the failover, got %+v", workloadHere.Spec)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// The operator's command is the confirmation the standby could not
	// reach on its own. The dead source's listener must be probed and
	// reported unreachable, not assumed dead — and a source that cannot be
	// reached carries no two-live-copies warning.
	triggerCtx, triggerCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer triggerCancel()
	result, err := standby.client.TriggerStandbyFailover(triggerCtx, workload.Spec.ID, "", false)
	if err != nil {
		t.Fatalf("operator failover: %v", err)
	}
	if result.SourceReachable != "no" {
		t.Fatalf("a dead source must be probed and reported unreachable, got %q", result.SourceReachable)
	}
	if result.Warning != "" {
		t.Fatalf("an unreachable source must not carry a two-copies warning: %q", result.Warning)
	}
	if result.Duty.State != model.StandbyFailedOver || result.Duty.FailoverRestoreID == "" {
		t.Fatalf("the duty must record the failover and its restore: %+v", result.Duty)
	}
	assertFailoverOutcome(t, standby, workload.Spec.ID)
	waitCounterResumes(t, logPath, quiet)
}

// TestE2EAutomaticFailover: the same death, judged by the standby itself.
// Both agents heartbeat to a control plane (an in-process stand-in for the
// presence half of shift-control); the source's presence record goes stale
// once it dies, and only then — dead peer listener plus a stale record, the
// two signals — does the standby restore the workload with no operator
// command at all. The wait is the presence staleness bound (90s) plus tick
// and restore time, which is why this scenario carries its own gate.
func TestE2EAutomaticFailover(t *testing.T) {
	requireE2E(t)
	if os.Getenv("SHIFT_TEST_FAILOVER") != "1" {
		t.Skip("set SHIFT_TEST_FAILOVER=1 to run the automatic failover end-to-end (it waits out the 90s presence staleness bound)")
	}
	const (
		fleetOrg    = "e2e-failover-org"
		fleetAPIKey = "failover-e2e-api-key-0000000000"
	)
	control := newFakeFleetControl(t, fleetOrg, fleetAPIKey)

	// The standby: with a control plane it can judge a source's death on its
	// own. Its configured machine id only names it in the fleet's presence
	// list — nothing looks the standby up — so one boot suffices; the source
	// below is the machine whose record must be found under its identity.
	standby := startAgentWith(t, "auto-standby", t.TempDir(), t.TempDir(), func(configuration *config.Agent) {
		advertiseAgentURL(configuration)
		configuration.ControlPlane = config.ControlReporter{
			URL: control.url(), OrganizationID: fleetOrg,
			MachineID: "auto-standby", AgentURL: configuration.ControlPlane.AgentURL,
			APIKey: fleetAPIKey, Interval: 10 * time.Second,
		}
	})
	control.registerMachine(t, "auto-standby", "auto-standby", standby.peerURL())

	// The source: its presence record must be keyed by the machine identity
	// its peer sessions authenticate as, so the configured machine id and
	// the identity must be the same string — the way a fleet registers a
	// machine with the id its agent reports. The identity is minted when a
	// state directory is first opened, so the source boots once to learn it
	// and again with the control-plane block carrying it.
	sourceState, sourceSocket := t.TempDir(), t.TempDir()
	bootstrap := startAgentAt(t, "auto-source", sourceState, sourceSocket)
	sourceIdentity := bootstrap.machineID(t)
	bootstrap.stop(t)
	source := startAgentWith(t, "auto-source", sourceState, sourceSocket, func(configuration *config.Agent) {
		advertiseAgentURL(configuration)
		configuration.ControlPlane = config.ControlReporter{
			URL: control.url(), OrganizationID: fleetOrg,
			MachineID: sourceIdentity, AgentURL: configuration.ControlPlane.AgentURL,
			APIKey: fleetAPIKey, Interval: 10 * time.Second,
		}
	})
	if source.machineID(t) != sourceIdentity {
		t.Fatal("the source's machine identity must survive its restart")
	}
	control.registerMachine(t, sourceIdentity, "auto-source", source.peerURL())

	// The source's presence must be live before its death is worth
	// anything: a heartbeat that never landed would leave the record
	// offline from the start, and a failover would fire for the wrong
	// reason.
	waitUntil(t, 30*time.Second, "the source's first heartbeat to land", func() (bool, string) {
		lastSeen, ok := control.lastSeen(sourceIdentity)
		return ok, fmt.Sprintf("last seen %s ago", time.Since(lastSeen).Round(time.Second))
	})

	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	workload := createWorkload(t, source.client, "failover-counter", root, script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})

	protectWorkload(t, source, standby, workload.Spec.ID)
	waitReplicated(t, source, standby, workload.Spec.ID)
	statusCtx, statusCancel := context.WithTimeout(context.Background(), 10*time.Second)
	status, err := standby.client.StandbyStatus(statusCtx)
	statusCancel()
	if err != nil {
		t.Fatalf("read standby status: %v", err)
	}
	if !status.AutomaticFailover {
		t.Fatal("a standby with a control plane must be able to fail over automatically")
	}

	// The source dies, and nobody tells the standby.
	source.stop(t)
	quiet := killSourceWorkload(t, script, logPath)

	// Half a minute later the source's record is still well within the
	// staleness bound — a workload restored now would prove the standby
	// acted on one signal. This pins the wait to the two-signal rule rather
	// than to "eventually".
	time.Sleep(30 * time.Second)
	earlyCtx, earlyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	earlyStatus, err := standby.client.StandbyStatus(earlyCtx)
	earlyCancel()
	if err != nil {
		t.Fatalf("read standby status: %v", err)
	}
	if duty, found := standbyDuty(earlyStatus, workload.Spec.ID); !found || duty.State != model.StandbyArmed {
		t.Fatalf("the standby must not fail over before the presence record goes stale, got %+v (found=%v)", duty, found)
	}

	// From here the standby decides on its own: the record goes stale 90
	// seconds after the last heartbeat, the next tick probes the dead
	// listener, and the restore follows without any operator command. A
	// running process is only half the fact — the supervisor records the
	// failover on the duty a moment after the restore commits (after the
	// restored spec's failover policy is dropped), so the wait is for both,
	// not for the process alone.
	waitUntil(t, 4*time.Minute, "the standby to fail the workload over on its own", func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		workloadHere, workloadErr := standby.client.Workload(ctx, workload.Spec.ID)
		standbyStatus, statusErr := standby.client.StandbyStatus(ctx)
		cancel()
		if workloadErr == nil && workloadHere.Status == model.WorkloadRunning && statusErr == nil {
			if duty, found := standbyDuty(standbyStatus, workload.Spec.ID); found &&
				duty.State == model.StandbyFailedOver && duty.FailoverRestoreID != "" {
				return true, ""
			}
		}
		detail := fmt.Sprintf("workload not here (%v)", workloadErr)
		if statusErr == nil {
			if duty, found := standbyDuty(standbyStatus, workload.Spec.ID); found {
				detail = fmt.Sprintf("duty state %s, last failover error %q, next attempt %s",
					duty.State, duty.LastFailoverError, duty.NextAttemptAt.Round(0))
			}
		}
		return false, detail
	})

	// The failover must be recorded with both signals in its reason: the
	// unreachable listener and what the control plane actually observed.
	dutyCtx, dutyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	finalStatus, err := standby.client.StandbyStatus(dutyCtx)
	dutyCancel()
	if err != nil {
		t.Fatalf("read standby status: %v", err)
	}
	duty, found := standbyDuty(finalStatus, workload.Spec.ID)
	if !found || duty.State != model.StandbyFailedOver || duty.FailoverRestoreID == "" {
		t.Fatalf("the duty must record the automatic failover and its restore: %+v (found=%v)", duty, found)
	}
	if !strings.Contains(duty.FailoverReason, "unreachable") || !strings.Contains(duty.FailoverReason, "last seen") {
		t.Fatalf("the failover reason must name both signals, got %q", duty.FailoverReason)
	}
	assertFailoverOutcome(t, standby, workload.Spec.ID)
	waitCounterResumes(t, logPath, quiet)
}
