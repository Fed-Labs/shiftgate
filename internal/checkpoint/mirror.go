package checkpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/objectstore"
)

var ErrMirrorDisabled = errors.New("checkpoint object-store mirror is disabled")

const (
	multipartThresholdBytes = int64(8 << 20)
	multipartPartSizeBytes  = int64(8 << 20)
)

type MirrorResult struct {
	CheckpointID string `json:"checkpoint_id"`
	Objects      int    `json:"objects"`
	Bytes        int64  `json:"bytes"`
}

type Mirror struct {
	store      objectstore.Store
	chunks     *chunkstore.Store
	repository *Repository
}

func NewMirror(store objectstore.Store, chunks *chunkstore.Store, repository *Repository) *Mirror {
	if store == nil || chunks == nil || repository == nil {
		return nil
	}
	return &Mirror{store: store, chunks: chunks, repository: repository}
}

// Upload persists the encrypted manifest envelope and encrypted chunk
// ciphertexts. The object store never receives workload keys or plaintext
// checkpoint state or metadata. Chunk object keys are content addressed, so
// retries and repeated checkpoints deduplicate naturally at the backend.
func (mirror *Mirror) Upload(ctx context.Context, manifest model.CheckpointManifest) (MirrorResult, error) {
	if mirror == nil {
		return MirrorResult{}, ErrMirrorDisabled
	}
	if err := VerifyManifest(manifest); err != nil {
		return MirrorResult{}, err
	}
	storedManifest, err := mirror.repository.Load(manifest.ID)
	if err != nil {
		return MirrorResult{}, fmt.Errorf("load local checkpoint manifest: %w", err)
	}
	if storedManifest.ManifestSHA256 != manifest.ManifestSHA256 {
		return MirrorResult{}, errors.New("local checkpoint manifest differs from upload request")
	}
	refs := uniqueChunkRefs(storedManifest.Assets)
	result := MirrorResult{CheckpointID: manifest.ID}
	for _, ref := range refs {
		key := chunkObjectKey(ref)
		if info, err := mirror.store.Head(ctx, key); err == nil {
			if err := verifyMirroredObject(info, ref.StoredSize, ref.CipherSHA256); err != nil {
				return result, fmt.Errorf("verify mirrored chunk %s: %w", ref.Address, err)
			}
			result.Objects++
			result.Bytes += ref.StoredSize
			continue
		} else if !errors.Is(err, objectstore.ErrNotFound) {
			return result, err
		}
		info, putErr, exportErr := mirror.uploadChunk(ctx, key, ref)
		if putErr != nil {
			return result, fmt.Errorf("mirror chunk %s: %w", ref.Address, putErr)
		}
		if exportErr != nil {
			return result, fmt.Errorf("export chunk %s: %w", ref.Address, exportErr)
		}
		if err := verifyMirroredObject(info, ref.StoredSize, ref.CipherSHA256); err != nil {
			return result, fmt.Errorf("verify uploaded chunk %s: %w", ref.Address, err)
		}
		result.Objects++
		result.Bytes += ref.StoredSize
	}
	encoded, err := mirror.repository.ExportEncryptedManifest(manifest.ID)
	if err != nil {
		return result, fmt.Errorf("export encrypted checkpoint manifest: %w", err)
	}
	manifestKey := checkpointManifestKey(manifest.ID)
	manifestInfo, err := mirror.store.Put(ctx, manifestKey, bytes.NewReader(encoded), int64(len(encoded)), sha256Bytes(encoded))
	if err != nil {
		return result, fmt.Errorf("mirror checkpoint manifest: %w", err)
	}
	if err := verifyMirroredObject(manifestInfo, int64(len(encoded)), sha256Bytes(encoded)); err != nil {
		return result, fmt.Errorf("verify mirrored checkpoint manifest: %w", err)
	}
	result.Objects++
	result.Bytes += int64(len(encoded))
	return result, nil
}

func (mirror *Mirror) uploadChunk(ctx context.Context, key string, ref model.ChunkRef) (objectstore.ObjectInfo, error, error) {
	reader, writer := io.Pipe()
	exportDone := make(chan error, 1)
	go func() {
		err := mirror.chunks.ExportChunk(ref, writer)
		_ = writer.CloseWithError(err)
		exportDone <- err
	}()

	var info objectstore.ObjectInfo
	var uploadErr error
	if ref.StoredSize < multipartThresholdBytes {
		info, uploadErr = mirror.store.Put(ctx, key, reader, ref.StoredSize, ref.CipherSHA256)
	} else {
		info, uploadErr = mirror.putMultipart(ctx, key, ref, reader)
	}
	_ = reader.CloseWithError(uploadErr)
	exportErr := <-exportDone
	return info, uploadErr, exportErr
}

func (mirror *Mirror) putMultipart(ctx context.Context, key string, ref model.ChunkRef, reader io.Reader) (objectstore.ObjectInfo, error) {
	upload, err := mirror.findOrCreateMultipart(ctx, key, ref)
	if err != nil {
		return objectstore.ObjectInfo{}, err
	}
	parts, err := mirror.store.ListParts(ctx, upload.UploadID)
	if err != nil {
		return objectstore.ObjectInfo{}, err
	}
	partWriter := &multipartChunkWriter{
		ctx:      ctx,
		store:    mirror.store,
		uploadID: upload.UploadID,
		partSize: multipartPartSizeBytes,
		existing: make(map[int]objectstore.PartInfo, len(parts)),
	}
	for _, part := range parts {
		partWriter.existing[part.Number] = part
	}
	if _, err := io.Copy(partWriter, reader); err != nil {
		return objectstore.ObjectInfo{}, err
	}
	if err := partWriter.Close(); err != nil {
		return objectstore.ObjectInfo{}, err
	}
	return mirror.store.CompleteMultipart(ctx, upload.UploadID, partWriter.parts)
}

func (mirror *Mirror) findOrCreateMultipart(ctx context.Context, key string, ref model.ChunkRef) (objectstore.MultipartUpload, error) {
	uploads, err := mirror.store.ListMultipart(ctx, key)
	if err != nil {
		return objectstore.MultipartUpload{}, err
	}
	for _, upload := range uploads {
		if upload.Key == key && upload.TotalSize == ref.StoredSize && strings.EqualFold(upload.ExpectedSHA256, ref.CipherSHA256) {
			return upload, nil
		}
	}
	return mirror.store.InitiateMultipart(ctx, key, ref.StoredSize, ref.CipherSHA256)
}

type multipartChunkWriter struct {
	ctx      context.Context
	store    objectstore.Store
	uploadID string
	partSize int64
	existing map[int]objectstore.PartInfo
	buffer   bytes.Buffer
	parts    []objectstore.PartInfo
	nextPart int
}

func (writer *multipartChunkWriter) Write(value []byte) (int, error) {
	total := 0
	for len(value) > 0 {
		if err := writer.ctx.Err(); err != nil {
			return total, err
		}
		remaining := writer.partSize - int64(writer.buffer.Len())
		count := len(value)
		if int64(count) > remaining {
			count = int(remaining)
		}
		_, _ = writer.buffer.Write(value[:count])
		total += count
		value = value[count:]
		if int64(writer.buffer.Len()) == writer.partSize {
			if err := writer.flushPart(); err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

func (writer *multipartChunkWriter) Close() error {
	if writer.buffer.Len() == 0 {
		return nil
	}
	return writer.flushPart()
}

func (writer *multipartChunkWriter) flushPart() error {
	writer.nextPart++
	data := append([]byte(nil), writer.buffer.Bytes()...)
	digest := sha256Bytes(data)
	part, exists := writer.existing[writer.nextPart]
	if !exists || part.Size != int64(len(data)) || !strings.EqualFold(part.SHA256, digest) {
		var err error
		part, err = writer.store.UploadPart(writer.ctx, writer.uploadID, writer.nextPart, bytes.NewReader(data), int64(len(data)), digest)
		if err != nil {
			return err
		}
	}
	part.Number = writer.nextPart
	part.Size = int64(len(data))
	part.SHA256 = digest
	writer.parts = append(writer.parts, part)
	writer.buffer.Reset()
	return nil
}

func (mirror *Mirror) Download(ctx context.Context, checkpointID string) (model.CheckpointManifest, error) {
	if mirror == nil {
		return model.CheckpointManifest{}, ErrMirrorDisabled
	}
	if checkpointID == "" {
		return model.CheckpointManifest{}, errors.New("checkpoint id is required")
	}
	manifestKey := checkpointManifestKey(checkpointID)
	manifestHead, err := mirror.store.Head(ctx, manifestKey)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	if manifestHead.Size < 0 || manifestHead.Size > maxEncryptedManifestBytes {
		return model.CheckpointManifest{}, errors.New("mirrored checkpoint manifest exceeds size limit")
	}
	var manifestBuffer boundedBuffer
	manifestBuffer.limit = maxEncryptedManifestBytes
	manifestBuffer.buffer.Grow(int(manifestHead.Size))
	manifestInfo, err := mirror.store.Get(ctx, manifestKey, &manifestBuffer)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	encodedManifest := manifestBuffer.buffer.Bytes()
	manifestDigest := sha256Bytes(encodedManifest)
	if err := verifyMirroredObject(manifestInfo, int64(len(encodedManifest)), manifestDigest); err != nil {
		return model.CheckpointManifest{}, fmt.Errorf("verify downloaded checkpoint manifest: %w", err)
	}
	if err := verifyMirroredObject(manifestHead, int64(len(encodedManifest)), manifestDigest); err != nil {
		return model.CheckpointManifest{}, fmt.Errorf("verify checkpoint manifest metadata: %w", err)
	}
	manifest, err := mirror.repository.DecodeEncryptedManifest(checkpointID, encodedManifest)
	if err != nil {
		return model.CheckpointManifest{}, err
	}
	for _, ref := range uniqueChunkRefs(manifest.Assets) {
		missing, err := mirror.chunks.Missing([]model.ChunkRef{ref})
		if err != nil {
			return model.CheckpointManifest{}, err
		}
		if len(missing) == 0 {
			if _, err := mirror.chunks.ImportChunkResult(ctx, manifest.Workload.ID, ref, bytes.NewReader(nil)); err != nil {
				return model.CheckpointManifest{}, fmt.Errorf("verify existing mirrored chunk %s: %w", ref.Address, err)
			}
			continue
		}
		reader, writer := io.Pipe()
		downloadDone := make(chan error, 1)
		go func(reference model.ChunkRef) {
			_, err := mirror.store.Get(ctx, chunkObjectKey(reference), writer)
			_ = writer.CloseWithError(err)
			downloadDone <- err
		}(ref)
		_, importErr := mirror.chunks.ImportChunkResult(ctx, manifest.Workload.ID, ref, reader)
		_ = reader.CloseWithError(importErr)
		downloadErr := <-downloadDone
		if importErr != nil {
			return model.CheckpointManifest{}, fmt.Errorf("import mirrored chunk %s: %w", ref.Address, importErr)
		}
		if downloadErr != nil {
			return model.CheckpointManifest{}, fmt.Errorf("download mirrored chunk %s: %w", ref.Address, downloadErr)
		}
	}
	if err := mirror.repository.Save(manifest); err != nil {
		return model.CheckpointManifest{}, err
	}
	return manifest, nil
}

func (mirror *Mirror) CleanupUploads(ctx context.Context, before time.Time) (int, error) {
	if mirror == nil {
		return 0, nil
	}
	removed := 0
	for _, prefix := range []string{"chunks/", "checkpoints/"} {
		count, err := mirror.store.CleanupMultipart(ctx, prefix, before)
		if err != nil {
			return removed, fmt.Errorf("cleanup multipart uploads under %s: %w", prefix, err)
		}
		removed += count
	}
	return removed, nil
}

func uniqueChunkRefs(assets []model.AssetManifest) []model.ChunkRef {
	seen := make(map[string]bool)
	refs := make([]model.ChunkRef, 0)
	for _, asset := range assets {
		for _, ref := range asset.Chunks {
			key := fmt.Sprintf("%d/%s", ref.KeyVersion, ref.Address)
			if !seen[key] {
				seen[key] = true
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

func chunkObjectKey(ref model.ChunkRef) string {
	return fmt.Sprintf("chunks/v%d/%s", ref.KeyVersion, ref.Address)
}

func checkpointManifestKey(checkpointID string) string {
	return "checkpoints/" + checkpointID + "/manifest.enc.json"
}

func sha256Bytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func verifyMirroredObject(info objectstore.ObjectInfo, size int64, digest string) error {
	if info.Size != size || !strings.EqualFold(strings.TrimSpace(info.SHA256), digest) {
		return objectstore.ErrIntegrity
	}
	return nil
}

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int64
}

func (writer *boundedBuffer) Write(value []byte) (int, error) {
	remaining := writer.limit - int64(writer.buffer.Len())
	if remaining <= 0 {
		return 0, errors.New("mirrored checkpoint manifest exceeds size limit")
	}
	if int64(len(value)) > remaining {
		written, _ := writer.buffer.Write(value[:remaining])
		return written, errors.New("mirrored checkpoint manifest exceeds size limit")
	}
	return writer.buffer.Write(value)
}
