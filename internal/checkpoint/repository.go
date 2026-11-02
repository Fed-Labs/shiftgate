package checkpoint

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/persistence"
	"shift.dev/shift/internal/securestore"
)

const maxEncryptedManifestBytes = int64(8 << 20)

// idShape is the only form a checkpoint or workload id may take: a single
// path-safe component of alphanumerics, dots, underscores, and dashes. Ids
// are joined into filesystem paths by this repository (and by the chunk
// store), so a separator, a leading dot, or a bare "." / ".." is a traversal
// primitive, never an id. Peer-supplied manifests reach Save unvalidated
// upstream; this boundary refuses them regardless of what any caller admits.
var idShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// reservedRepositoryNames are the files the repository itself owns inside its
// root; a checkpoint id may never collide with them.
var reservedRepositoryNames = map[string]bool{
	"index.enc.json":       true,
	"trusted-sources.json": true,
}

// validateID enforces the id shape for one checkpoint or workload id.
func validateID(id, what string) error {
	if id == "" {
		return fmt.Errorf("%s is required", what)
	}
	if !idShape.MatchString(id) {
		return fmt.Errorf("invalid %s %q: ids must be single path-safe components matching %s", what, id, idShape)
	}
	if reservedRepositoryNames[id] {
		return fmt.Errorf("invalid %s %q: reserved repository name", what, id)
	}
	return nil
}

// checkpointDir resolves a validated checkpoint id to its directory under the
// repository root and proves the join cannot escape that root. Every path sink
// (Save, Load, Delete) routes through here, so even a validation regression
// cannot turn an id into an out-of-tree write or RemoveAll.
func (r *Repository) checkpointDir(id string) (string, error) {
	if err := validateID(id, "checkpoint id"); err != nil {
		return "", err
	}
	root := filepath.Clean(r.root)
	dir := filepath.Join(root, id)
	if filepath.Dir(dir) != root || filepath.Base(dir) != id || !pathContains(root, dir) {
		return "", fmt.Errorf("checkpoint id %q escapes the repository root", id)
	}
	return dir, nil
}

type Summary struct {
	ID           string               `json:"id"`
	WorkloadID   string               `json:"workload_id"`
	WorkloadName string               `json:"workload_name"`
	ParentID     string               `json:"parent_id,omitempty"`
	Kind         model.CheckpointKind `json:"kind"`
	CreatedAt    time.Time            `json:"created_at"`
	PlainBytes   int64                `json:"plain_bytes"`
	StoredBytes  int64                `json:"stored_bytes"`
	KeyVersion   uint32               `json:"key_version"`
	ManifestHash string               `json:"manifest_hash"`
}

type encryptedManifest struct {
	FormatVersion int    `json:"format_version"`
	WorkloadID    string `json:"workload_id"`
	CheckpointID  string `json:"checkpoint_id"`
	KeyVersion    uint32 `json:"key_version"`
	Nonce         string `json:"nonce"`
	Ciphertext    string `json:"ciphertext"`
}

type Repository struct {
	root  string
	keys  *securestore.Manager
	index *securestore.EncryptedCollection[Summary]
	trust *TrustStore
}

func OpenRepository(root string, keys *securestore.Manager) (*Repository, error) {
	index, err := securestore.OpenEncryptedCollection[Summary](filepath.Join(root, "index.enc.json"), "checkpoint-index-v1", keys)
	if err != nil {
		return nil, err
	}
	trust, err := OpenTrustStore(filepath.Join(root, "trusted-sources.json"))
	if err != nil {
		return nil, err
	}
	return &Repository{root: root, keys: keys, index: index, trust: trust}, nil
}

// Trust is the repository's signer-admission policy for non-peer ingest.
func (r *Repository) Trust() *TrustStore {
	return r.trust
}

func (r *Repository) Save(manifest model.CheckpointManifest) error {
	if err := validateID(manifest.ID, "checkpoint id"); err != nil {
		return err
	}
	if err := validateID(manifest.Workload.ID, "workload id"); err != nil {
		return err
	}
	if err := VerifyManifest(manifest); err != nil {
		return err
	}
	keyVersion, dataKey, err := r.keys.GetOrCreateWorkloadKey(manifest.Workload.ID)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	aad := []byte(fmt.Sprintf("shift-checkpoint-manifest-v1\n%s\n%s\n%d", manifest.Workload.ID, manifest.ID, keyVersion))
	nonce, ciphertext, err := securestore.Encrypt(dataKey, manifest.Workload.ID, "checkpoint-manifest", encoded, aad)
	if err != nil {
		return err
	}
	envelope := encryptedManifest{
		FormatVersion: 1,
		WorkloadID:    manifest.Workload.ID,
		CheckpointID:  manifest.ID,
		KeyVersion:    keyVersion,
		Nonce:         base64.RawStdEncoding.EncodeToString(nonce),
		Ciphertext:    base64.RawStdEncoding.EncodeToString(ciphertext),
	}
	directory, err := r.checkpointDir(manifest.ID)
	if err != nil {
		return err
	}
	path := filepath.Join(directory, "manifest.enc.json")
	if existing, err := r.Load(manifest.ID); err == nil {
		if existing.ManifestSHA256 == manifest.ManifestSHA256 {
			return nil
		}
		return errors.New("checkpoint id already exists with different contents")
	} else if !errors.Is(err, persistence.ErrNotFound) && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := persistence.WriteJSON(path, envelope, 0o600); err != nil {
		return err
	}
	summary := Summary{
		ID: manifest.ID, WorkloadID: manifest.Workload.ID, WorkloadName: manifest.Workload.Name,
		ParentID: manifest.ParentID, Kind: manifest.Kind, CreatedAt: manifest.CreatedAt,
		PlainBytes: manifest.Metrics.PlainBytes, StoredBytes: manifest.Metrics.StoredBytes,
		KeyVersion: keyVersion, ManifestHash: manifest.ManifestSHA256,
	}
	if err := r.index.Put(manifest.ID, summary); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func (r *Repository) Load(id string) (model.CheckpointManifest, error) {
	manifest, _, err := r.loadEncryptedManifest(id)
	return manifest, err
}

// ExportEncryptedManifest returns the authenticated encrypted envelope stored
// by the repository. The envelope can be mirrored without exposing manifest
// fields such as workload environment variables to the object store.
func (r *Repository) ExportEncryptedManifest(id string) ([]byte, error) {
	_, encoded, err := r.loadEncryptedManifest(id)
	return encoded, err
}

// DecodeEncryptedManifest authenticates and decrypts an exported manifest
// envelope using a workload key already present in this repository's key
// manager. It does not persist repository metadata.
func (r *Repository) DecodeEncryptedManifest(checkpointID string, encoded []byte) (model.CheckpointManifest, error) {
	manifest, _, err := r.decodeEncryptedManifest(checkpointID, encoded)
	return manifest, err
}

func (r *Repository) loadEncryptedManifest(id string) (model.CheckpointManifest, []byte, error) {
	directory, err := r.checkpointDir(id)
	if err != nil {
		return model.CheckpointManifest{}, nil, err
	}
	summary, err := r.index.Get(id)
	if err != nil {
		return model.CheckpointManifest{}, nil, err
	}
	encoded, err := readBoundedFile(filepath.Join(directory, "manifest.enc.json"), maxEncryptedManifestBytes)
	if err != nil {
		return model.CheckpointManifest{}, nil, err
	}
	manifest, envelope, err := r.decodeEncryptedManifest(id, encoded)
	if err != nil {
		return model.CheckpointManifest{}, nil, err
	}
	if envelope.FormatVersion != 1 || envelope.WorkloadID != summary.WorkloadID || envelope.CheckpointID != id || envelope.KeyVersion != summary.KeyVersion {
		return model.CheckpointManifest{}, nil, errors.New("checkpoint index and encrypted manifest disagree")
	}
	if manifest.ManifestSHA256 != summary.ManifestHash {
		return model.CheckpointManifest{}, nil, errors.New("manifest hash does not match checkpoint index")
	}
	return manifest, encoded, nil
}

func (r *Repository) decodeEncryptedManifest(checkpointID string, encoded []byte) (model.CheckpointManifest, encryptedManifest, error) {
	if err := validateID(checkpointID, "checkpoint id"); err != nil {
		return model.CheckpointManifest{}, encryptedManifest{}, err
	}
	if len(encoded) == 0 || int64(len(encoded)) > maxEncryptedManifestBytes {
		return model.CheckpointManifest{}, encryptedManifest{}, errors.New("encrypted checkpoint manifest exceeds size limit")
	}
	var envelope encryptedManifest
	if err := decodeJSON(encoded, &envelope); err != nil {
		return model.CheckpointManifest{}, encryptedManifest{}, fmt.Errorf("decode encrypted checkpoint manifest: %w", err)
	}
	if envelope.FormatVersion != 1 || envelope.WorkloadID == "" || envelope.CheckpointID != checkpointID || envelope.KeyVersion == 0 {
		return model.CheckpointManifest{}, encryptedManifest{}, errors.New("invalid encrypted checkpoint manifest envelope")
	}
	if err := validateID(envelope.WorkloadID, "workload id"); err != nil {
		return model.CheckpointManifest{}, encryptedManifest{}, err
	}
	dataKey, err := r.keys.WorkloadKey(envelope.WorkloadID, envelope.KeyVersion)
	if err != nil {
		return model.CheckpointManifest{}, encryptedManifest{}, err
	}
	nonce, err := base64.RawStdEncoding.DecodeString(envelope.Nonce)
	if err != nil {
		return model.CheckpointManifest{}, encryptedManifest{}, err
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		return model.CheckpointManifest{}, encryptedManifest{}, err
	}
	aad := []byte(fmt.Sprintf("shift-checkpoint-manifest-v1\n%s\n%s\n%d", envelope.WorkloadID, checkpointID, envelope.KeyVersion))
	plaintext, err := securestore.Decrypt(dataKey, envelope.WorkloadID, "checkpoint-manifest", nonce, ciphertext, aad)
	if err != nil {
		return model.CheckpointManifest{}, encryptedManifest{}, err
	}
	var manifest model.CheckpointManifest
	if err := decodeJSON(plaintext, &manifest); err != nil {
		return model.CheckpointManifest{}, encryptedManifest{}, err
	}
	if manifest.ID != checkpointID || manifest.Workload.ID != envelope.WorkloadID {
		return model.CheckpointManifest{}, encryptedManifest{}, errors.New("encrypted envelope and checkpoint manifest disagree")
	}
	if err := VerifyManifest(manifest); err != nil {
		return model.CheckpointManifest{}, encryptedManifest{}, err
	}
	return manifest, envelope, nil
}

func (r *Repository) List(workloadID string) []Summary {
	values := r.index.List()
	filtered := values[:0]
	for _, value := range values {
		if workloadID == "" || value.WorkloadID == workloadID {
			filtered = append(filtered, value)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].CreatedAt.After(filtered[j].CreatedAt) })
	return filtered
}

// Delete removes a checkpoint's encrypted manifest and its index entry. Chunks
// are content addressed and shared between checkpoints, so they are reclaimed
// by garbage collection rather than here; removing the manifest is what makes a
// checkpoint unreachable.
func (r *Repository) Delete(id string) error {
	directory, err := r.checkpointDir(id)
	if err != nil {
		return err
	}
	if _, err := r.index.Get(id); err != nil {
		return err
	}
	if err := r.index.Delete(id); err != nil {
		return err
	}
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("remove checkpoint directory: %w", err)
	}
	return nil
}

func SignManifest(manifest *model.CheckpointManifest, signer *identity.Identity) error {
	manifest.SourceIdentity = signer.Machine
	manifest.Security.SignerMachineID = signer.Machine.ID
	manifest.Security.Algorithm = "Ed25519+SHA-256"
	manifest.Security.ManifestSignature = ""
	manifest.ManifestSHA256 = ""
	canonical, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	manifest.ManifestSHA256 = hex.EncodeToString(digest[:])
	manifest.Security.ManifestSignature = base64.RawStdEncoding.EncodeToString(signer.Sign(digest[:]))
	return nil
}

func VerifyManifest(manifest model.CheckpointManifest) error {
	if err := validateID(manifest.ID, "checkpoint id"); err != nil {
		return err
	}
	if err := validateID(manifest.Workload.ID, "workload id"); err != nil {
		return err
	}
	if manifest.Format != model.StateFormatName || manifest.FormatVersion != model.StateFormatVersion {
		return errors.New("unsupported checkpoint state format")
	}
	if manifest.SourceIdentity.ID == "" || manifest.SourceIdentity.ID != manifest.Security.SignerMachineID {
		return errors.New("manifest signer identity mismatch")
	}
	publicID, err := identity.PublicKeyID(manifest.SourceIdentity.PublicKeyPEM)
	if err != nil || publicID != manifest.SourceIdentity.ID {
		return errors.New("source machine id does not match its public key")
	}
	signature, err := base64.RawStdEncoding.DecodeString(manifest.Security.ManifestSignature)
	if err != nil {
		return errors.New("invalid manifest signature encoding")
	}
	expectedHash := manifest.ManifestSHA256
	manifest.Security.ManifestSignature = ""
	manifest.ManifestSHA256 = ""
	canonical, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	if hex.EncodeToString(digest[:]) != expectedHash {
		return errors.New("manifest digest mismatch")
	}
	if err := identity.Verify(manifest.SourceIdentity.PublicKeyPEM, digest[:], signature); err != nil {
		return err
	}
	return nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	encoded, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > limit {
		return nil, errors.New("encrypted checkpoint manifest exceeds size limit")
	}
	return encoded, nil
}

func decodeJSON(encoded []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON data")
		}
		return err
	}
	return nil
}
