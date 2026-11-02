package observability

import (
	"fmt"
	"io"
	"sync"
)

// Histogram is a fixed-bucket cumulative histogram in Prometheus text form:
// one series per bucket boundary, plus _sum and _count. Buckets are upper
// bounds in the observed unit; the final +Inf bucket is implicit.
type Histogram struct {
	name    string
	help    string
	buckets []float64
	mu      sync.Mutex
	counts  []uint64
	sum     float64
	count   uint64
}

// LatencyBuckets covers HTTP request latencies from sub-millisecond handler
// work to multi-second database stalls, on a roughly log scale.
var LatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// ThroughputBuckets covers per-operation transfer rates from slow WAN links
// (1 MiB/s) to local NVMe saturation (1 GiB/s), bytes per second.
var ThroughputBuckets = []float64{
	1 << 20, 4 << 20, 16 << 20, 64 << 20, 128 << 20, 256 << 20, 512 << 20, 1 << 30, 4 << 30,
}

// NewHistogram builds a histogram with the given metric name (without
// formatting suffixes), help text, and ascending bucket boundaries.
func NewHistogram(name, help string, buckets []float64) *Histogram {
	copied := append([]float64(nil), buckets...)
	return &Histogram{name: name, help: help, buckets: copied, counts: make([]uint64, len(copied))}
}

// Observe records one sample.
func (h *Histogram) Observe(value float64) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sum += value
	h.count++
	for index, boundary := range h.buckets {
		if value <= boundary {
			h.counts[index]++
		}
	}
}

// RenderTo writes the histogram in Prometheus text exposition format.
func (h *Histogram) RenderTo(writer io.Writer) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.help != "" {
		_, _ = fmt.Fprintf(writer, "# HELP %s %s\n", h.name, h.help)
	}
	_, _ = fmt.Fprintf(writer, "# TYPE %s histogram\n", h.name)
	for index, boundary := range h.buckets {
		_, _ = fmt.Fprintf(writer, "%s_bucket{le=\"%g\"} %d\n", h.name, boundary, h.counts[index])
	}
	_, _ = fmt.Fprintf(writer, "%s_bucket{le=\"+Inf\"} %d\n", h.name, h.count)
	_, _ = fmt.Fprintf(writer, "%s_sum %g\n", h.name, h.sum)
	_, _ = fmt.Fprintf(writer, "%s_count %d\n", h.name, h.count)
}
