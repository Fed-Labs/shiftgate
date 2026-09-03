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

	"shift.dev/shift/internal/model"
)

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
