package observability

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Metrics struct {
	startedAt      time.Time
	requests       atomic.Uint64
	errors         atomic.Uint64
	activeRequests atomic.Int64
	diagnostics    *Diagnostics
	// httpDuration is the request-latency histogram behind
	// shift_http_request_duration_seconds. Its own _sum and _count series are
	// the only writers of those names: a second pair would render duplicate
	// samples and break every Prometheus scrape.
	httpDuration *Histogram
	mu           sync.RWMutex
	byStatus     map[int]uint64
	gauges       map[string]gaugeSource
}

type gaugeSource struct {
	help string
	fn   func() float64
}

func NewMetrics() *Metrics {
	return &Metrics{
		startedAt:    time.Now(),
		byStatus:     make(map[int]uint64),
		gauges:       make(map[string]gaugeSource),
		httpDuration: NewHistogram("shift_http_request_duration_seconds", "control-plane HTTP request latency in seconds", LatencyBuckets),
	}
}

// RegisterGauge publishes a computed gauge on the metrics endpoint. The
// function is scraped at render time, so pool and queue gauges never need
// write-side bookkeeping. Names that would break the exposition format are
// ignored rather than smuggled into the scrape.
func (m *Metrics) RegisterGauge(name, help string, fn func() float64) {
	if !validMetricName(name) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[name] = gaugeSource{help: help, fn: fn}
}

// ObserveDuration records one request-latency sample.
func (m *Metrics) ObserveDuration(seconds float64) {
	m.httpDuration.Observe(seconds)
}

// SetDiagnostics attaches the operational series so the metrics handler
// renders request counters and migration diagnostics on one endpoint.
func (m *Metrics) SetDiagnostics(diagnostics *Diagnostics) {
	m.diagnostics = diagnostics
}

func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintf(writer, "shift_uptime_seconds %f\n", time.Since(m.startedAt).Seconds())
		_, _ = fmt.Fprintf(writer, "shift_http_requests_total %d\n", m.requests.Load())
		_, _ = fmt.Fprintf(writer, "shift_http_errors_total %d\n", m.errors.Load())
		_, _ = fmt.Fprintf(writer, "shift_http_active_requests %d\n", m.activeRequests.Load())
		m.httpDuration.RenderTo(writer)
		m.mu.RLock()
		for status, count := range m.byStatus {
			_, _ = fmt.Fprintf(writer, "shift_http_responses_total{status=%q} %d\n", strconv.Itoa(status), count)
		}
		gauges := make(map[string]gaugeSource, len(m.gauges))
		for name, source := range m.gauges {
			gauges[name] = source
		}
		m.mu.RUnlock()
		for name, source := range gauges {
			if source.help != "" {
				_, _ = fmt.Fprintf(writer, "# HELP shift_%s %s\n", name, source.help)
			}
			_, _ = fmt.Fprintf(writer, "# TYPE shift_%s gauge\n", name)
			_, _ = fmt.Fprintf(writer, "shift_%s %f\n", name, source.fn())
		}
		if m.diagnostics != nil {
			m.diagnostics.RenderTo(writer)
		}
	})
}

func (m *Metrics) Middleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		requestID := request.Header.Get("X-Request-ID")
		if requestID == "" || len(requestID) > 128 {
			requestID = newRequestID()
		}
		request.Header.Set("X-Request-ID", requestID)
		writer.Header().Set("X-Request-ID", requestID)
		capture := &statusWriter{ResponseWriter: writer, status: http.StatusOK}
		m.requests.Add(1)
		m.activeRequests.Add(1)
		defer func() {
			m.activeRequests.Add(-1)
			duration := time.Since(started)
			m.httpDuration.Observe(duration.Seconds())
			m.mu.Lock()
			m.byStatus[capture.status]++
			m.mu.Unlock()
			if capture.status >= 500 {
				m.errors.Add(1)
			}
			logger.Info("http request", "request_id", requestID, "method", request.Method, "path", request.URL.Path, "status", capture.status, "duration", duration, "remote", request.RemoteAddr)
		}()
		next.ServeHTTP(capture, request)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func NewLogger(level string) *slog.Logger {
	var parsed slog.Level
	switch strings.ToLower(level) {
	case "debug":
		parsed = slog.LevelDebug
	case "warn", "warning":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	default:
		parsed = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parsed}))
}

func newRequestID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(value[:])
}
