// Package device implements SHIFT's GPU and device compatibility layer: it
// detects the accelerators a machine actually has — vendor, model, driver,
// memory, compute capability, and the device files a restored process would
// need opened — along with the CUDA and ROCm environments installed on the
// machine. Detection only reports what the machine proves: a vendor tool that
// is not installed yields no entry, never a guessed one.
//
// The checkpoint/restore flag is an advertised capability, not a promise. On
// NVIDIA it is derived from the driver and toolkit versions that ship the CUDA
// checkpoint APIs; on AMD it is always false, because ROCm has no process GPU
// state checkpoint API for SHIFT to drive. Whether a workload may migrate is
// decided by the compatibility checker from these facts, never assumed.
package device

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"

	"shift.dev/shift/internal/model"
)

// trustedLookPath resolves a vendor tool like exec.LookPath but refuses
// binaries that could spoof this machine's device inventory: the GPU vendor,
// model, driver, and checkpoint/restore claims all come from these tools'
// output, so a shadowed binary in a user-writable PATH entry would forge
// device identities. The resolved file must be a regular, non-group/
// world-writable binary owned by root or by the agent's own user.
func trustedLookPath(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return "", errUntrustedBinary
	}
	stats, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stats.Uid != 0 && stats.Uid != uint32(os.Geteuid())) {
		return "", errUntrustedBinary
	}
	return path, nil
}

var errUntrustedBinary = errors.New("executable is not a regular root-owned file with safe permissions")

// Report is everything the device layer can prove about one machine.
type Report struct {
	GPUs     []model.GPUDevice
	Runtimes []model.GPURuntime
}

// Detect inventories the machine's GPUs and GPU software environments. It
// never fails because of absent hardware: a machine with no accelerators
// reports empty slices, which is a valid and useful answer.
func Detect(ctx context.Context) Report {
	report := Report{}
	report.GPUs = append(report.GPUs, detectNVIDIA(ctx)...)
	report.GPUs = append(report.GPUs, detectAMD(ctx)...)
	if cuda := detectCUDA(ctx); cuda.Installed {
		report.Runtimes = append(report.Runtimes, cuda)
	}
	if rocm := detectROCm(); rocm.Installed {
		report.Runtimes = append(report.Runtimes, rocm)
	}
	return report
}
