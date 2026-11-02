package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/device"
	"shift.dev/shift/internal/observability"
)

func TestParseMeminfo(t *testing.T) {
	content := "MemTotal:       16384000 kB\n" +
		"MemFree:          512000 kB\n" +
		"MemAvailable:    8192000 kB\n" +
		"Buffers:          256000 kB\n"
	usage, err := parseMeminfo(content)
	if err != nil {
		t.Fatal(err)
	}
	if usage.totalBytes != 16384000*1024 {
		t.Fatalf("total = %d bytes", usage.totalBytes)
	}
	if usage.availableBytes != 8192000*1024 {
		t.Fatalf("available = %d bytes", usage.availableBytes)
	}
}

func TestParseMeminfoWithoutMemTotal(t *testing.T) {
	if _, err := parseMeminfo("SwapTotal: 0 kB\n"); !errors.Is(err, errMeminfoUnreadable) {
		t.Fatalf("err = %v, want errMeminfoUnreadable", err)
	}
	if _, err := parseMeminfo(""); !errors.Is(err, errMeminfoUnreadable) {
		t.Fatalf("err = %v, want errMeminfoUnreadable", err)
	}
}

func TestParseLoadavg(t *testing.T) {
	load, err := parseLoadavg("0.52 0.58 0.59 1/1087 12345\n")
	if err != nil {
		t.Fatal(err)
	}
	if load.oneMinute != 0.52 || load.fiveMinute != 0.58 {
		t.Fatalf("load = %+v", load)
	}
}

func TestParseLoadavgRejectsTruncatedFile(t *testing.T) {
	if _, err := parseLoadavg("0.52\n"); !errors.Is(err, errLoadUnreadable) {
		t.Fatalf("err = %v, want errLoadUnreadable", err)
	}
}

// TestSampleGPUsRetiresVanishedDevices: gauges are keyed by index, and a GPU
// that disappears (or a tool that starts reporting fewer devices) must not
// leave its last reading frozen on the dashboard forever.
func TestSampleGPUsRetiresVanishedDevices(t *testing.T) {
	original := sampleGPUUtilization
	t.Cleanup(func() { sampleGPUUtilization = original })
	three := []device.GPUSample{
		{UtilizationPercent: 10, MemoryUsedBytes: 1, MemoryTotalBytes: 2},
		{UtilizationPercent: 20, MemoryUsedBytes: 3, MemoryTotalBytes: 4},
		{UtilizationPercent: 30, MemoryUsedBytes: 5, MemoryTotalBytes: 6},
	}
	sampleGPUUtilization = func(context.Context) []device.GPUSample { return three }
	service := &Service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	diagnostics := observability.NewDiagnostics()
	reported := service.sampleGPUs(context.Background(), diagnostics, 0)
	if reported != 3 {
		t.Fatalf("reported %d devices, want 3", reported)
	}
	sampleGPUUtilization = func(context.Context) []device.GPUSample { return three[:1] }
	if reported = service.sampleGPUs(context.Background(), diagnostics, reported); reported != 1 {
		t.Fatalf("reported %d devices, want 1", reported)
	}
	var rendered bytes.Buffer
	diagnostics.RenderTo(&rendered)
	for _, line := range []string{
		"shift_gpu_1_utilization_percent 0",
		"shift_gpu_2_utilization_percent 0",
		"shift_gpu_2_memory_total_bytes 0",
	} {
		if !strings.Contains(rendered.String(), line) {
			t.Fatalf("vanished GPU gauge %q not retired:\n%s", line, rendered.String())
		}
	}
	if !strings.Contains(rendered.String(), "shift_gpu_0_utilization_percent 10") {
		t.Fatalf("the surviving GPU must keep its reading:\n%s", rendered.String())
	}
}

// TestRunSamplerJoinsTheGPULoop: the GPU loop is a sampler goroutine and
// shutdown is not over while it still publishes. runSampler must wait for it
// on the way out — otherwise a detached loop keeps gauges alive after the
// agent says it is done.
func TestRunSamplerJoinsTheGPULoop(t *testing.T) {
	original := sampleGPUUtilization
	t.Cleanup(func() { sampleGPUUtilization = original })
	var mu sync.Mutex
	alive := false
	started := make(chan struct{})
	var once sync.Once
	sampleGPUUtilization = func(ctx context.Context) []device.GPUSample {
		once.Do(func() { close(started) })
		mu.Lock()
		alive = true
		mu.Unlock()
		<-ctx.Done()
		// A vendor tool winding down: the loop exits only after this returns.
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		alive = false
		mu.Unlock()
		return nil
	}
	service := &Service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.runSampler(ctx, observability.NewDiagnostics())
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runSampler did not return after shutdown")
	}
	mu.Lock()
	defer mu.Unlock()
	if alive {
		t.Fatal("runSampler returned while the GPU loop was still running")
	}
}
