package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/model"
)

// waitready.go: `shiftgate clone --wait-ready` (master BR-07). The flag turns
// the clone into a barrier: it blocks until every spawned workload passes its
// health check (or, when the workload has none, until its process is
// running), then prints per-worker ready latency. Failure semantics match the
// promise — a failed wait never touches the source workload and never rolls
// the clone back silently: the set stands, the failure lists the workers.

// workerWaitStatus is one member's readiness outcome for the latency table.
type workerWaitStatus struct {
	Name      string        `json:"name"`
	Workload  string        `json:"workload_id"`
	Ready     bool          `json:"ready"`
	Latency   time.Duration `json:"-"`
	LatencyMS int64         `json:"latency_ms"`
	Detail    string        `json:"detail,omitempty"`
}

// waitCloneReady blocks until every clone member is ready or the timeout
// fires. Ready means: the workload's health check passes, or — when the
// workload declares none — the process is running. Originals are never
// touched; the caller decides what a non-zero exit means.
func waitCloneReady(ctx context.Context, client *agentclient.Client, record checkpoint.CloneRecord, waitTimeout time.Duration) ([]workerWaitStatus, error) {
	deadline := time.Now().Add(waitTimeout)
	statuses := make([]workerWaitStatus, 0, len(record.Members))
	for _, member := range record.Members {
		statuses = append(statuses, workerWaitStatus{Name: member.Name, Workload: member.WorkloadID})
	}
	start := time.Now()
	for {
		allReady := true
		for index := range statuses {
			if statuses[index].Ready {
				continue
			}
			ready, detail := workerReady(ctx, client, statuses[index].Workload)
			if ready {
				statuses[index].Ready = true
				statuses[index].Latency = time.Since(start)
				statuses[index].LatencyMS = statuses[index].Latency.Milliseconds()
				statuses[index].Detail = detail
			} else {
				statuses[index].Detail = detail
				allReady = false
			}
		}
		if allReady {
			return statuses, nil
		}
		if time.Now().After(deadline) {
			return statuses, errWaitTimeout
		}
		select {
		case <-ctx.Done():
			return statuses, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

var errWaitTimeout = errors.New("wait-ready timed out before every worker was ready")

// workerReady reports one workload's readiness. A workload with a health
// check must pass it (tcp dial / http 2xx / command exit 0 — the same
// semantics the restore health gate uses); a workload without one is ready
// when its process is running.
func workerReady(ctx context.Context, client *agentclient.Client, workloadID string) (bool, string) {
	if workloadID == "" {
		return false, "no workload id"
	}
	workload, err := client.Workload(ctx, workloadID)
	if err != nil {
		return false, "workload lookup failed: " + err.Error()
	}
	check := workload.Spec.HealthCheck
	if check == nil {
		if workload.Process != nil && workload.Status == model.WorkloadRunning {
			return true, "process running (no health check)"
		}
		return false, "waiting for process (status " + string(workload.Status) + ")"
	}
	probeCtx, cancel := context.WithTimeout(ctx, check.Timeout)
	defer cancel()
	if err := runReadinessProbe(probeCtx, workload.Spec, *check); err != nil {
		return false, "health check: " + err.Error()
	}
	return true, "health check passed"
}

// runReadinessProbe mirrors the restore health gate's probe semantics: tcp
// dials, http GETs expecting 2xx/3xx, or an exit-0 command. The CLI probes
// from where it runs — the same machine as the agent in the supported
// topology (see docs/support-matrix.json).
func runReadinessProbe(ctx context.Context, workload model.WorkloadSpec, check model.HealthCheckSpec) error {
	switch check.Type {
	case "tcp":
		dialer := net.Dialer{}
		connection, err := dialer.DialContext(ctx, "tcp", check.Address)
		if err != nil {
			return err
		}
		return connection.Close()
	case "http":
		parsed, err := url.Parse(check.Address)
		if err != nil {
			return err
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return errors.New("health URL must use http or https")
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), http.NoBody)
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode < 200 || response.StatusCode >= 400 {
			return fmt.Errorf("health endpoint returned %s", response.Status)
		}
		return nil
	case "command":
		if len(check.Command) == 0 {
			return errors.New("health command is empty")
		}
		command := exec.CommandContext(ctx, check.Command[0], check.Command[1:]...)
		command.Dir = workload.WorkingDir
		return command.Run()
	default:
		return fmt.Errorf("unknown health check type %q", check.Type)
	}
}
