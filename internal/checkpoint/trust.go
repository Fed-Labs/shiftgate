package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"shift.dev/shift/internal/model"
)

// ErrUntrustedSigner marks a manifest-admission refusal: the signature proves
// only that the manifest is self-consistent, and any key pair mints a
// "validly signed" manifest. Admission requires a trust anchor — this
// machine, an operator-pinned source, or an established peer session.
var ErrUntrustedSigner = errors.New("checkpoint signer is not admitted on this destination")

const maxTrustFileBytes = int64(1 << 20)

type trustFile struct {
	TrustedMachineIDs []string `json:"trusted_machine_ids"`
}

// TrustStore is the destination's checkpoint-signer admission policy. Local
// checkpoints are signed by this machine's own identity and are admitted
// through TrustLocal; operator pins live in a small JSON file beside the
// checkpoint repository; peer transfers do not consult the store at all —
// their manifests are admitted because the signer is bound to the CA-verified
// peer session that delivers them.
type TrustStore struct {
	mu      sync.Mutex
	path    string
	trusted map[string]bool
}

// OpenTrustStore loads the operator pins from path. A missing or empty file
// simply means no pins; a malformed one is an error, never silently ignored.
//
// The trust file is operator authority exactly like restore-policy.json, so
// it is read with the same hardening bar: O_NOFOLLOW (a symlink planted at
// the path is refused, never followed), regular-file / owner / mode checks
// against the opened descriptor — not a stat-then-read that a swap could
// defeat — and a bounded read so a FIFO, /dev/zero, or huge file cannot
// exhaust memory before the size check.
func OpenTrustStore(path string) (*TrustStore, error) {
	store := &TrustStore{path: path, trusted: make(map[string]bool)}
	descriptor, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ENOENT) {
		return store, nil
	}
	if errors.Is(err, syscall.ELOOP) {
		return nil, errors.New("checkpoint trust file must be a regular file, not a symlink")
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(descriptor), path)
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("checkpoint trust file must be a regular file")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("checkpoint trust file must not be group- or world-writable")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if euid := os.Geteuid(); stat.Uid != uint32(euid) && stat.Uid != 0 {
			return nil, errors.New("checkpoint trust file must be owned by root or the agent user")
		}
	}
	encoded, err := io.ReadAll(io.LimitReader(file, maxTrustFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > maxTrustFileBytes {
		return nil, errors.New("checkpoint trust file exceeds size limit")
	}
	var fileContent trustFile
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fileContent); err != nil {
		return nil, fmt.Errorf("decode checkpoint trust file: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("checkpoint trust file contains trailing JSON data")
		}
		return nil, fmt.Errorf("decode checkpoint trust file: %w", err)
	}
	for _, id := range fileContent.TrustedMachineIDs {
		if !validMachineID(id) {
			return nil, fmt.Errorf("checkpoint trust file contains invalid machine id %q", id)
		}
		store.trusted[id] = true
	}
	return store, nil
}

// TrustLocal admits checkpoints this machine signs itself. It is in-memory
// only: the local identity is derived from the state directory, not operator
// input, and never needs pinning.
func (t *TrustStore) TrustLocal(machineID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.trusted[machineID] = true
}

// Pin admits a source machine identity persistently and writes the pin set
// back to the trust file. This is the operator enrollment step.
func (t *TrustStore) Pin(machineID string) error {
	if !validMachineID(machineID) {
		return fmt.Errorf("invalid machine id %q", machineID)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.trusted[machineID] = true
	return t.persist()
}

// Admit enforces signer admission for non-peer ingest paths (import,
// operator archives, object-store mirror downloads): a manifest is accepted
// only when its signer is a trust anchor of this destination.
func (t *TrustStore) Admit(manifest model.CheckpointManifest) error {
	signer := manifest.Security.SignerMachineID
	t.mu.Lock()
	defer t.mu.Unlock()
	if signer == "" || !t.trusted[signer] {
		return fmt.Errorf("%w: %q", ErrUntrustedSigner, signer)
	}
	return nil
}

func (t *TrustStore) Trusted(machineID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.trusted[machineID]
}

// persist writes the pin set through a temp file + rename. The rename lands
// on the directory entry itself, so a symlink planted at the trust-file path
// after Open is replaced rather than followed — Pin can never write through
// it into an arbitrary root-owned file.
func (t *TrustStore) persist() error {
	file := trustFile{TrustedMachineIDs: make([]string, 0, len(t.trusted))}
	for id := range t.trusted {
		file.TrustedMachineIDs = append(file.TrustedMachineIDs, id)
	}
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(t.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".trust-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(append(encoded, '\n')); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, t.path)
}

func validMachineID(id string) bool {
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return false
	}
	return true
}
