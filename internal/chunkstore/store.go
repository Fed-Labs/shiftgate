package chunkstore

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/securestore"
)

const (
	magic             = "SHFTCHK1"
	fileFormatVersion = uint16(1)
	maxEncodedChunk   = 96 << 20
)

type Store struct {
	root      string
	chunkSize int
	keys      *securestore.Manager
	mu        sync.RWMutex
}

type PutResult struct {
	Asset             model.AssetManifest
	DeduplicatedBytes int64
}

func Open(root string, chunkSize int, keys *securestore.Manager) (*Store, error) {
	if keys == nil {
		return nil, errors.New("secure key manager is required")
	}
	if chunkSize < 64<<10 || chunkSize > 64<<20 {
		return nil, errors.New("chunk size must be between 64 KiB and 64 MiB")
	}
	if err := os.MkdirAll(filepath.Join(root, "chunks"), 0o700); err != nil {
		return nil, err
	}
	return &Store{root: root, chunkSize: chunkSize, keys: keys}, nil
}

func (s *Store) ActiveKeyVersion(workloadID string) (uint32, error) {
	version, _, err := s.keys.GetOrCreateWorkloadKey(workloadID)
	return version, err
}

func (s *Store) PutAsset(ctx context.Context, workloadID, name, mediaType string, reader io.Reader, required bool, restoreOrder int) (PutResult, error) {
	if workloadID == "" || name == "" {
		return PutResult{}, errors.New("workload id and asset name are required")
	}
	keyVersion, dataKey, err := s.keys.GetOrCreateWorkloadKey(workloadID)
	if err != nil {
		return PutResult{}, err
	}
	asset := model.AssetManifest{Name: name, MediaType: mediaType, Required: required, RestoreOrder: restoreOrder}
	wholeHash := sha256.New()
	buffer := make([]byte, s.chunkSize)
	var deduplicated int64
	for sequence := 0; ; sequence++ {
		if err := ctx.Err(); err != nil {
			return PutResult{}, err
		}
		count, readErr := io.ReadFull(reader, buffer)
		if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
			return PutResult{}, fmt.Errorf("read asset %s: %w", name, readErr)
		}
		if count == 0 {
			break
		}
		plaintext := buffer[:count]
		_, _ = wholeHash.Write(plaintext)
		ref, existed, err := s.putChunk(workloadID, keyVersion, dataKey, sequence, plaintext)
		if err != nil {
			return PutResult{}, fmt.Errorf("store asset %s chunk %d: %w", name, sequence, err)
		}
		asset.Chunks = append(asset.Chunks, ref)
		asset.PlainSize += ref.PlainSize
		asset.StoredSize += ref.StoredSize
		if existed {
			deduplicated += ref.PlainSize
		}
		if errors.Is(readErr, io.ErrUnexpectedEOF) || errors.Is(readErr, io.EOF) {
			break
		}
	}
	asset.SHA256 = hex.EncodeToString(wholeHash.Sum(nil))
	return PutResult{Asset: asset, DeduplicatedBytes: deduplicated}, nil
}

func (s *Store) RestoreAsset(ctx context.Context, workloadID string, asset model.AssetManifest, writer io.Writer) error {
	ordered := append([]model.ChunkRef(nil), asset.Chunks...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })
	wholeHash := sha256.New()
	written := int64(0)
	for expected, ref := range ordered {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ref.Sequence != expected {
			return fmt.Errorf("asset %s has non-contiguous chunk sequence", asset.Name)
		}
		plaintext, err := s.readChunk(workloadID, ref)
		if err != nil {
			return fmt.Errorf("restore asset %s chunk %d: %w", asset.Name, ref.Sequence, err)
		}
		if _, err := writer.Write(plaintext); err != nil {
			return fmt.Errorf("write restored asset %s: %w", asset.Name, err)
		}
		_, _ = wholeHash.Write(plaintext)
		written += int64(len(plaintext))
	}
	if written != asset.PlainSize {
		return fmt.Errorf("asset %s size mismatch: got %d want %d", asset.Name, written, asset.PlainSize)
	}
	if digest := hex.EncodeToString(wholeHash.Sum(nil)); digest != asset.SHA256 {
		return fmt.Errorf("asset %s digest mismatch", asset.Name)
	}
	return nil
}

func (s *Store) Missing(refs []model.ChunkRef) ([]model.ChunkRef, error) {
	missing := make([]model.ChunkRef, 0)
	for _, ref := range refs {
		path, err := s.chunkPath(ref)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			missing = append(missing, ref)
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Size() != ref.StoredSize {
			missing = append(missing, ref)
		}
	}
	return missing, nil
}

func (s *Store) ExportChunk(ref model.ChunkRef, writer io.Writer) error {
	path, err := s.chunkPath(ref)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(writer, hasher), io.LimitReader(file, maxEncodedChunk+1))
	if err != nil {
		return err
	}
	if written != ref.StoredSize {
		return fmt.Errorf("chunk %s stored size mismatch", ref.Address)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != ref.CipherSHA256 {
		return fmt.Errorf("chunk %s ciphertext digest mismatch", ref.Address)
	}
	return nil
}

func (s *Store) ImportChunk(ctx context.Context, workloadID string, ref model.ChunkRef, reader io.Reader) error {
	_, err := s.ImportChunkResult(ctx, workloadID, ref, reader)
	return err
}

// ImportChunkResult reports whether this call installed a new chunk. A retry
// of an already accepted upload is successful but does not consume quota a
// second time.
func (s *Store) ImportChunkResult(ctx context.Context, workloadID string, ref model.ChunkRef, reader io.Reader) (bool, error) {
	path, err := s.chunkPath(ref)
	if err != nil {
		return false, err
	}
	if info, err := os.Stat(path); err == nil && info.Size() == ref.StoredSize {
		_, verifyErr := s.readChunk(workloadID, ref)
		return false, verifyErr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".import-*")
	if err != nil {
		return false, err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return false, err
	}
	hasher := sha256.New()
	limited := &io.LimitedReader{R: reader, N: ref.StoredSize + 1}
	written, err := copyContext(ctx, io.MultiWriter(temporary, hasher), limited)
	if err != nil {
		return false, err
	}
	if written != ref.StoredSize || limited.N == 0 {
		return false, fmt.Errorf("chunk %s upload size mismatch", ref.Address)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != ref.CipherSHA256 {
		return false, fmt.Errorf("chunk %s upload digest mismatch", ref.Address)
	}
	if err := temporary.Sync(); err != nil {
		return false, err
	}
	if err := temporary.Close(); err != nil {
		return false, err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			if _, verifyErr := s.readChunk(workloadID, ref); verifyErr != nil {
				return false, verifyErr
			}
			return false, nil
		}
		return false, err
	}
	committed = true
	if _, err := s.readChunk(workloadID, ref); err != nil {
		_ = os.Remove(path)
		return false, err
	}
	return true, nil
}

func (s *Store) ValidateAsset(ctx context.Context, workloadID string, asset model.AssetManifest) error {
	return s.RestoreAsset(ctx, workloadID, asset, io.Discard)
}

func (s *Store) putChunk(workloadID string, keyVersion uint32, dataKey []byte, sequence int, plaintext []byte) (model.ChunkRef, bool, error) {
	address, err := securestore.Address(dataKey, workloadID, plaintext)
	if err != nil {
		return model.ChunkRef{}, false, err
	}
	plainDigest := sha256.Sum256(plaintext)
	compressed, err := compress(plaintext)
	if err != nil {
		return model.ChunkRef{}, false, err
	}
	aad := chunkAAD(workloadID, address, keyVersion, int64(len(plaintext)))
	nonce, ciphertext, err := securestore.Encrypt(dataKey, workloadID, "checkpoint-chunk", compressed, aad)
	if err != nil {
		return model.ChunkRef{}, false, err
	}
	encoded, err := encodeChunk(keyVersion, int64(len(plaintext)), nonce, ciphertext)
	if err != nil {
		return model.ChunkRef{}, false, err
	}
	cipherDigest := sha256.Sum256(encoded)
	ref := model.ChunkRef{
		Address:      address,
		KeyVersion:   keyVersion,
		PlainSize:    int64(len(plaintext)),
		StoredSize:   int64(len(encoded)),
		CipherSHA256: hex.EncodeToString(cipherDigest[:]),
		PlainSHA256:  hex.EncodeToString(plainDigest[:]),
		Sequence:     sequence,
		Compression:  "gzip",
		Encryption:   "AES-256-GCM",
	}
	path, err := s.chunkPath(ref)
	if err != nil {
		return model.ChunkRef{}, false, err
	}
	if info, err := os.Stat(path); err == nil && info.Size() == int64(len(encoded)) {
		existing, readErr := os.ReadFile(path)
		if readErr == nil {
			existingDigest := sha256.Sum256(existing)
			existingRef := ref
			existingRef.StoredSize = int64(len(existing))
			existingRef.CipherSHA256 = hex.EncodeToString(existingDigest[:])
			existingPlaintext, verifyErr := s.readChunk(workloadID, existingRef)
			if verifyErr == nil && bytes.Equal(existingPlaintext, plaintext) {
				return existingRef, true, nil
			}
		}
		return model.ChunkRef{}, false, fmt.Errorf("content address collision or corrupt existing chunk %s", address)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return model.ChunkRef{}, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return model.ChunkRef{}, false, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".chunk-*")
	if err != nil {
		return model.ChunkRef{}, false, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return model.ChunkRef{}, false, err
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return model.ChunkRef{}, false, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return model.ChunkRef{}, false, err
	}
	if err := temporary.Close(); err != nil {
		return model.ChunkRef{}, false, err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			existingRef, verifyErr := s.existingRef(workloadID, ref, plaintext)
			if verifyErr != nil {
				return model.ChunkRef{}, false, verifyErr
			}
			return existingRef, true, nil
		}
		return model.ChunkRef{}, false, err
	}
	return ref, false, nil
}

// existingRef resolves manifest metadata for a chunk that won a concurrent
// content-addressed write race. Encryption uses a fresh nonce, so the losing
// writer's locally computed ciphertext digest cannot be reused.
func (s *Store) existingRef(workloadID string, reference model.ChunkRef, expected []byte) (model.ChunkRef, error) {
	path, err := s.chunkPath(reference)
	if err != nil {
		return model.ChunkRef{}, err
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return model.ChunkRef{}, err
	}
	resolved := reference
	resolved.StoredSize = int64(len(encoded))
	digest := sha256.Sum256(encoded)
	resolved.CipherSHA256 = hex.EncodeToString(digest[:])
	plaintext, err := s.readChunk(workloadID, resolved)
	if err != nil {
		return model.ChunkRef{}, fmt.Errorf("verify concurrent chunk %s: %w", reference.Address, err)
	}
	if !bytes.Equal(plaintext, expected) {
		return model.ChunkRef{}, fmt.Errorf("content address collision or corrupt existing chunk %s", reference.Address)
	}
	return resolved, nil
}

func (s *Store) readChunk(workloadID string, ref model.ChunkRef) ([]byte, error) {
	path, err := s.chunkPath(ref)
	if err != nil {
		return nil, err
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) != ref.StoredSize || len(encoded) > maxEncodedChunk {
		return nil, errors.New("invalid encoded chunk size")
	}
	cipherDigest := sha256.Sum256(encoded)
	if hex.EncodeToString(cipherDigest[:]) != ref.CipherSHA256 {
		return nil, errors.New("ciphertext digest mismatch")
	}
	keyVersion, plainSize, nonce, ciphertext, err := decodeChunk(encoded)
	if err != nil {
		return nil, err
	}
	if keyVersion != ref.KeyVersion || plainSize != ref.PlainSize {
		return nil, errors.New("chunk header does not match manifest")
	}
	dataKey, err := s.keys.WorkloadKey(workloadID, keyVersion)
	if err != nil {
		return nil, err
	}
	aad := chunkAAD(workloadID, ref.Address, keyVersion, plainSize)
	compressed, err := securestore.Decrypt(dataKey, workloadID, "checkpoint-chunk", nonce, ciphertext, aad)
	if err != nil {
		return nil, err
	}
	plaintext, err := decompress(compressed, plainSize)
	if err != nil {
		return nil, err
	}
	plainDigest := sha256.Sum256(plaintext)
	if hex.EncodeToString(plainDigest[:]) != ref.PlainSHA256 {
		return nil, errors.New("plaintext digest mismatch")
	}
	address, err := securestore.Address(dataKey, workloadID, plaintext)
	if err != nil || address != ref.Address {
		return nil, errors.New("content address mismatch")
	}
	return plaintext, nil
}

func (s *Store) chunkPath(ref model.ChunkRef) (string, error) {
	if len(ref.Address) != 64 || strings.Trim(ref.Address, "0123456789abcdef") != "" {
		return "", errors.New("invalid chunk address")
	}
	return filepath.Join(s.root, "chunks", fmt.Sprintf("v%d", ref.KeyVersion), ref.Address[:2], ref.Address+".chunk"), nil
}

func chunkAAD(workloadID, address string, keyVersion uint32, plainSize int64) []byte {
	return []byte(fmt.Sprintf("shift-checkpoint-chunk-v1\n%s\n%s\n%d\n%d", workloadID, address, keyVersion, plainSize))
}

func encodeChunk(keyVersion uint32, plainSize int64, nonce, ciphertext []byte) ([]byte, error) {
	if len(nonce) > 255 || len(ciphertext) > maxEncodedChunk {
		return nil, errors.New("chunk encoding limit exceeded")
	}
	buffer := bytes.NewBuffer(make([]byte, 0, 8+2+4+8+2+8+len(nonce)+len(ciphertext)))
	_, _ = buffer.WriteString(magic)
	_ = binary.Write(buffer, binary.BigEndian, fileFormatVersion)
	_ = binary.Write(buffer, binary.BigEndian, keyVersion)
	_ = binary.Write(buffer, binary.BigEndian, plainSize)
	_ = binary.Write(buffer, binary.BigEndian, uint16(len(nonce)))
	_ = binary.Write(buffer, binary.BigEndian, uint64(len(ciphertext)))
	_, _ = buffer.Write(nonce)
	_, _ = buffer.Write(ciphertext)
	return buffer.Bytes(), nil
}

func decodeChunk(encoded []byte) (uint32, int64, []byte, []byte, error) {
	reader := bytes.NewReader(encoded)
	magicBytes := make([]byte, len(magic))
	if _, err := io.ReadFull(reader, magicBytes); err != nil || string(magicBytes) != magic {
		return 0, 0, nil, nil, errors.New("invalid chunk magic")
	}
	var format uint16
	var keyVersion uint32
	var plainSize int64
	var nonceLength uint16
	var ciphertextLength uint64
	if err := binary.Read(reader, binary.BigEndian, &format); err != nil || format != fileFormatVersion {
		return 0, 0, nil, nil, errors.New("unsupported chunk format")
	}
	if err := binary.Read(reader, binary.BigEndian, &keyVersion); err != nil {
		return 0, 0, nil, nil, err
	}
	if err := binary.Read(reader, binary.BigEndian, &plainSize); err != nil {
		return 0, 0, nil, nil, err
	}
	if err := binary.Read(reader, binary.BigEndian, &nonceLength); err != nil {
		return 0, 0, nil, nil, err
	}
	if err := binary.Read(reader, binary.BigEndian, &ciphertextLength); err != nil {
		return 0, 0, nil, nil, err
	}
	if nonceLength == 0 || ciphertextLength > maxEncodedChunk || uint64(reader.Len()) != uint64(nonceLength)+ciphertextLength {
		return 0, 0, nil, nil, errors.New("invalid chunk lengths")
	}
	nonce := make([]byte, nonceLength)
	ciphertext := make([]byte, ciphertextLength)
	if _, err := io.ReadFull(reader, nonce); err != nil {
		return 0, 0, nil, nil, err
	}
	if _, err := io.ReadFull(reader, ciphertext); err != nil {
		return 0, 0, nil, nil, err
	}
	return keyVersion, plainSize, nonce, ciphertext, nil
}

func compress(plaintext []byte) ([]byte, error) {
	var output bytes.Buffer
	writer, err := gzip.NewWriterLevel(&output, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(plaintext); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func decompress(compressed []byte, expectedSize int64) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	limited := io.LimitReader(reader, expectedSize+1)
	plaintext, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(plaintext)) != expectedSize {
		return nil, errors.New("decompressed chunk size mismatch")
	}
	return plaintext, nil
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
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
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func hashWriter() hash.Hash {
	return sha256.New()
}
