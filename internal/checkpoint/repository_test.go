package checkpoint

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/securestore"
)

func TestSignedEncryptedManifestRoundTrip(t *testing.T) {
	root := t.TempDir()
	machine, err := identity.Ensure(root + "/identity")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := securestore.Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := OpenRepository(root+"/checkpoints", keys)
	if err != nil {
		t.Fatal(err)
	}
	manifest := model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		ID: "checkpoint-1", Kind: model.CheckpointFull, CreatedAt: time.Now().UTC(),
		Workload: model.WorkloadSpec{ID: "workload-1", Name: "manifest-sensitive-workload-name", RootPath: "/srv/private", Environment: map[string]string{"SECRET": "manifest-secret-value-that-must-not-leak"}},
	}
	if err := SignManifest(&manifest, machine); err != nil {
		t.Fatal(err)
	}
	if err := repository.Save(manifest); err != nil {
		t.Fatal(err)
	}
	restored, err := repository.Load(manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Workload.Environment["SECRET"] != "manifest-secret-value-that-must-not-leak" {
		t.Fatal("manifest payload changed")
	}

	encoded, err := repository.ExportEncryptedManifest(manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("manifest-sensitive-workload-name")) || bytes.Contains(encoded, []byte("manifest-secret-value-that-must-not-leak")) {
		t.Fatal("encrypted manifest envelope exposed plaintext metadata")
	}
	decoded, err := repository.DecodeEncryptedManifest(manifest.ID, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ManifestSHA256 != manifest.ManifestSHA256 {
		t.Fatal("decoded encrypted manifest changed")
	}

	var envelope encryptedManifest
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext[len(ciphertext)-1] ^= 0xff
	envelope.Ciphertext = base64.RawStdEncoding.EncodeToString(ciphertext)
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DecodeEncryptedManifest(manifest.ID, tampered); err == nil {
		t.Fatal("tampered encrypted manifest authenticated")
	}
}

func TestManifestSignatureDetectsChange(t *testing.T) {
	machine, err := identity.Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifest := model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		ID: "checkpoint-1", Workload: model.WorkloadSpec{ID: "w", Name: "before"},
	}
	if err := SignManifest(&manifest, machine); err != nil {
		t.Fatal(err)
	}
	manifest.Workload.Name = "after"
	if err := VerifyManifest(manifest); err == nil {
		t.Fatal("changed manifest verified")
	}
}
