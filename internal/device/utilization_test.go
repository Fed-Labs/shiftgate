package device

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseNvidiaUtilizationReportsEveryGPU(t *testing.T) {
	output := "NVIDIA GeForce RTX 4090, 512, 24564, 3\n" +
		"NVIDIA A100-SXM4-40GB, 40960, 40960, 100\n"
	samples := parseNvidiaUtilization(output)
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
	}
	first := samples[0]
	if first.Vendor != "NVIDIA" || first.Model != "NVIDIA GeForce RTX 4090" {
		t.Fatalf("unexpected identity: %+v", first)
	}
	if first.MemoryUsedBytes != 512*1024*1024 || first.MemoryTotalBytes != 24564*1024*1024 {
		t.Fatalf("memory = %d/%d bytes", first.MemoryUsedBytes, first.MemoryTotalBytes)
	}
	if first.UtilizationPercent != 3 {
		t.Fatalf("utilization = %v", first.UtilizationPercent)
	}
	if samples[1].UtilizationPercent != 100 || samples[1].MemoryUsedBytes != samples[1].MemoryTotalBytes {
		t.Fatalf("second sample = %+v", samples[1])
	}
}

func TestParseNvidiaUtilizationHandlesModelsWithCommas(t *testing.T) {
	// A model name containing a comma must not shift the numeric columns:
	// they are read from the end of the row, the name is everything before.
	samples := parseNvidiaUtilization("NVIDIA Tesla, Special Edition, 512, 24564, 3\n")
	if len(samples) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(samples))
	}
	if samples[0].Model != "NVIDIA Tesla, Special Edition" {
		t.Fatalf("model = %q", samples[0].Model)
	}
	if samples[0].MemoryUsedBytes != 512*1024*1024 || samples[0].UtilizationPercent != 3 {
		t.Fatalf("columns shifted: %+v", samples[0])
	}
}

func TestParseNvidiaUtilizationSkipsUnreadableRows(t *testing.T) {
	output := "NVIDIA H100, [N/A], 81559, 3\n" +
		"\n" +
		"only,three,columns\n" +
		"NVIDIA H100, 1024, 81559, not-a-number\n" +
		"NVIDIA H100, 1024, 81559, 5\n"
	samples := parseNvidiaUtilization(output)
	if len(samples) != 1 {
		t.Fatalf("expected 1 sample after skipping unreadable rows, got %d", len(samples))
	}
	if samples[0].UtilizationPercent != 5 {
		t.Fatalf("utilization = %v", samples[0].UtilizationPercent)
	}
}

func TestParseNvidiaUtilizationOnEmptyOutput(t *testing.T) {
	// nvidia-smi absent or failing produces no output and no samples — never
	// a fabricated zero reading.
	if samples := parseNvidiaUtilization(""); len(samples) != 0 {
		t.Fatalf("empty output produced %d samples", len(samples))
	}
}

func TestReadSysfsHelpers(t *testing.T) {
	directory := t.TempDir()
	if err := writeTestFile(filepath.Join(directory, "value"), "123\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(filepath.Join(directory, "name"), "AMD Radeon RX 7900 XTX\n"); err != nil {
		t.Fatal(err)
	}
	if value, ok := readSysfsUint(directory, "value"); !ok || value != 123 {
		t.Fatalf("readSysfsUint = %d, %v", value, ok)
	}
	if name, ok := readSysfsString(directory, "name"); !ok || name != "AMD Radeon RX 7900 XTX" {
		t.Fatalf("readSysfsString = %q, %v", name, ok)
	}
	if _, ok := readSysfsUint(directory, "name"); ok {
		t.Fatal("a text attribute parsed as a number")
	}
	if _, ok := readSysfsString(directory, "missing"); ok {
		t.Fatal("a missing attribute claimed to exist")
	}
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
