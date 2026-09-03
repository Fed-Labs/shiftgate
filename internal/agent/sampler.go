package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shift.dev/shift/internal/device"
	"shift.dev/shift/internal/observability"
)

// Sampler cadence: host resources are cheap to read and drift fast; GPU
// utilization spawns a vendor tool, so it runs less often.
const (
	hostSampleInterval = 30 * time.Second
	gpuSampleInterval  = 5 * time.Minute
)

// runSampler publishes resource and agent-health gauges on diagnostics until
// ctx is done. Sampling failures return an error to the caller (which logs it
// at debug level); the previous gauge values stay in place, because a failed
// read must not look like an idle or empty machine.
func (s *Service) runSampler(ctx context.Context, diagnostics *observability.Diagnostics) {
	s.sampleHost(diagnostics)
	go s.sampleGPULoop(ctx, diagnostics)
	ticker := time.NewTicker(hostSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.sampleHost(diagnostics); err != nil {
				s.logger.Debug("host sampling failed", "error", err)
			}
		}
	}
}

// sampleGPULoop samples GPU utilization on its slower cadence.
func (s *Service) sampleGPULoop(ctx context.Context, diagnostics *observability.Diagnostics) {
	s.sampleGPUs(ctx, diagnostics)
	ticker := time.NewTicker(gpuSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sampleGPUs(ctx, diagnostics)
		}
	}
}

// sampleHost publishes memory, load, storage, and workload-count gauges, plus
// the sample timestamp — a stale value is the agent-health signal, since it
// only advances while the sampler goroutine is running.
func (s *Service) sampleHost(diagnostics *observability.Diagnostics) error {
	memory, err := sampleMemory()
	if err != nil {
		return err
	}
	availableRatio := 0.0
	if memory.totalBytes > 0 {
		availableRatio = float64(memory.availableBytes) / float64(memory.totalBytes)
	}
	diagnostics.SetGauge("memory_available_bytes", float64(memory.availableBytes))
	diagnostics.SetGauge("memory_total_bytes", float64(memory.totalBytes))
	diagnostics.SetGauge("memory_available_ratio", availableRatio)
	if load, err := sampleLoad(); err == nil {
		diagnostics.SetGauge("host_load_1m", load.oneMinute)
		diagnostics.SetGauge("host_load_5m", load.fiveMinute)
	}
	if storage, err := sampleStorage(s.config.StateDir); err == nil {
		diagnostics.SetGauge("state_storage_available_bytes", float64(storage.availableBytes))
		diagnostics.SetGauge("state_storage_total_bytes", float64(storage.totalBytes))
	}
	if s.runtime != nil {
		diagnostics.SetGauge("workloads_tracked", float64(len(s.runtime.List())))
	}
	diagnostics.SetGauge("host_sample_timestamp_seconds", float64(time.Now().Unix()))
	return nil
}

// sampleGPUs publishes per-GPU utilization and memory gauges. GPUs whose vendor
// tool or sysfs counters are absent produce no gauges — SHIFT reports measured
// utilization only, never a zero standing in for an unreadable sensor.
func (s *Service) sampleGPUs(ctx context.Context, diagnostics *observability.Diagnostics) {
	for index, sample := range device.SampleUtilization(ctx) {
		prefix := "gpu_" + strconv.Itoa(index) + "_"
		diagnostics.SetGauge(prefix+"utilization_percent", sample.UtilizationPercent)
		diagnostics.SetGauge(prefix+"memory_used_bytes", float64(sample.MemoryUsedBytes))
		diagnostics.SetGauge(prefix+"memory_total_bytes", float64(sample.MemoryTotalBytes))
	}
}

type memoryUsage struct {
	totalBytes     uint64
	availableBytes uint64
}

var errMeminfoUnreadable = errors.New("MemTotal missing from /proc/meminfo")

// sampleMemory reads MemTotal and MemAvailable from /proc/meminfo. MemAvailable
// is the kernel's own estimate of memory usable without swapping, which is the
// number a scheduler cares about.
func sampleMemory() (memoryUsage, error) {
	content, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return memoryUsage{}, err
	}
	return parseMeminfo(string(content))
}

// parseMeminfo extracts MemTotal and MemAvailable, in bytes, from the contents
// of /proc/meminfo.
func parseMeminfo(content string) (memoryUsage, error) {
	usage := memoryUsage{}
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			usage.totalBytes = value * 1024
		case "MemAvailable:":
			usage.availableBytes = value * 1024
		}
	}
	if usage.totalBytes == 0 {
		return memoryUsage{}, errMeminfoUnreadable
	}
	return usage, nil
}

var errLoadUnreadable = errors.New("load averages missing from /proc/loadavg")

type loadAverages struct {
	oneMinute  float64
	fiveMinute float64
}

// sampleLoad reads the load averages from /proc/loadavg.
func sampleLoad() (loadAverages, error) {
	content, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return loadAverages{}, err
	}
	return parseLoadavg(string(content))
}

// parseLoadavg extracts the one- and five-minute load averages.
func parseLoadavg(content string) (loadAverages, error) {
	fields := strings.Fields(content)
	if len(fields) < 2 {
		return loadAverages{}, errLoadUnreadable
	}
	var load loadAverages
	load.oneMinute, _ = strconv.ParseFloat(fields[0], 64)
	load.fiveMinute, _ = strconv.ParseFloat(fields[1], 64)
	return load, nil
}

type storageUsage struct {
	totalBytes     uint64
	availableBytes uint64
}

// sampleStorage reports free space on the filesystem holding the state
// directory — the disk a checkpoint fills.
func sampleStorage(stateDir string) (storageUsage, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(filepath.Clean(stateDir), &stats); err != nil {
		return storageUsage{}, err
	}
	return storageUsage{
		totalBytes:     stats.Blocks * uint64(stats.Bsize),
		availableBytes: stats.Bavail * uint64(stats.Bsize),
	}, nil
}
