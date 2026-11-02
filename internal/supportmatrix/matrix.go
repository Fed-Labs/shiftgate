// Package supportmatrix is the operator-facing surface of the release support
// contract: loading docs/support-matrix.json, probing the local machine, and
// verifying restored state. The decision logic lives in the compatibility
// package so the same rules govern the runtime's compatibility gate; the
// names here are aliases to those types.
package supportmatrix

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/compatibility"
	"shift.dev/shift/internal/securestore"
)

type Matrix = compatibility.SupportMatrix
type Policy = compatibility.SupportPolicy
type Platform = compatibility.SupportPlatform
type GPU = compatibility.SupportGPU
type Evidence = compatibility.SupportEvidence
type Decision = compatibility.SupportDecision

// Policy action vocabulary.
const (
	ActionAllow               = compatibility.ActionAllow
	ActionBlockAutomatic      = compatibility.ActionBlockAutomatic
	ActionExperimentalOnly    = compatibility.ActionExperimentalOnly
	ActionExternalGPURequired = compatibility.ActionExternalGPURequired
)

func Load(path string) (Matrix, error) {
	return compatibility.LoadSupportMatrix(path)
}

func Probe(ctx context.Context, matrix Matrix) Evidence {
	evidence := Evidence{MatrixVersion: matrix.Version, ObservedAt: time.Now().UTC(), OS: runtime.GOOS, Architecture: runtime.GOARCH, Distribution: distributionID(), Kernel: kernelVersion(), PlatformStatus: "unknown"}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		evidence.CRIUCheck = "not-run: unsupported platform"
		evidence.PlatformStatus = "unsupported"
		return evidence
	}
	version, versionErr := criuVersion(ctx)
	evidence.CRIU = version
	if versionErr != nil {
		evidence.CRIUCheck = "not-run: " + versionErr.Error()
		evidence.PlatformStatus = "unknown"
		return evidence
	}
	check := exec.CommandContext(ctx, "criu", "check")
	if output, err := check.CombinedOutput(); err != nil {
		evidence.CRIUCheck = "failed: " + strings.TrimSpace(string(output))
		evidence.PlatformStatus = "unsupported"
	} else {
		evidence.CRIUCheck = "passed"
		evidence.PlatformStatus = "probe-passed"
	}
	if version == "" {
		evidence.Notes = append(evidence.Notes, "CRIU version could not be parsed")
	}
	return evidence
}

func kernelVersion() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(data))
}

// distributionID reads /etc/os-release and returns the "id-version" form the
// support matrix matches against ("ubuntu-24.04"), falling back to "unknown".
func distributionID() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "unknown"
	}
	var id, version string
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"`)
		switch key {
		case "ID":
			id = value
		case "VERSION_ID":
			version = value
		}
	}
	if id == "" {
		return "unknown"
	}
	if version == "" {
		return id
	}
	return id + "-" + version
}

func criuVersion(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, "criu", "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("criu unavailable: %w", err)
	}
	parts := strings.Fields(string(output))
	for _, part := range parts {
		if _, parseErr := parseVersion(part); parseErr == nil {
			return part, nil
		}
	}
	return "", errors.New("could not parse criu version")
}

// VerifyState reopens the encrypted key collection, checkpoint index, and
// chunk store from an isolated restored state directory. Every indexed manifest
// is authenticated and every referenced asset is decrypted and hash-checked.
// It never bootstraps missing material: a restored tree without its keyrings
// fails here instead of having keys minted over the damage, and an empty
// checkpoint index is a failed verification, never a vacuous pass.
func VerifyState(ctx context.Context, stateDir string) error {
	keys, err := securestore.OpenExisting(filepath.Join(stateDir, "keys"))
	if err != nil {
		return fmt.Errorf("open restored keys: %w", err)
	}
	repository, err := checkpoint.OpenRepository(filepath.Join(stateDir, "checkpoints"), keys)
	if err != nil {
		return fmt.Errorf("open restored checkpoint repository: %w", err)
	}
	chunks, err := chunkstore.Open(filepath.Join(stateDir, "objects"), 4<<20, keys)
	if err != nil {
		return fmt.Errorf("open restored chunk store: %w", err)
	}
	summaries := repository.List("")
	if len(summaries) == 0 {
		return errors.New("restored checkpoint index is empty: nothing was verified, which a real backup never is")
	}
	for _, summary := range summaries {
		manifest, err := repository.Load(summary.ID)
		if err != nil {
			return fmt.Errorf("verify checkpoint %s: %w", summary.ID, err)
		}
		for _, asset := range manifest.Assets {
			if err := chunks.ValidateAsset(ctx, manifest.Workload.ID, asset); err != nil {
				return fmt.Errorf("verify checkpoint %s asset %s: %w", summary.ID, asset.Name, err)
			}
		}
	}
	return nil
}

func parseVersion(value string) ([3]int, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	parts := strings.Split(value, ".")
	if len(parts) < 2 {
		return [3]int{}, errors.New("not a version")
	}
	var result [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		var err error
		result[i], err = strconv.Atoi(parts[i])
		if err != nil {
			return [3]int{}, err
		}
	}
	return result, nil
}
