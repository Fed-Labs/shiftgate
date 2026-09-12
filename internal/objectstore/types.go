// Package objectstore provides a bounded-memory object-storage contract for
// encrypted SHIFT checkpoint objects. Implementations are intentionally
// independent from the control plane database so a deployment can use local
// disk, S3, or an S3-compatible service without changing migration code.
package objectstore

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrNotFound      = errors.New("object not found")
	ErrConflict      = errors.New("object already exists with different content")
	ErrIntegrity     = errors.New("object integrity check failed")
	ErrInvalidKey    = errors.New("invalid object key")
	ErrInvalidUpload = errors.New("invalid multipart upload")
	ErrInvalidPart   = errors.New("invalid multipart part")
	ErrRemote        = errors.New("remote object storage error")
	ErrUnsupported   = errors.New("object storage operation is unsupported")
	// ErrNoStateDirectory is returned by multipart upload-state methods on a
	// client built without a state directory. Such clients (the control
	// plane's read-only listing broker) can read, list, and delete objects
	// but cannot start or resume multipart uploads.
	ErrNoStateDirectory = errors.New("S3 client has no state directory; multipart upload state is unavailable")
)

type ObjectInfo struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	SHA256       string    `json:"sha256,omitempty"`
	ETag         string    `json:"etag,omitempty"`
	LastModified time.Time `json:"last_modified,omitempty"`
}

type MultipartUpload struct {
	UploadID       string    `json:"upload_id"`
	Key            string    `json:"key"`
	TotalSize      int64     `json:"total_size"`
	ExpectedSHA256 string    `json:"expected_sha256,omitempty"`
	InitiatedAt    time.Time `json:"initiated_at"`
}

type PartInfo struct {
	Number int    `json:"number"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	ETag   string `json:"etag,omitempty"`
}

// Store is safe for concurrent callers. Put and UploadPart consume readers
// once and never buffer an entire object or part in memory. Multipart upload
// state is durable in each implementation, allowing a process to resume after
// a crash by listing uploads and their completed parts.
type Store interface {
	Put(context.Context, string, io.Reader, int64, string) (ObjectInfo, error)
	Get(context.Context, string, io.Writer) (ObjectInfo, error)
	Head(context.Context, string) (ObjectInfo, error)
	Delete(context.Context, string) error

	InitiateMultipart(context.Context, string, int64, string) (MultipartUpload, error)
	UploadPart(context.Context, string, int, io.Reader, int64, string) (PartInfo, error)
	ListParts(context.Context, string) ([]PartInfo, error)
	CompleteMultipart(context.Context, string, []PartInfo) (ObjectInfo, error)
	AbortMultipart(context.Context, string) error
	ListMultipart(context.Context, string) ([]MultipartUpload, error)
	CleanupMultipart(context.Context, string, time.Time) (int, error)
}
