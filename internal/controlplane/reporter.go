package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
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
	logger               *slog.Logger
	client               *http.Client

	startOnce sync.Once
	stopOnce  sync.Once
	done      chan struct{}

	// peerProtocol is the protocol version the control plane reported on its most
	// recent response, or zero before the first successful heartbeat. It is read
	// by the update manager, which runs on another goroutine, so it is atomic.
	peerProtocol atomic.Int64
}

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
		if capabilities, err := reporter.capabilitiesProvider(ctx); err == nil {
			payload["capabilities"] = capabilities
		} else {
			reporter.logger.Warn("control-plane inventory lookup failed", "error", err)
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		reporter.logger.Warn("control-plane heartbeat encode failed", "error", err)
		return
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		reporter.logger.Warn("control-plane heartbeat request failed", "error", err)
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+reporter.apiKey)
	response, err := reporter.client.Do(request)
	if err != nil {
		reporter.logger.Warn("control-plane heartbeat failed", "error", err)
		return
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	reporter.observeProtocol(response.Header.Get(model.ProtocolVersionHeader))
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		reporter.logger.Debug("control-plane heartbeat accepted")
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		reporter.logger.Warn("control-plane rejected heartbeat credentials", "status", response.StatusCode)
	default:
		reporter.logger.Warn("control-plane heartbeat rejected", "status", response.StatusCode)
	}
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
