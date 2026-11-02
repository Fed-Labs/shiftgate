package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shift.dev/shift/internal/model"
)

// Reporter sends non-authoritative machine presence to shift-control. The
// local agent remains the authority for workloads, checkpoints, and transfers.
type AgentCommand struct {
	ID         string          `json:"id"`
	Action     string          `json:"action"`
	Payload    json.RawMessage `json:"payload"`
	ClaimToken string          `json:"claim_token,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

type Reporter struct {
	url                  string
	organizationID       string
	machineID            string
	machineName          string
	agentURL             string
	apiKey               string
	interval             time.Duration
	timeout              time.Duration
	capabilitiesProvider func(context.Context) (any, error)
	commandHandler       func(context.Context, AgentCommand) error
	entitlementListener  func(bool)
	logger               *slog.Logger
	client               *http.Client

	startOnce sync.Once
	stopOnce  sync.Once
	done      chan struct{}

	// peerProtocol is the protocol version the control plane reported on its most
	// recent response, or zero before the first successful heartbeat. It is read
	// by the update manager, which runs on another goroutine, so it is atomic.
	peerProtocol atomic.Int64

	// transferMu guards the bandwidth deltas the agent accumulates between
	// heartbeats. The reporter is the agent's TransferMeter: the relay bridge
	// and the transfer server increment here, and every heartbeat carries the
	// drained deltas so the control plane can record them in usage_records.
	transferMu    sync.Mutex
	transferBytes map[string]int64
	// transferDisabled latches on after a control plane rejects the
	// transfer_bytes heartbeat field (an older control plane that does not
	// know it), so a version skew degrades to "no metering" instead of
	// breaking heartbeats entirely.
	transferDisabled atomic.Bool
	// pollInFlight keeps command polling single-flight: polls run on their own
	// budget so a slow command handler never stalls the heartbeat loop.
	pollInFlight atomic.Bool
}

const (
	// relayTransferChannel is the bandwidth channel the relay server meters
	// authoritatively into usage_records at session close. The agent-side
	// relay bridge must not report the same bytes again through the
	// heartbeat: relayed traffic is metered exactly once, at the relay.
	relayTransferChannel = "relay"
	// maxPendingTransferBytes caps the per-channel accumulator while sends
	// are refused, so a long version-skew outage degrades to bounded local
	// state instead of unbounded growth.
	maxPendingTransferBytes = int64(1) << 40
	// commandSettleBudget bounds one claim → execute → settle cycle. It is
	// deliberately independent of the heartbeat timeout: a command handler
	// slower than the heartbeat budget must still get to settle its claim.
	commandSettleBudget = 2 * time.Minute
	// commandSettleAttempts bounds how often the completion POST is retried
	// before the control plane's claim lease requeues the command instead.
	commandSettleAttempts = 4
)

func NewReporter(url, organizationID, machineID, machineName, agentURL, apiKey string, interval, timeout time.Duration, logger *slog.Logger) *Reporter {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Reporter{
		url:            strings.TrimRight(strings.TrimSpace(url), "/"),
		organizationID: strings.TrimSpace(organizationID),
		machineID:      strings.TrimSpace(machineID),
		machineName:    strings.TrimSpace(machineName),
		agentURL:       strings.TrimRight(strings.TrimSpace(agentURL), "/"),
		apiKey:         strings.TrimSpace(apiKey),
		interval:       interval,
		timeout:        timeout,
		logger:         logger,
		client:         &http.Client{Timeout: timeout},
		done:           make(chan struct{}),
	}
}

// SetCapabilitiesProvider attaches a best-effort inventory provider. Failures
// never prevent the heartbeat; the endpoint can update presence without inventory.
func (reporter *Reporter) SetCapabilitiesProvider(provider func(context.Context) (any, error)) {
	reporter.capabilitiesProvider = provider
}

func (reporter *Reporter) SetCommandHandler(handler func(context.Context, AgentCommand) error) {
	reporter.commandHandler = handler
}

// SetEntitlementListener attaches the agent's entitlement cache. Every
// heartbeat response carries the live-migration allowance the control plane
// computed for the organization; the listener is how the agent's admission
// gate learns that value instead of trusting a forgeable local assertion. It
// runs on the heartbeat goroutine and must be fast.
func (reporter *Reporter) SetEntitlementListener(listener func(bool)) {
	reporter.entitlementListener = listener
}

// RecordTransferBytes is the agent-side bandwidth meter seam. It has the same
// shape as database.Store.RecordTransferBytes, so the relay bridge, the relay
// server, and the transfer server can all record against whichever meter they
// are handed. The organization argument is accepted for interface parity and
// ignored: an agent belongs to exactly one organization, and the control
// plane attributes the heartbeat to it from the request path. Deltas ride the
// next heartbeat; a failed send merges them back so no byte is dropped.
// Relayed bytes are refused here on purpose: the relay server meters the
// session (both directions) into usage_records at close, and counting the
// bridge's leg again through the heartbeat would double-bill the transfer.
func (reporter *Reporter) RecordTransferBytes(_ context.Context, _ string, channel string, bytes int64) error {
	if bytes <= 0 || strings.TrimSpace(channel) == "" {
		return nil
	}
	if channel == relayTransferChannel {
		return nil
	}
	reporter.transferMu.Lock()
	defer reporter.transferMu.Unlock()
	if reporter.transferBytes == nil {
		reporter.transferBytes = map[string]int64{}
	}
	if pending := reporter.transferBytes[channel]; pending >= maxPendingTransferBytes {
		return nil
	}
	reporter.transferBytes[channel] += bytes
	if reporter.transferBytes[channel] > maxPendingTransferBytes {
		reporter.transferBytes[channel] = maxPendingTransferBytes
	}
	return nil
}

// drainTransferBytes removes and returns the accumulated bandwidth deltas.
func (reporter *Reporter) drainTransferBytes() map[string]int64 {
	reporter.transferMu.Lock()
	defer reporter.transferMu.Unlock()
	if len(reporter.transferBytes) == 0 {
		return nil
	}
	drained := reporter.transferBytes
	reporter.transferBytes = nil
	return drained
}

// restoreTransferBytes merges undelivered deltas back into the accumulator,
// clamped so an outage cannot grow local state without bound.
func (reporter *Reporter) restoreTransferBytes(drained map[string]int64) {
	if len(drained) == 0 {
		return
	}
	reporter.transferMu.Lock()
	defer reporter.transferMu.Unlock()
	if reporter.transferBytes == nil {
		reporter.transferBytes = map[string]int64{}
	}
	for channel, bytes := range drained {
		if channel == relayTransferChannel {
			continue
		}
		if reporter.transferBytes[channel] >= maxPendingTransferBytes {
			continue
		}
		reporter.transferBytes[channel] += bytes
		if reporter.transferBytes[channel] > maxPendingTransferBytes {
			reporter.transferBytes[channel] = maxPendingTransferBytes
		}
	}
}

func (reporter *Reporter) Start(ctx context.Context) {
	reporter.startOnce.Do(func() { go reporter.loop(ctx) })
}

func (reporter *Reporter) Stop() {
	reporter.stopOnce.Do(func() { close(reporter.done) })
}

// PeerProtocolVersion is the protocol version the control plane reported on its
// most recent response. It is zero until the first heartbeat is answered, which
// an update manager reads as "no control-plane requirement observed yet" rather
// than as a requirement of zero.
func (reporter *Reporter) PeerProtocolVersion() int {
	return int(reporter.peerProtocol.Load())
}

func (reporter *Reporter) loop(ctx context.Context) {
	ticker := time.NewTicker(reporter.interval)
	defer ticker.Stop()
	reporter.report(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-reporter.done:
			return
		case <-ticker.C:
			reporter.report(ctx)
		}
	}
}

func (reporter *Reporter) report(ctx context.Context) {
	requestContext, cancel := context.WithTimeout(ctx, reporter.timeout)
	defer cancel()
	endpoint := fmt.Sprintf("%s/v1/organizations/%s/machines/%s/heartbeat", reporter.url, reporter.organizationID, reporter.machineID)
	payload := map[string]any{
		"name":      reporter.machineName,
		"agent_url": reporter.agentURL,
		"status":    "online",
	}
	if reporter.capabilitiesProvider != nil {
		// The inventory walk runs inside the heartbeat's own timeout budget: a
		// hung capability probe must not push the report past its deadline.
		if capabilities, err := reporter.capabilitiesProvider(requestContext); err == nil {
			payload["capabilities"] = capabilities
		} else {
			reporter.logger.Warn("control-plane inventory lookup failed", "error", err)
		}
	}
	var transferBytes map[string]int64
	if !reporter.transferDisabled.Load() {
		transferBytes = reporter.drainTransferBytes()
		if len(transferBytes) > 0 {
			payload["transfer_bytes"] = transferBytes
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		reporter.restoreTransferBytes(transferBytes)
		reporter.logger.Warn("control-plane heartbeat encode failed", "error", err)
		return
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		reporter.restoreTransferBytes(transferBytes)
		reporter.logger.Warn("control-plane heartbeat request failed", "error", err)
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+reporter.apiKey)
	response, err := reporter.client.Do(request)
	if err != nil {
		reporter.restoreTransferBytes(transferBytes)
		reporter.logger.Warn("control-plane heartbeat failed", "error", err)
		return
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	reporter.observeProtocol(response.Header.Get(model.ProtocolVersionHeader))
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		reporter.learnEntitlement(responseBody)
		reporter.logger.Debug("control-plane heartbeat accepted")
		if reporter.commandHandler != nil {
			// Polling runs on its own budget: a slow command handler must not
			// delay the next heartbeat, and its settlement must not be cut
			// short by the heartbeat's shared deadline.
			if reporter.pollInFlight.CompareAndSwap(false, true) {
				go func() {
					defer reporter.pollInFlight.Store(false)
					reporter.pollCommand(ctx)
				}()
			}
		}
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		// Rejected on credentials before any handler ran: the deltas were not
		// recorded, so keep them for the next accepted heartbeat.
		reporter.restoreTransferBytes(transferBytes)
		reporter.logger.Warn("control-plane rejected heartbeat credentials", "status", response.StatusCode)
	case response.StatusCode == http.StatusBadRequest && len(transferBytes) > 0 && bodyMentionsTransferField(responseBody):
		// An older control plane rejects the unknown transfer_bytes field by
		// name: stop sending it and keep the deltas local (bounded).
		reporter.transferDisabled.Store(true)
		reporter.restoreTransferBytes(transferBytes)
		reporter.logger.Warn("control plane rejected the transfer_bytes field; bandwidth metering is paused for this agent")
	case response.StatusCode == http.StatusBadRequest:
		// A 400 is a rejected request body: the heartbeat handler never ran,
		// so the deltas were not recorded and are kept for the next attempt.
		reporter.restoreTransferBytes(transferBytes)
		reporter.logger.Warn("control-plane heartbeat rejected", "status", response.StatusCode)
	default:
		// Ambiguous rejection (404 after the handler metered, 5xx mid-handler):
		// the deltas may already be in usage_records. Restoring them would
		// double-count on the next heartbeat, so they are dropped — metering
		// undercounts on a rare pre-handler failure rather than overbilling.
		if len(transferBytes) > 0 {
			reporter.logger.Warn("dropping transfer deltas after a rejected heartbeat to avoid double metering", "status", response.StatusCode)
		}
		reporter.logger.Warn("control-plane heartbeat rejected", "status", response.StatusCode)
	}
}

// bodyMentionsTransferField reports whether a rejection body names the
// transfer_bytes field, which is how an older control plane refuses it. A 400
// about any other field must not latch metering off forever.
func bodyMentionsTransferField(body []byte) bool {
	return strings.Contains(string(body), "transfer_bytes")
}

// pollCommand claims one queued command, runs its handler, and settles the
// claim. Execution and settlement run on a deadline of their own (not the
// heartbeat's), and the completion POST is retried until acknowledged: a
// command that ran but whose settlement is lost would otherwise sit claimed
// until the control plane's claim lease requeues it.
func (reporter *Reporter) pollCommand(ctx context.Context) {
	commandContext, cancel := context.WithTimeout(ctx, commandSettleBudget)
	defer cancel()
	endpoint := fmt.Sprintf("%s/v1/organizations/%s/machines/%s/commands/poll", reporter.url, reporter.organizationID, reporter.machineID)
	req, err := http.NewRequestWithContext(commandContext, http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+reporter.apiKey)
	response, err := reporter.client.Do(req)
	if err != nil {
		reporter.logger.Warn("control-plane command poll failed", "error", err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return
	}
	if response.StatusCode != http.StatusOK {
		reporter.logger.Warn("control-plane command poll rejected", "status", response.StatusCode)
		return
	}
	var command AgentCommand
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&command); err != nil {
		// A command that cannot be decoded still has to settle, or its claim
		// is stranded. Settle it failed when the identity decoded; otherwise
		// the control plane's claim lease will requeue it.
		if command.ID != "" && command.ClaimToken != "" {
			reporter.settleCommand(commandContext, command, errors.New("command payload could not be decoded"))
		}
		return
	}
	err = reporter.commandHandler(commandContext, command)
	reporter.settleCommand(commandContext, command, err)
}

// settleCommand posts the completion until the control plane acknowledges it.
// A 409 means the claim already settled (a lease redelivery won the race) and
// is treated as acknowledged.
func (reporter *Reporter) settleCommand(ctx context.Context, command AgentCommand, handlerErr error) {
	completeEndpoint := fmt.Sprintf("%s/v1/organizations/%s/machines/%s/commands/%s/complete", reporter.url, reporter.organizationID, reporter.machineID, command.ID)
	body, _ := json.Marshal(map[string]any{"failed": handlerErr != nil, "error": errorString(handlerErr), "claim_token": command.ClaimToken})
	for attempt := 0; attempt < commandSettleAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				reporter.logger.Warn("control-plane command settlement abandoned", "command_id", command.ID, "error", ctx.Err())
				return
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		complete, createErr := http.NewRequestWithContext(ctx, http.MethodPost, completeEndpoint, bytes.NewReader(body))
		if createErr != nil {
			continue
		}
		complete.Header.Set("Authorization", "Bearer "+reporter.apiKey)
		complete.Header.Set("Content-Type", "application/json")
		response, callErr := reporter.client.Do(complete)
		if callErr != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		_ = response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return
		}
		if response.StatusCode == http.StatusConflict {
			// Already settled: a requeued redelivery or a replayed completion
			// closed the claim. Nothing left to acknowledge.
			return
		}
	}
	reporter.logger.Warn("control-plane command settlement failed after retries; the claim lease will requeue it", "command_id", command.ID)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// learnEntitlement consumes the heartbeat response's live_migration_allowed
// field — the control plane's licensing truth for this organization — and
// forwards it to the agent's admission gate. A response that lacks the field
// (an older control plane) teaches nothing and leaves the gate on its
// fail-closed default; the field is never inferred from anything else.
func (reporter *Reporter) learnEntitlement(body []byte) {
	if reporter.entitlementListener == nil {
		return
	}
	var response struct {
		LiveMigrationAllowed *bool `json:"live_migration_allowed"`
	}
	if json.Unmarshal(body, &response) != nil || response.LiveMigrationAllowed == nil {
		return
	}
	reporter.entitlementListener(*response.LiveMigrationAllowed)
}

// observeProtocol records the protocol version the control plane reported. A
// control plane speaking a protocol this build does not support is logged rather
// than hidden: it is the same condition that will stop an update from being
// applied, and an operator needs to see it from either side.
func (reporter *Reporter) observeProtocol(header string) {
	value := strings.TrimSpace(header)
	if value == "" {
		return
	}
	version, err := strconv.Atoi(value)
	if err != nil || version <= 0 {
		reporter.logger.Warn("control plane reported an unreadable protocol version", "value", value)
		return
	}
	previous := reporter.peerProtocol.Swap(int64(version))
	if previous == int64(version) {
		return
	}
	if version < model.MinimumProtocolVersion || version > model.ProtocolVersion {
		reporter.logger.Warn("control plane speaks a protocol version this build does not support",
			"control_plane_protocol", version,
			"supported_minimum", model.MinimumProtocolVersion,
			"supported_maximum", model.ProtocolVersion)
		return
	}
	if previous != 0 {
		reporter.logger.Info("control-plane protocol version changed", "from", previous, "to", version)
	}
}
