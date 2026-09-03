package linux

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func ProcessStartTicks(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, errors.New("invalid pid")
	}
	content, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	closing := strings.LastIndexByte(string(content), ')')
	if closing < 0 || closing+2 >= len(content) {
		return 0, errors.New("malformed process stat")
	}
	fields := strings.Fields(string(content[closing+2:]))
	if len(fields) <= 19 {
		return 0, errors.New("process stat missing start time")
	}
	value, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process start time: %w", err)
	}
	return value, nil
}

func ProcessAlive(pid int, expectedStartTicks uint64) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	actual, err := ProcessStartTicks(pid)
	return err == nil && actual == expectedStartTicks
}
