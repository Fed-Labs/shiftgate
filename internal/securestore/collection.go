package securestore

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"

	"shift.dev/shift/internal/persistence"
)

const systemMetadataKeyID = "__shift_system_metadata__"

type encryptedCollectionEnvelope struct {
	FormatVersion int    `json:"format_version"`
	KeyVersion    uint32 `json:"key_version"`
	Nonce         string `json:"nonce"`
	Ciphertext    string `json:"ciphertext"`
}

type encryptedCollectionData[T any] struct {
	FormatVersion int          `json:"format_version"`
	Revision      uint64       `json:"revision"`
	Items         map[string]T `json:"items"`
}

type EncryptedCollection[T any] struct {
	mu      sync.RWMutex
	path    string
	label   string
	manager *Manager
	data    encryptedCollectionData[T]
}

func OpenEncryptedCollection[T any](path, label string, manager *Manager) (*EncryptedCollection[T], error) {
	if manager == nil {
		return nil, errors.New("secure store manager is required")
	}
	if label == "" {
		return nil, errors.New("encrypted collection label is required")
	}
	collection := &EncryptedCollection[T]{
		path: path, label: label, manager: manager,
		data: encryptedCollectionData[T]{FormatVersion: 1, Items: make(map[string]T)},
	}
	var envelope encryptedCollectionEnvelope
	if err := persistence.ReadJSON(path, &envelope); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return collection, nil
		}
		return nil, err
	}
	if envelope.FormatVersion != 1 {
		return nil, fmt.Errorf("unsupported encrypted collection format %d", envelope.FormatVersion)
	}
	key, err := manager.WorkloadKey(systemMetadataKeyID, envelope.KeyVersion)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.RawStdEncoding.DecodeString(envelope.Nonce)
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		return nil, err
	}
	plaintext, err := Decrypt(key, systemMetadataKeyID, "metadata-collection", nonce, ciphertext, []byte(label))
	if err != nil {
		return nil, fmt.Errorf("decrypt collection %s: %w", label, err)
	}
	if err := json.Unmarshal(plaintext, &collection.data); err != nil {
		return nil, fmt.Errorf("decode collection %s: %w", label, err)
	}
	if collection.data.FormatVersion != 1 {
		return nil, fmt.Errorf("unsupported collection payload format %d", collection.data.FormatVersion)
	}
	if collection.data.Items == nil {
		collection.data.Items = make(map[string]T)
	}
	return collection, nil
}

func (c *EncryptedCollection[T]) Put(id string, value T) error {
	if id == "" {
		return errors.New("record id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	previous, existed := c.data.Items[id]
	c.data.Items[id] = value
	c.data.Revision++
	if err := c.persist(); err != nil {
		if existed {
			c.data.Items[id] = previous
		} else {
			delete(c.data.Items, id)
		}
		c.data.Revision--
		return err
	}
	return nil
}

func (c *EncryptedCollection[T]) Update(id string, update func(T) (T, error)) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero T
	current, ok := c.data.Items[id]
	if !ok {
		return zero, persistence.ErrNotFound
	}
	next, err := update(current)
	if err != nil {
		return zero, err
	}
	c.data.Items[id] = next
	c.data.Revision++
	if err := c.persist(); err != nil {
		c.data.Items[id] = current
		c.data.Revision--
		return zero, err
	}
	return next, nil
}

func (c *EncryptedCollection[T]) Get(id string) (T, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value, ok := c.data.Items[id]
	if !ok {
		var zero T
		return zero, persistence.ErrNotFound
	}
	return value, nil
}

func (c *EncryptedCollection[T]) List() []T {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make([]string, 0, len(c.data.Items))
	for key := range c.data.Items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]T, 0, len(keys))
	for _, key := range keys {
		values = append(values, c.data.Items[key])
	}
	return values
}

func (c *EncryptedCollection[T]) Delete(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.data.Items[id]
	if !ok {
		return persistence.ErrNotFound
	}
	delete(c.data.Items, id)
	c.data.Revision++
	if err := c.persist(); err != nil {
		c.data.Items[id] = current
		c.data.Revision--
		return err
	}
	return nil
}

func (c *EncryptedCollection[T]) Revision() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data.Revision
}

func (c *EncryptedCollection[T]) persist() error {
	keyVersion, key, err := c.manager.GetOrCreateWorkloadKey(systemMetadataKeyID)
	if err != nil {
		return err
	}
	plaintext, err := json.Marshal(c.data)
	if err != nil {
		return err
	}
	nonce, ciphertext, err := Encrypt(key, systemMetadataKeyID, "metadata-collection", plaintext, []byte(c.label))
	if err != nil {
		return err
	}
	envelope := encryptedCollectionEnvelope{
		FormatVersion: 1, KeyVersion: keyVersion,
		Nonce:      base64.RawStdEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
	}
	return persistence.WriteJSON(c.path, envelope, 0o600)
}
