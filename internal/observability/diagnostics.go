package observability

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Outcome labels for the migration, checkpoint, and restore series.
const (
	OutcomeSuccess   = "success"
	OutcomeFailure   = "failure"
	OutcomeCancelled = "cancelled" //nolint:misspell // metric label; matches the CANCELLED stage vocabulary
)

// Diagnostics records SHIFT's operational series: migration outcomes,
// durations, and downtime; checkpoint and restore outcomes; transfer volume;
// and the resource gauges the agent samples. Every method is safe for
// concurrent use. Values are process-local and reset on restart — durable
// history lives in the migration records and the control plane.
type Diagnostics struct {
	mu sync.Mutex

	migrations   map[string]uint64
	duration     time.Duration
	downtime     time.Duration
	lastDuration time.Duration
	lastDowntime time.Duration

	checkpoints map[string]uint64
	restores    map[string]uint64

	transferBytes map[string]int64
	transferNanos time.Duration

	throughput *Histogram

	gauges map[string]float64
}

// NewDiagnostics returns an empty recorder.
func NewDiagnostics() *Diagnostics {
	return &Diagnostics{
		migrations:    make(map[string]uint64),
		checkpoints:   make(map[string]uint64),
		restores:      make(map[string]uint64),
		transferBytes: make(map[string]int64),
		throughput:    NewHistogram("shift_transfer_throughput_bytes_per_second", "observed chunk-transfer throughput in bytes per second", ThroughputBuckets),
		gauges:        make(map[string]float64),
	}
}

// MigrationFinished records one migration outcome with its wall-clock duration
// and the measured downtime — the window the workload was stopped, which is
// zero for migrations that failed before the freeze.
func (d *Diagnostics) MigrationFinished(outcome string, duration, downtime time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.migrations[normalizeOutcome(outcome)]++
	d.duration += duration
	d.downtime += downtime
	d.lastDuration = duration
	d.lastDowntime = downtime
}

// CheckpointFinished records one checkpoint outcome.
func (d *Diagnostics) CheckpointFinished(outcome string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.checkpoints[normalizeOutcome(outcome)]++
}

// RestoreFinished records one restore outcome.
func (d *Diagnostics) RestoreFinished(outcome string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.restores[normalizeOutcome(outcome)]++
}

// Transferred accumulates transferred bytes and elapsed transfer time per
// direction ("upload" for a source pushing chunks, "download" for a
// destination receiving them).
func (d *Diagnostics) Transferred(direction string, bytes int64, elapsed time.Duration) {
	if bytes <= 0 {
		return
	}
	direction = normalizeDirection(direction)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.transferBytes[direction] += bytes
	d.transferNanos += elapsed
	if elapsed > 0 {
		d.throughput.Observe(float64(bytes) / elapsed.Seconds())
	}
}

// SetGauge records a point-in-time value such as workload count or available
// memory. Names are lowercase with underscores, without the shift_ prefix the
// renderer adds.
func (d *Diagnostics) SetGauge(name string, value float64) {
	if !validMetricName(name) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gauges[name] = value
}

// OutcomeOther is the bucket every unrecognized outcome lands in. Label
// values come from callers' strings; without a vocabulary an arbitrary string
// would mint a new series per distinct value and grow the map without bound.
const OutcomeOther = "other"

// normalizeOutcome clamps a caller-supplied outcome to the series vocabulary.
func normalizeOutcome(outcome string) string {
	switch outcome {
	case OutcomeSuccess, OutcomeFailure, OutcomeCancelled:
		return outcome
	default:
		return OutcomeOther
	}
}

// normalizeDirection clamps a transfer direction to the series vocabulary.
func normalizeDirection(direction string) string {
	switch direction {
	case "upload", "download":
		return direction
	default:
		return OutcomeOther
	}
}

var metricNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// validMetricName rejects names that would break the exposition format or
// smuggle label sets: gauges are keyed by name, and a name containing braces
// or newlines would inject series the caller did not earn.
func validMetricName(name string) bool {
	return metricNamePattern.MatchString(name)
}

// RenderTo writes every series in Prometheus text format. Migration success
// rate is the success fraction of all finished migrations on this process; an
// empty history renders no ratio rather than a misleading 0.
func (d *Diagnostics) RenderTo(writer io.Writer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	writeSeries := func(name string, labels string, value any) {
		if labels == "" {
			_, _ = fmt.Fprintf(writer, "shift_%s %v\n", name, value)
			return
		}
		_, _ = fmt.Fprintf(writer, "shift_%s{%s} %v\n", name, labels, value)
	}
	for _, outcome := range sortedKeys(d.migrations) {
		writeSeries("migration_outcomes_total", fmt.Sprintf("outcome=%q", outcome), d.migrations[outcome])
	}
	total := 0
	for _, count := range d.migrations {
		total += int(count)
	}
	if total > 0 {
		writeSeries("migration_success_ratio", "", float64(d.migrations[OutcomeSuccess])/float64(total))
		writeSeries("migration_duration_seconds_last", "", d.lastDuration.Seconds())
		writeSeries("migration_duration_seconds_sum", "", d.duration.Seconds())
		writeSeries("migration_downtime_seconds_last", "", d.lastDowntime.Seconds())
		writeSeries("migration_downtime_seconds_sum", "", d.downtime.Seconds())
	}
	for _, outcome := range sortedKeys(d.checkpoints) {
		writeSeries("checkpoint_outcomes_total", fmt.Sprintf("outcome=%q", outcome), d.checkpoints[outcome])
	}
	for _, outcome := range sortedKeys(d.restores) {
		writeSeries("restore_outcomes_total", fmt.Sprintf("outcome=%q", outcome), d.restores[outcome])
	}
	for _, direction := range sortedKeys(d.transferBytes) {
		writeSeries("transfer_bytes_total", fmt.Sprintf("direction=%q", direction), d.transferBytes[direction])
	}
	if d.transferNanos > 0 {
		writeSeries("transfer_seconds_sum", "", d.transferNanos.Seconds())
	}
	for _, name := range sortedKeys(d.gauges) {
		writeSeries(name, "", d.gauges[name])
	}
	d.throughput.RenderTo(writer)
}

// sortedKeys returns a map's keys in sorted order so rendered output is
// deterministic. Generic over the value type because the renderers draw from
// counters, byte totals, and gauges alike.
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
