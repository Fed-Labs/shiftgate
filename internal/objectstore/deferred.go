package objectstore

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// ErrCredentialsUnavailable reports that a Deferred store has no backend yet:
// the credentials the backend needs have not arrived. It is distinct from
// every remote error so callers can tell "not configured yet" from "the store
// is broken", and mirrors never mistake it for a stored-object problem.
var ErrCredentialsUnavailable = errors.New("object store credentials are not available yet")

// Deferred is a Store whose backend appears later. The hosted-storage mode
// builds it at startup with no credentials and swaps in an S3 store when the
// control plane first issues them; until then every operation fails closed
// with ErrCredentialsUnavailable rather than pretending to store anything.
// Swap atomically replaces the delegate, and an operation that already
// resolved its delegate keeps running against it, so a credential rotation
// never tears down a transfer mid-flight.
type Deferred struct {
	mu    sync.RWMutex
	store *S3
}

// NewDeferred returns a store with no backend installed.
func NewDeferred() *Deferred {
	return &Deferred{}
}

// Swap installs store as the delegate, replacing any previous one. A nil
// store is ignored.
func (deferred *Deferred) Swap(store *S3) {
	if store == nil {
		return
	}
	deferred.mu.Lock()
	defer deferred.mu.Unlock()
	deferred.store = store
}

// SetCredentials rotates the credentials of the installed delegate without
// replacing the store, so multipart and upload state survive a refresh.
func (deferred *Deferred) SetCredentials(accessKeyID, secretAccessKey, sessionToken string) error {
	deferred.mu.RLock()
	store := deferred.store
	deferred.mu.RUnlock()
	if store == nil {
		return ErrCredentialsUnavailable
	}
	store.SetCredentials(accessKeyID, secretAccessKey, sessionToken)
	return nil
}

// Available reports whether a delegate is installed.
func (deferred *Deferred) Available() bool {
	deferred.mu.RLock()
	defer deferred.mu.RUnlock()
	return deferred.store != nil
}

func (deferred *Deferred) delegate() (*S3, error) {
	deferred.mu.RLock()
	defer deferred.mu.RUnlock()
	if deferred.store == nil {
		return nil, ErrCredentialsUnavailable
	}
	return deferred.store, nil
}

func (deferred *Deferred) Put(ctx context.Context, key string, reader io.Reader, size int64, expectedSHA256 string) (ObjectInfo, error) {
	store, err := deferred.delegate()
	if err != nil {
		return ObjectInfo{}, err
	}
	return store.Put(ctx, key, reader, size, expectedSHA256)
}

func (deferred *Deferred) Get(ctx context.Context, key string, writer io.Writer) (ObjectInfo, error) {
	store, err := deferred.delegate()
	if err != nil {
		return ObjectInfo{}, err
	}
	return store.Get(ctx, key, writer)
}

func (deferred *Deferred) Head(ctx context.Context, key string) (ObjectInfo, error) {
	store, err := deferred.delegate()
	if err != nil {
		return ObjectInfo{}, err
	}
	return store.Head(ctx, key)
}

func (deferred *Deferred) Delete(ctx context.Context, key string) error {
	store, err := deferred.delegate()
	if err != nil {
		return err
	}
	return store.Delete(ctx, key)
}

func (deferred *Deferred) InitiateMultipart(ctx context.Context, key string, totalSize int64, expectedSHA256 string) (MultipartUpload, error) {
	store, err := deferred.delegate()
	if err != nil {
		return MultipartUpload{}, err
	}
	return store.InitiateMultipart(ctx, key, totalSize, expectedSHA256)
}

func (deferred *Deferred) UploadPart(ctx context.Context, uploadID string, number int, reader io.Reader, size int64, expectedSHA256 string) (PartInfo, error) {
	store, err := deferred.delegate()
	if err != nil {
		return PartInfo{}, err
	}
	return store.UploadPart(ctx, uploadID, number, reader, size, expectedSHA256)
}

func (deferred *Deferred) ListParts(ctx context.Context, uploadID string) ([]PartInfo, error) {
	store, err := deferred.delegate()
	if err != nil {
		return nil, err
	}
	return store.ListParts(ctx, uploadID)
}

func (deferred *Deferred) CompleteMultipart(ctx context.Context, uploadID string, parts []PartInfo) (ObjectInfo, error) {
	store, err := deferred.delegate()
	if err != nil {
		return ObjectInfo{}, err
	}
	return store.CompleteMultipart(ctx, uploadID, parts)
}

func (deferred *Deferred) AbortMultipart(ctx context.Context, uploadID string) error {
	store, err := deferred.delegate()
	if err != nil {
		return err
	}
	return store.AbortMultipart(ctx, uploadID)
}

func (deferred *Deferred) ListMultipart(ctx context.Context, prefix string) ([]MultipartUpload, error) {
	store, err := deferred.delegate()
	if err != nil {
		return nil, err
	}
	return store.ListMultipart(ctx, prefix)
}

func (deferred *Deferred) CleanupMultipart(ctx context.Context, prefix string, before time.Time) (int, error) {
	store, err := deferred.delegate()
	if err != nil {
		return 0, err
	}
	return store.CleanupMultipart(ctx, prefix, before)
}

var _ Store = (*Deferred)(nil)
