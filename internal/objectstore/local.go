package objectstore

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	localMetaSuffix = ".shift-object.json"
	localTempPrefix = ".shift-tmp-"
	maxObjectSize   = int64(5 << 40)
)

type Local struct {
	root string
	mu   sync.RWMutex
}

type localUploadMeta struct {
	MultipartUpload
	Parts []PartInfo `json:"parts,omitempty"`
}

func OpenLocal(root string) (*Local, error) {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) {
		return nil, errors.New("local object-store root must be an absolute path")
	}
	for _, directory := range []string{filepath.Join(root, "objects"), filepath.Join(root, "multipart")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create object-store directory: %w", err)
		}
	}
	return &Local{root: root}, nil
}

func (s *Local) Put(ctx context.Context, key string, reader io.Reader, size int64, expectedSHA256 string) (ObjectInfo, error) {
	if size < 0 || size > maxObjectSize {
		return ObjectInfo{}, errors.New("object size must be between zero and 5 TiB")
	}
	objectPath, err := s.objectPath(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(objectPath), 0o700); err != nil {
		return ObjectInfo{}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(objectPath), localTempPrefix)
	if err != nil {
		return ObjectInfo{}, err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	info, err := copyHashed(ctx, temporary, reader, size, expectedSHA256)
	closeErr := temporary.Close()
	if err != nil {
		return ObjectInfo{}, err
	}
	if closeErr != nil {
		return ObjectInfo{}, closeErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, err := s.headLocked(key, objectPath); err == nil {
		if existing.Size != info.Size || (existing.SHA256 != "" && existing.SHA256 != info.SHA256) {
			return ObjectInfo{}, ErrConflict
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return ObjectInfo{}, err
	}
	if err := os.Rename(temporaryName, objectPath); err != nil {
		return ObjectInfo{}, fmt.Errorf("install object: %w", err)
	}
	info.Key = key
	info.LastModified = time.Now().UTC()
	if err := writeJSONAtomic(s.metaPath(objectPath), info); err != nil {
		_ = os.Remove(objectPath)
		return ObjectInfo{}, err
	}
	return info, nil
}

func (s *Local) Get(ctx context.Context, key string, writer io.Writer) (ObjectInfo, error) {
	objectPath, err := s.objectPath(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	s.mu.RLock()
	info, err := s.headLocked(key, objectPath)
	s.mu.RUnlock()
	if err != nil {
		return ObjectInfo{}, err
	}
	file, err := os.Open(objectPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, err
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	written, copyErr := copyWithContext(ctx, io.MultiWriter(writer, hasher), file)
	if copyErr != nil {
		return ObjectInfo{}, copyErr
	}
	if written != info.Size || (info.SHA256 != "" && hex.EncodeToString(hasher.Sum(nil)) != info.SHA256) {
		return ObjectInfo{}, ErrIntegrity
	}
	return info, nil
}

func (s *Local) Head(_ context.Context, key string) (ObjectInfo, error) {
	objectPath, err := s.objectPath(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.headLocked(key, objectPath)
}

func (s *Local) Delete(_ context.Context, key string) error {
	objectPath, err := s.objectPath(key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(objectPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	_ = os.Remove(s.metaPath(objectPath))
	return nil
}

func (s *Local) InitiateMultipart(_ context.Context, key string, totalSize int64, expectedSHA256 string) (MultipartUpload, error) {
	if _, err := s.objectPath(key); err != nil {
		return MultipartUpload{}, err
	}
	if totalSize < 0 || totalSize > maxObjectSize {
		return MultipartUpload{}, errors.New("multipart total size must be between zero and 5 TiB")
	}
	expected, err := expectedHash(expectedSHA256)
	if err != nil {
		return MultipartUpload{}, err
	}
	id, err := randomID()
	if err != nil {
		return MultipartUpload{}, err
	}
	upload := MultipartUpload{UploadID: id, Key: key, TotalSize: totalSize, ExpectedSHA256: expected, InitiatedAt: time.Now().UTC()}
	directory := filepath.Join(s.root, "multipart", id)
	if err := os.MkdirAll(filepath.Join(directory, "parts"), 0o700); err != nil {
		return MultipartUpload{}, err
	}
	if err := writeJSONAtomic(filepath.Join(directory, "meta.json"), localUploadMeta{MultipartUpload: upload}); err != nil {
		_ = os.RemoveAll(directory)
		return MultipartUpload{}, err
	}
	return upload, nil
}

func (s *Local) UploadPart(ctx context.Context, uploadID string, number int, reader io.Reader, size int64, expectedSHA256 string) (PartInfo, error) {
	if number < 1 || number > 10000 || size < 0 || size > maxObjectSize {
		return PartInfo{}, ErrInvalidPart
	}
	_, directory, err := s.loadUpload(uploadID)
	if err != nil {
		return PartInfo{}, err
	}
	temporary, err := os.CreateTemp(filepath.Join(directory, "parts"), localTempPrefix)
	if err != nil {
		return PartInfo{}, err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	object, err := copyHashed(ctx, temporary, reader, size, expectedSHA256)
	closeErr := temporary.Close()
	if err != nil {
		return PartInfo{}, err
	}
	if closeErr != nil {
		return PartInfo{}, closeErr
	}
	part := PartInfo{Number: number, Size: object.Size, SHA256: object.SHA256, ETag: object.SHA256}
	partPath := filepath.Join(directory, "parts", fmt.Sprintf("%06d.part", number))
	metaPath := filepath.Join(directory, "parts", fmt.Sprintf("%06d.json", number))
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, readErr := readPartMeta(metaPath); readErr == nil {
		if existing.Size != part.Size || existing.SHA256 != part.SHA256 {
			return PartInfo{}, ErrConflict
		}
		return existing, nil
	}
	if err := os.Rename(temporaryName, partPath); err != nil {
		return PartInfo{}, err
	}
	if err := writeJSONAtomic(metaPath, part); err != nil {
		_ = os.Remove(partPath)
		return PartInfo{}, err
	}
	return part, nil
}

func (s *Local) ListParts(_ context.Context, uploadID string) ([]PartInfo, error) {
	_, directory, err := s.loadUpload(uploadID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(directory, "parts"))
	if err != nil {
		return nil, err
	}
	parts := make([]PartInfo, 0)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		part, err := readPartMeta(filepath.Join(directory, "parts", entry.Name()))
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	return parts, nil
}

func (s *Local) CompleteMultipart(ctx context.Context, uploadID string, parts []PartInfo) (ObjectInfo, error) {
	upload, directory, err := s.loadUpload(uploadID)
	if err != nil {
		return ObjectInfo{}, err
	}
	if len(parts) == 0 {
		parts, err = s.ListParts(ctx, uploadID)
		if err != nil {
			return ObjectInfo{}, err
		}
	}
	if err := validateParts(parts); err != nil {
		return ObjectInfo{}, err
	}
	objectPath, err := s.objectPath(upload.Key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(objectPath), 0o700); err != nil {
		return ObjectInfo{}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(objectPath), localTempPrefix)
	if err != nil {
		return ObjectInfo{}, err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	hasher := sha256.New()
	var total int64
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			_ = temporary.Close()
			return ObjectInfo{}, err
		}
		partPath := filepath.Join(directory, "parts", fmt.Sprintf("%06d.part", part.Number))
		file, err := os.Open(partPath)
		if err != nil {
			_ = temporary.Close()
			return ObjectInfo{}, ErrInvalidPart
		}
		partHasher := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(temporary, hasher, partHasher), file)
		_ = file.Close()
		if copyErr != nil || written != part.Size || hex.EncodeToString(partHasher.Sum(nil)) != part.SHA256 {
			_ = temporary.Close()
			return ObjectInfo{}, ErrIntegrity
		}
		total += written
	}
	if upload.TotalSize >= 0 && total != upload.TotalSize {
		_ = temporary.Close()
		return ObjectInfo{}, fmt.Errorf("multipart size mismatch: got %d want %d", total, upload.TotalSize)
	}
	if upload.ExpectedSHA256 != "" && hex.EncodeToString(hasher.Sum(nil)) != upload.ExpectedSHA256 {
		_ = temporary.Close()
		return ObjectInfo{}, ErrIntegrity
	}
	if err := temporary.Close(); err != nil {
		return ObjectInfo{}, err
	}
	info := ObjectInfo{Key: upload.Key, Size: total, SHA256: hex.EncodeToString(hasher.Sum(nil)), LastModified: time.Now().UTC()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, headErr := s.headLocked(upload.Key, objectPath); headErr == nil {
		if existing.Size != info.Size || existing.SHA256 != info.SHA256 {
			return ObjectInfo{}, ErrConflict
		}
		_ = os.RemoveAll(directory)
		return existing, nil
	}
	if err := os.Rename(temporaryName, objectPath); err != nil {
		return ObjectInfo{}, err
	}
	if err := writeJSONAtomic(s.metaPath(objectPath), info); err != nil {
		_ = os.Remove(objectPath)
		return ObjectInfo{}, err
	}
	if err := os.RemoveAll(directory); err != nil {
		return ObjectInfo{}, err
	}
	return info, nil
}

func (s *Local) AbortMultipart(_ context.Context, uploadID string) error {
	_, directory, err := s.loadUpload(uploadID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	return nil
}

func (s *Local) ListMultipart(_ context.Context, prefix string) ([]MultipartUpload, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "multipart"))
	if err != nil {
		return nil, err
	}
	result := make([]MultipartUpload, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, _, err := s.loadUpload(entry.Name())
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if prefix == "" || strings.HasPrefix(meta.Key, prefix) {
			result = append(result, meta.MultipartUpload)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].InitiatedAt.Before(result[j].InitiatedAt) })
	return result, nil
}

func (s *Local) CleanupMultipart(_ context.Context, prefix string, before time.Time) (int, error) {
	uploads, err := s.ListMultipart(context.Background(), prefix)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, upload := range uploads {
		if upload.InitiatedAt.Before(before) {
			if err := s.AbortMultipart(context.Background(), upload.UploadID); err != nil && !errors.Is(err, ErrNotFound) {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

func (s *Local) objectPath(key string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	clean := filepath.FromSlash(key)
	path := filepath.Join(s.root, "objects", clean)
	base := filepath.Join(s.root, "objects")
	if path != base && !strings.HasPrefix(path, base+string(filepath.Separator)) {
		return "", ErrInvalidKey
	}
	return path, nil
}

func (s *Local) metaPath(objectPath string) string { return objectPath + localMetaSuffix }

func (s *Local) headLocked(key, objectPath string) (ObjectInfo, error) {
	metadata, err := os.Open(s.metaPath(objectPath))
	if err == nil {
		defer func() { _ = metadata.Close() }()
		var info ObjectInfo
		if decodeErr := json.NewDecoder(metadata).Decode(&info); decodeErr != nil {
			return ObjectInfo{}, ErrIntegrity
		}
		info.Key = key
		if stat, statErr := os.Stat(objectPath); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return ObjectInfo{}, ErrNotFound
			}
			return ObjectInfo{}, statErr
		} else if stat.Size() != info.Size {
			return ObjectInfo{}, ErrIntegrity
		}
		return info, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return ObjectInfo{}, err
	}
	if _, statErr := os.Stat(objectPath); errors.Is(statErr, os.ErrNotExist) {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{}, ErrIntegrity
}

func (s *Local) loadUpload(uploadID string) (localUploadMeta, string, error) {
	if err := validateID(uploadID); err != nil {
		return localUploadMeta{}, "", ErrInvalidUpload
	}
	directory := filepath.Join(s.root, "multipart", uploadID)
	file, err := os.Open(filepath.Join(directory, "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return localUploadMeta{}, "", ErrNotFound
	}
	if err != nil {
		return localUploadMeta{}, "", err
	}
	defer func() { _ = file.Close() }()
	var meta localUploadMeta
	if err := json.NewDecoder(file).Decode(&meta); err != nil || meta.UploadID != uploadID || meta.Key == "" {
		return localUploadMeta{}, "", ErrInvalidUpload
	}
	return meta, directory, nil
}

func copyHashed(ctx context.Context, destination io.Writer, source io.Reader, size int64, expected string) (ObjectInfo, error) {
	if size < 0 {
		return ObjectInfo{}, errors.New("size cannot be negative")
	}
	hasher := sha256.New()
	limited := io.LimitReader(source, size+1)
	written, err := copyWithContext(ctx, io.MultiWriter(destination, hasher), limited)
	if err != nil {
		return ObjectInfo{}, err
	}
	if written != size {
		return ObjectInfo{}, fmt.Errorf("object size mismatch: got %d want %d", written, size)
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	normalized, err := expectedHash(expected)
	if err != nil {
		return ObjectInfo{}, err
	}
	if normalized != "" && normalized != digest {
		return ObjectInfo{}, ErrIntegrity
	}
	return ObjectInfo{Size: written, SHA256: digest}, nil
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			written, writeErr := destination.Write(buffer[:count])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != count {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

func writeJSONAtomic(path string, value any) error {
	file, err := os.CreateTemp(filepath.Dir(path), localTempPrefix)
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	encoderErr := json.NewEncoder(file).Encode(value)
	closeErr := file.Close()
	if encoderErr != nil {
		return encoderErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}

func readPartMeta(path string) (PartInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return PartInfo{}, err
	}
	defer func() { _ = file.Close() }()
	var part PartInfo
	if err := json.NewDecoder(bufio.NewReader(file)).Decode(&part); err != nil {
		return PartInfo{}, ErrInvalidPart
	}
	return part, nil
}

func validateParts(parts []PartInfo) error {
	if len(parts) == 0 {
		return ErrInvalidPart
	}
	last := 0
	for _, part := range parts {
		if part.Number < 1 || part.Number <= last || part.Size < 0 || len(part.SHA256) != 64 {
			return ErrInvalidPart
		}
		last = part.Number
	}
	return nil
}

func validateKey(key string) error {
	if key == "" || len(key) > 1024 || strings.ContainsRune(key, 0) || strings.Contains(key, "\\") || strings.HasPrefix(key, "/") {
		return ErrInvalidKey
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return ErrInvalidKey
		}
	}
	return nil
}

func validateID(id string) error {
	if id == "" || len(id) > 128 || strings.ContainsRune(id, 0) || strings.ContainsAny(id, `/\\`) {
		return ErrInvalidUpload
	}
	return nil
}

func randomID() (string, error) {
	value := make([]byte, 18)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func expectedHash(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if len(value) != 64 {
		return "", errors.New("SHA-256 digest must contain 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", errors.New("SHA-256 digest must be hexadecimal")
	}
	return value, nil
}

var _ Store = (*Local)(nil)
