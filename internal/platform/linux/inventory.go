package linux

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/device"
	"shift.dev/shift/internal/model"
)

type Inventory struct {
	machineID string
}

func NewInventory(machineID string) *Inventory {
	return &Inventory{machineID: machineID}
}

func (i *Inventory) Inspect(ctx context.Context) (model.MachineCapabilities, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return model.MachineCapabilities{}, fmt.Errorf("hostname: %w", err)
	}
	kernel, err := kernelRelease()
	if err != nil {
		return model.MachineCapabilities{}, err
	}
	distribution := distributionName()
	memory, err := totalMemory()
	if err != nil {
		return model.MachineCapabilities{}, err
	}
	storage, err := storageDevices()
	if err != nil {
		return model.MachineCapabilities{}, err
	}
	criu := inspectCRIU(ctx)
	features := inspectFeatures()
	for _, flag := range cpuFlags() {
		features["cpu."+flag] = true
	}
	gpuReport := device.Detect(ctx)
	return model.MachineCapabilities{
		MachineID:    i.machineID,
		Hostname:     hostname,
		OS:           runtime.GOOS,
		Distribution: distribution,
		Kernel:       kernel,
		Architecture: runtime.GOARCH,
		CPUs:         runtime.NumCPU(),
		MemoryBytes:  memory,
		Storage:      storage,
		GPUs:         gpuReport.GPUs,
		GPURuntimes:  gpuReport.Runtimes,
		CRIU:         criu,
		Features:     features,
		AgentVersion: config.Version,
		ObservedAt:   time.Now().UTC(),
	}, nil
}

func kernelRelease() (string, error) {
	var value syscall.Utsname
	if err := syscall.Uname(&value); err != nil {
		return "", fmt.Errorf("uname: %w", err)
	}
	return charsToString(value.Release[:]), nil
}

func charsToString(value []int8) string {
	buffer := make([]byte, 0, len(value))
	for _, character := range value {
		if character == 0 {
			break
		}
		buffer = append(buffer, byte(character))
	}
	return string(buffer)
}

func distributionName() string {
	content, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "linux"
	}
	values := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			values[key] = strings.Trim(value, "\"")
		}
	}
	if values["PRETTY_NAME"] != "" {
		return values["PRETTY_NAME"]
	}
	return strings.TrimSpace(values["ID"] + " " + values["VERSION_ID"])
}

func totalMemory() (uint64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("open meminfo: %w", err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return value * 1024, nil
		}
	}
	return 0, errors.New("MemTotal not found")
}

func storageDevices() ([]model.StorageDevice, error) {
	content, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("read mountinfo: %w", err)
	}
	seen := make(map[string]bool)
	devices := make([]model.StorageDevice, 0)
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if separator < 0 || separator+1 >= len(fields) || len(fields) < 6 {
			continue
		}
		mountpoint := unescapeMount(fields[4])
		filesystem := fields[separator+1]
		if seen[mountpoint] || virtualFilesystem(filesystem) {
			continue
		}
		var stats syscall.Statfs_t
		if err := syscall.Statfs(mountpoint, &stats); err != nil {
			continue
		}
		seen[mountpoint] = true
		devices = append(devices, model.StorageDevice{
			Mountpoint:     mountpoint,
			Filesystem:     filesystem,
			TotalBytes:     stats.Blocks * uint64(stats.Bsize),
			AvailableBytes: stats.Bavail * uint64(stats.Bsize),
		})
	}
	if len(devices) == 0 {
		return nil, errors.New("no persistent storage mounts detected")
	}
	return devices, nil
}

func unescapeMount(value string) string {
	replacer := strings.NewReplacer("\\040", " ", "\\011", "\t", "\\012", "\n", "\\134", "\\")
	return replacer.Replace(value)
}

func virtualFilesystem(filesystem string) bool {
	switch filesystem {
	case "proc", "sysfs", "devtmpfs", "devpts", "tmpfs", "cgroup", "cgroup2", "securityfs", "debugfs", "tracefs", "pstore", "mqueue", "hugetlbfs", "fusectl", "configfs", "bpf", "autofs":
		return true
	default:
		return false
	}
}

func inspectCRIU(parent context.Context) model.CRIUCapabilities {
	capabilities := model.CRIUCapabilities{}
	path, err := exec.LookPath("criu")
	if err != nil {
		capabilities.Errors = []string{"criu executable not found"}
		return capabilities
	}
	capabilities.Installed = true
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	versionOutput, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err == nil {
		capabilities.Version = strings.TrimSpace(string(versionOutput))
	}
	check := exec.CommandContext(ctx, path, "check")
	output, err := check.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		capabilities.Errors = append(capabilities.Errors, message)
		return capabilities
	}
	capabilities.Healthy = true
	capabilities.Features = []string{"process_tree", "memory", "namespaces", "file_locks", "unix_sockets"}
	if featureCheck(parent, path, "mem_dirty_track") {
		capabilities.Features = append(capabilities.Features, "mem_dirty_track")
		capabilities.Features = append(capabilities.Features, "pre_dump")
	}
	return capabilities
}

func featureCheck(parent context.Context, path, feature string) bool {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, path, "check", "--feature", feature).Run() == nil
}

func inspectFeatures() map[string]bool {
	features := map[string]bool{
		"cgroup_v2":        exists("/sys/fs/cgroup/cgroup.controllers"),
		"pid_namespaces":   exists("/proc/self/ns/pid"),
		"net_namespaces":   exists("/proc/self/ns/net"),
		"user_namespaces":  exists("/proc/self/ns/user"),
		"mount_namespaces": exists("/proc/self/ns/mnt"),
		"overlayfs":        filesystemSupported("overlay"),
		"nftables":         commandExists("nft"),
		"gnu_tar":          commandExists("tar"),
	}
	if content, err := os.ReadFile("/proc/sys/vm/unprivileged_userfaultfd"); err == nil {
		features["unprivileged_userfaultfd"] = strings.TrimSpace(string(content)) == "1"
	}
	return features
}

func filesystemSupported(name string) bool {
	content, err := os.ReadFile("/proc/filesystems")
	return err == nil && strings.Contains(string(content), name)
}

func cpuFlags() []string {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "flags") {
			_, value, ok := strings.Cut(line, ":")
			if ok {
				return strings.Fields(value)
			}
		}
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
