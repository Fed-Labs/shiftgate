package device

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GPUSample is a point-in-time utilization reading for one GPU: memory in use
// and total, and the vendor's own busy percentage.
type GPUSample struct {
	Vendor             string
	Model              string
	MemoryUsedBytes    uint64
	MemoryTotalBytes   uint64
	UtilizationPercent float64
}

// SampleUtilization reads current GPU utilization: NVIDIA through nvidia-smi's
// query interface, AMD through the amdgpu driver's sysfs counters. GPUs whose
// vendor tool or sysfs attributes are absent produce no sample — utilization is
// measured, never estimated.
func SampleUtilization(parent context.Context) []GPUSample {
	var samples []GPUSample
	samples = append(samples, sampleNVIDIA(parent)...)
	samples = append(samples, sampleAMD()...)
	return samples
}

// sampleNVIDIA reads one CSV row per GPU from nvidia-smi.
func sampleNVIDIA(parent context.Context) []GPUSample {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	output := commandOutput(ctx, "nvidia-smi",
		"--query-gpu=name,memory.used,memory.total,utilization.gpu",
		"--format=csv,noheader,nounits")
	return parseNvidiaUtilization(output)
}

// parseNvidiaUtilization parses nvidia-smi's noheader CSV output. Memory is
// reported in MiB with nounits; utilization is a percentage. The name is
// everything before the last three fields, so a model containing a comma still
// parses.
func parseNvidiaUtilization(output string) []GPUSample {
	var samples []GPUSample
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 4 {
			continue
		}
		usedMiB, err := strconv.Atoi(strings.TrimSpace(fields[len(fields)-3]))
		if err != nil {
			continue
		}
		totalMiB, err := strconv.Atoi(strings.TrimSpace(fields[len(fields)-2]))
		if err != nil {
			continue
		}
		utilization, err := strconv.ParseFloat(strings.TrimSpace(fields[len(fields)-1]), 64)
		if err != nil {
			continue
		}
		samples = append(samples, GPUSample{
			Vendor:             "NVIDIA",
			Model:              strings.TrimSpace(strings.Join(fields[:len(fields)-3], ",")),
			MemoryUsedBytes:    uint64(usedMiB) * 1024 * 1024,
			MemoryTotalBytes:   uint64(totalMiB) * 1024 * 1024,
			UtilizationPercent: utilization,
		})
	}
	return samples
}

// sampleAMD reads the amdgpu driver's utilization counters from sysfs. A card
// without the busy counter is skipped rather than reported as idle, because a
// missing reading must not look like an unloaded GPU.
func sampleAMD() []GPUSample {
	var samples []GPUSample
	for _, cardPath := range amdCardPaths() {
		busy, ok := readSysfsUint(cardPath, "device", "gpu_busy_percent")
		if !ok {
			continue
		}
		sample := GPUSample{Vendor: "AMD", UtilizationPercent: float64(busy)}
		sample.MemoryUsedBytes, _ = readSysfsUint(cardPath, "device", "mem_info_vram_used")
		sample.MemoryTotalBytes, _ = readSysfsUint(cardPath, "device", "mem_info_vram_total")
		sample.Model = "AMD GPU"
		if model, ok := readSysfsString(cardPath, "device", "product_name"); ok && model != "" {
			sample.Model = model
		}
		samples = append(samples, sample)
	}
	return samples
}

// amdCardPaths lists sysfs paths of the machine's AMD DRM cards. It is the one
// enumeration both detection and sampling use, so the two never disagree about
// which cards exist.
func amdCardPaths() []string {
	matches, err := filepath.Glob("/sys/class/drm/card[0-9]*")
	if err != nil {
		return nil
	}
	var paths []string
	for _, cardPath := range matches {
		if !cardNameRegexp.MatchString(filepath.Base(cardPath)) {
			continue
		}
		vendor, err := os.ReadFile(filepath.Join(cardPath, "device", "vendor"))
		if err != nil || strings.TrimSpace(string(vendor)) != amdVendorID {
			continue
		}
		paths = append(paths, cardPath)
	}
	return paths
}

// readSysfsUint reads a numeric sysfs attribute.
func readSysfsUint(parts ...string) (uint64, bool) {
	content, ok := readSysfsString(parts...)
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseUint(content, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// readSysfsString reads a sysfs attribute, trimmed of whitespace.
func readSysfsString(parts ...string) (string, bool) {
	content, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(content)), true
}
