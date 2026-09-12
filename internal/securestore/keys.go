package securestore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"shift.dev/shift/internal/persistence"
)

const keySize = 32

type masterKeyRecord struct {
	Version   uint32     `json:"version"`
	Key       string     `json:"key"`
	CreatedAt time.Time  `json:"created_at"`
	RetiredAt *time.Time `json:"retired_at,omitempty"`
}

type keyringFile struct {
	FormatVersion int               `json:"format_version"`
	ActiveVersion uint32            `json:"active_version"`
	Keys          []masterKeyRecord `json:"keys"`
}

type wrappedKeyRecord struct {
	FormatVersion int       `json:"format_version"`
	WorkloadID    string    `json:"workload_id"`
	KeyVersion    uint32    `json:"key_version"`
	WrappedBy     uint32    `json:"wrapped_by"`
	Nonce         string    `json:"nonce"`
	Ciphertext    string    `json:"ciphertext"`
	CreatedAt     time.Time `json:"created_at"`
}

type Manager struct {
	mu      sync.RWMutex
	root    string
	keyring keyringFile
}

func Open(root string) (*Manager, error) {
	if err := os.MkdirAll(filepath.Join(root, "workload-keys"), 0o700); err != nil {
		return nil, fmt.Errorf("create secure store: %w", err)
	}
	manager := &Manager{root: root}
	path := filepath.Join(root, "master-keyring.json")
	if err := persistence.ReadJSON(path, &manager.keyring); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		key := make([]byte, keySize)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate master key: %w", err)
		}
		manager.keyring = keyringFile{
			FormatVersion: 1,
			ActiveVersion: 1,
			Keys: []masterKeyRecord{{
				Version:   1,
				Key:       base64.RawStdEncoding.EncodeToString(key),
				CreatedAt: time.Now().UTC(),
			}},
		}
		if err := persistence.WriteJSON(path, manager.keyring, 0o600); err != nil {
			return nil, err
		}
	}
	if manager.keyring.FormatVersion != 1 || len(manager.keyring.Keys) == 0 {
		return nil, errors.New("invalid master keyring")
	}
	return manager, nil
}

func (m *Manager) GetOrCreateWorkloadKey(workloadID string) (version uint32, key []byte, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	records, err := m.loadWorkloadRecords(workloadID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, nil, err
	}
	if len(records) > 0 {
		record := records[len(records)-1]
		key, err := m.unwrap(record)
		return record.KeyVersion, key, err
	}
	key = make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return 0, nil, err
	}
	if err := m.storeWorkloadKey(workloadID, 1, key, nil); err != nil {
		return 0, nil, err
	}
	return 1, append([]byte(nil), key...), nil
}

func (m *Manager) WorkloadKey(workloadID string, version uint32) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	records, err := m.loadWorkloadRecords(workloadID)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.KeyVersion == version {
			return m.unwrap(record)
		}
	}
	return nil, fmt.Errorf("workload key %s version %d: %w", workloadID, version, os.ErrNotExist)
}

func (m *Manager) ExportWorkloadKey(workloadID string, version uint32) ([]byte, error) {
	return m.WorkloadKey(workloadID, version)
}

func (m *Manager) ImportWorkloadKey(workloadID string, version uint32, key []byte) error {
	if len(key) != keySize {
		return errors.New("workload data key must be 256 bits")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	records, err := m.loadWorkloadRecords(workloadID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, record := range records {
		if record.KeyVersion != version {
			continue
		}
		existing, err := m.unwrap(record)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare(existing, key) != 1 {
			return errors.New("a different key already exists for this workload and version")
		}
		return nil
	}
	return m.storeWorkloadKey(workloadID, version, key, records)
}

func (m *Manager) RotateWorkloadKey(workloadID string) (version uint32, key []byte, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	records, err := m.loadWorkloadRecords(workloadID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, nil, err
	}
	version = 1
	if len(records) > 0 {
		version = records[len(records)-1].KeyVersion + 1
	}
	key = make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return 0, nil, err
	}
	if err := m.storeWorkloadKey(workloadID, version, key, records); err != nil {
		return 0, nil, err
	}
	return version, append([]byte(nil), key...), nil
}

func Derive(dataKey []byte, workloadID, purpose string) ([]byte, error) {
	if len(dataKey) != keySize {
		return nil, errors.New("invalid data key length")
	}
	return hkdf.Key(sha256.New, dataKey, []byte(workloadID), "shift/v1/"+purpose, keySize)
}

func Address(dataKey []byte, workloadID string, plaintext []byte) (string, error) {
	addressKey, err := Derive(dataKey, workloadID, "content-address")
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, addressKey)
	_, _ = mac.Write(plaintext)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func Encrypt(dataKey []byte, workloadID, purpose string, plaintext, associatedData []byte) (nonce, ciphertext []byte, err error) {
	aeadKey, err := Derive(dataKey, workloadID, purpose)
	if err != nil {
		return nil, nil, err
	}
	block, err := aes.NewCipher(aeadKey)
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, aead.Seal(nil, nonce, plaintext, associatedData), nil
}

func Decrypt(dataKey []byte, workloadID, purpose string, nonce, ciphertext, associatedData []byte) ([]byte, error) {
	aeadKey, err := Derive(dataKey, workloadID, purpose)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(aeadKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, errors.New("invalid nonce size")
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, associatedData)
	if err != nil {
		return nil, errors.New("authenticated decryption failed")
	}
	return plaintext, nil
}

func (m *Manager) storeWorkloadKey(workloadID string, version uint32, key []byte, records []wrappedKeyRecord) error {
	masterVersion := m.keyring.ActiveVersion
	master, err := m.masterKey(masterVersion)
	if err != nil {
		return err
	}
	aad := []byte(fmt.Sprintf("shift-workload-key-v1\n%s\n%d\n%d", workloadID, version, masterVersion))
	nonce, ciphertext, err := Encrypt(master, workloadID, "wrap-workload-key", key, aad)
	if err != nil {
		return err
	}
	records = append(records, wrappedKeyRecord{
		FormatVersion: 1,
		WorkloadID:    workloadID,
		KeyVersion:    version,
		WrappedBy:     masterVersion,
		Nonce:         base64.RawStdEncoding.EncodeToString(nonce),
		Ciphertext:    base64.RawStdEncoding.EncodeToString(ciphertext),
		CreatedAt:     time.Now().UTC(),
	})
	sort.Slice(records, func(i, j int) bool { return records[i].KeyVersion < records[j].KeyVersion })
	return persistence.WriteJSON(m.workloadPath(workloadID), records, 0o600)
}

func (m *Manager) unwrap(record wrappedKeyRecord) ([]byte, error) {
	master, err := m.masterKey(record.WrappedBy)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.RawStdEncoding.DecodeString(record.Nonce)
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(record.Ciphertext)
	if err != nil {
		return nil, err
	}
	aad := []byte(fmt.Sprintf("shift-workload-key-v1\n%s\n%d\n%d", record.WorkloadID, record.KeyVersion, record.WrappedBy))
	return Decrypt(master, record.WorkloadID, "wrap-workload-key", nonce, ciphertext, aad)
}

func (m *Manager) masterKey(version uint32) ([]byte, error) {
	for _, record := range m.keyring.Keys {
		if record.Version == version {
			decoded, err := base64.RawStdEncoding.DecodeString(record.Key)
			if err != nil || len(decoded) != keySize {
				return nil, errors.New("invalid master key record")
			}
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("master key version %d not found", version)
}

func (m *Manager) loadWorkloadRecords(workloadID string) ([]wrappedKeyRecord, error) {
	var records []wrappedKeyRecord
	if err := persistence.ReadJSON(m.workloadPath(workloadID), &records); err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.FormatVersion != 1 || record.WorkloadID != workloadID {
			return nil, errors.New("invalid workload key record")
		}
	}
	return records, nil
}

func (m *Manager) workloadPath(workloadID string) string {
	digest := sha256.Sum256([]byte(workloadID))
	return filepath.Join(m.root, "workload-keys", hex.EncodeToString(digest[:])+".json")
}
