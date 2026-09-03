package agent

import (
	"errors"
	"testing"
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
