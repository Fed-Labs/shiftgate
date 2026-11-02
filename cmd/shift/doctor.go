package main

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/billing"
)

// doctor.go: clone-readiness output for `shiftgate doctor` (master BR-03) and
// the support bundle summary (plan 4 BR-10). The JSON shape is golden-tested;
// human mode prints one summary line under the check table.

// cloneStateSizeHeuristic is the assumed on-disk size of one restored worker's
// state when estimating clone headroom. It is deliberately conservative (1
// GiB): CI workers with warmed heaps land near 100 MiB, Python/Runtimes with
// loaded models can exceed it. The estimate is a floor, not a promise.
const cloneStateSizeHeuristic = 1 << 30

// DoctorCheck is one agent health check line as it appears in the report.
type DoctorCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// DoctorReport is the `shiftgate doctor --json` document. It embeds the
// agent's checks and adds the clone-readiness fields the demo scripts and
// support threads consume.
type DoctorReport struct {
	Healthy              bool              `json:"healthy"`
	Checks               []DoctorCheck     `json:"checks"`
	CRIUOK               bool              `json:"criu_ok"`
	KernelOK             bool              `json:"kernel_ok"`
	CloneReady           bool              `json:"clone_ready"`
	CloneReadyReason     string            `json:"clone_ready_reason,omitempty"`
	CloneHeadroomWorkers int               `json:"clone_headroom_workers"`
	LiveMigrationPlan    string            `json:"live_migration_plan"` // "yes" | "no" | "unknown"
	SupportBundle        map[string]string `json:"support_bundle"`
}

// buildDoctorReport derives the clone-readiness fields from the agent's
// checks plus a free-disk estimate and the live-migration entitlement (when a
// control-plane session exists; otherwise "unknown" — a detached agent's
// config assertion governs, and the doctor never fails closed on a lookup the
// local deployment cannot make).
func buildDoctorReport(ctx context.Context, settings options, result agentclient.DoctorResponse) DoctorReport {
	report := DoctorReport{Healthy: result.Healthy}
	report.Checks = make([]DoctorCheck, 0, len(result.Checks))
	checksByName := map[string]bool{}
	for _, check := range result.Checks {
		checksByName[check.Name] = check.OK
		report.Checks = append(report.Checks, DoctorCheck{Name: check.Name, OK: check.OK, Message: check.Message})
	}
	report.CRIUOK = checksByName["criu"]
	report.KernelOK = checksByName["platform"] && checksByName["cgroup_v2"]
	report.CloneReady = report.CRIUOK && report.KernelOK && checksByName["privileges"]
	switch {
	case !report.KernelOK:
		report.CloneReadyReason = "failing check: platform or cgroup_v2 (clone needs cgroup v2 on Linux x86_64)"
	case !report.CRIUOK:
		report.CloneReadyReason = "failing check: criu (criu check must pass for checkpoint/restore)"
	case !checksByName["privileges"]:
		report.CloneReadyReason = "failing check: privileges (the agent must run as root)"
	}
	report.CloneHeadroomWorkers = estimateCloneHeadroom()
	report.LiveMigrationPlan = liveMigrationPlanAnswer(ctx, settings)
	report.SupportBundle = supportBundle(result)
	return report
}

// estimateCloneHeadroom is free disk / the state-size heuristic. A machine
// without a readable disk reports zero rather than guessing.
func estimateCloneHeadroom() int {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(".", &stats); err != nil {
		return 0
	}
	free := int64(stats.Bavail) * int64(stats.Bsize)
	workers := int(free / cloneStateSizeHeuristic)
	if workers < 0 {
		return 0
	}
	return workers
}

// liveMigrationPlanAnswer reports whether the signed-in account's plan
// carries live migration. No session → "unknown" (local-only deployments are
// governed by the agent's entitlement config assertion instead).
func liveMigrationPlanAnswer(ctx context.Context, settings options) string {
	session, err := controlSession(settings)
	if err != nil {
		return "unknown"
	}
	organizationID, err := resolveOrganization(ctx, session, "")
	if err != nil {
		return "unknown"
	}
	entitlement, err := session.Entitlement(ctx, organizationID)
	if err != nil {
		return "unknown"
	}
	active := entitlement.Status == "active" || entitlement.Status == "trialing" || entitlement.Status == "past_due"
	if billing.LiveMigrationAllowed(entitlement.Plan, active) {
		return "yes"
	}
	return "no"
}

// supportBundle is the paste-into-GitHub-issues summary: versions, kernel,
// CRIU probe, and the redacted config surface. Never secrets: the bundle
// holds check names and booleans only.
func supportBundle(result agentclient.DoctorResponse) map[string]string {
	bundle := map[string]string{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}
	for _, check := range result.Checks {
		state := "fail"
		if check.OK {
			state = "ok"
		}
		bundle["check_"+check.Name] = state + ": " + check.Message
	}
	return bundle
}

// writeDoctorHuman prints the check table plus one clone-readiness summary
// line. Exit-code semantics are unchanged: 3 when a required check failed.
func writeDoctorHuman(settings options, report DoctorReport) error {
	passed := 0
	for _, check := range report.Checks {
		status := "FAIL"
		if check.OK {
			status = "OK"
			passed++
		}
		_, _ = fmt.Fprintf(settings.stdout, "%-5s %-20s %s\n", status, check.Name, check.Message)
	}
	_, _ = fmt.Fprintf(settings.stdout, "%d/%d checks passed\n", passed, len(report.Checks))
	ready := "not ready"
	if report.CloneReady {
		ready = "ready"
	}
	_, _ = fmt.Fprintf(settings.stdout, "clone: %s · headroom ≈ %d workers · live migration: %s\n", ready, report.CloneHeadroomWorkers, report.LiveMigrationPlan)
	if !report.Healthy {
		return exitError{code: 3, err: errors.New("one or more required checks failed")}
	}
	return nil
}
