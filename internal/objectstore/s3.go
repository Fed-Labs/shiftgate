package objectstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type S3Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Prefix          string
	StateDir        string
	ForcePathStyle  bool
	HTTPClient      *http.Client
}

type S3 struct {
	endpoint       *url.URL
	region         string
	bucket         string
	accessKeyID    string
	secretKey      string
	sessionToken   string
	prefix         string
	stateDir       string
	forcePathStyle bool
	client         *http.Client
	clock          func() time.Time
	mu             sync.Mutex
}

type s3UploadState struct {
	Upload MultipartUpload `json:"upload"`
	Parts  []PartInfo      `json:"parts,omitempty"`
}

func OpenS3(configuration S3Config) (*S3, error) {
	if strings.TrimSpace(configuration.Endpoint) == "" || strings.TrimSpace(configuration.Bucket) == "" || strings.TrimSpace(configuration.Region) == "" {
		return nil, errors.New("S3 endpoint, bucket, and region are required")
	}
	endpoint, err := url.Parse(strings.TrimRight(configuration.Endpoint, "/"))
	if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.Host == "" || endpoint.RawQuery != "" || endpoint.User != nil {
		return nil, errors.New("S3 endpoint must be an http(s) URL without credentials or query parameters")
	}
	if err := ValidateBucket(configuration.Bucket); err != nil {
		return nil, err
	}
	// A state directory is optional: a read-only client (the control plane's
	// listing broker) needs no multipart bookkeeping, and its upload-state
	// methods fail with ErrNoStateDirectory instead of touching the disk.
	if configuration.StateDir != "" {
		if !filepath.IsAbs(configuration.StateDir) {
			return nil, errors.New("S3 state directory must be an absolute path")
		}
		if err := os.MkdirAll(filepath.Join(configuration.StateDir, "multipart"), 0o700); err != nil {
			return nil, fmt.Errorf("create S3 state directory: %w", err)
		}
	}
	client := configuration.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 0}
	}
	return &S3{
		endpoint: endpoint, region: configuration.Region, bucket: configuration.Bucket,
		accessKeyID: configuration.AccessKeyID, secretKey: configuration.SecretAccessKey,
		sessionToken: configuration.SessionToken, prefix: strings.Trim(configuration.Prefix, "/"),
		stateDir: configuration.StateDir, forcePathStyle: configuration.ForcePathStyle,
		client: client, clock: time.Now,
	}, nil
}

func (s *S3) Put(ctx context.Context, key string, reader io.Reader, size int64, expectedSHA256 string) (ObjectInfo, error) {
	key, err := s.fullKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if size < 0 || size > maxObjectSize {
		return ObjectInfo{}, errors.New("object size must be between zero and 5 TiB")
	}
	expected, err := expectedHash(expectedSHA256)
	if err != nil {
		return ObjectInfo{}, err
	}
	if existing, headErr := s.headFull(ctx, key); headErr == nil {
		if existing.Size == size && (expected == "" || existing.SHA256 == expected) {
			return s.externalInfo(existing), nil
		}
		return ObjectInfo{}, ErrConflict
	} else if !errors.Is(headErr, ErrNotFound) {
		return ObjectInfo{}, headErr
	}
	headers := make(http.Header)
	headers.Set("Content-Length", strconv.FormatInt(size, 10))
	headers.Set("If-None-Match", "*")
	if expected != "" {
		headers.Set("X-Amz-Meta-Sha256", expected)
	}
	hashing := &hashingReader{reader: reader, hasher: sha256.New()}
	response, err := s.doKey(ctx, http.MethodPut, key, nil, headers, hashing, size)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			if existing, headErr := s.Head(ctx, key); headErr == nil && existing.Size == size && (expected == "" || existing.SHA256 == expected) {
				return existing, nil
			}
		}
		return ObjectInfo{}, err
	}
	_ = response.Body.Close()
	if hashing.n != size || (expected != "" && hex.EncodeToString(hashing.hasher.Sum(nil)) != expected) {
		return ObjectInfo{}, ErrIntegrity
	}
	info, err := s.headFull(ctx, key)
	return s.externalInfo(info), err
}

func (s *S3) Get(ctx context.Context, key string, writer io.Writer) (ObjectInfo, error) {
	key, err := s.fullKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	response, err := s.doKey(ctx, http.MethodGet, key, nil, nil, nil, 0)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer func() { _ = response.Body.Close() }()
	expected := strings.ToLower(strings.TrimSpace(response.Header.Get("X-Amz-Meta-Sha256")))
	if expected != "" {
		if expected, err = expectedHash(expected); err != nil {
			return ObjectInfo{}, ErrIntegrity
		}
	}
	hasher := sha256.New()
	written, err := copyWithContext(ctx, io.MultiWriter(writer, hasher), response.Body)
	if err != nil {
		return ObjectInfo{}, err
	}
	if response.ContentLength >= 0 && written != response.ContentLength {
		return ObjectInfo{}, ErrIntegrity
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if expected != "" && digest != expected {
		return ObjectInfo{}, ErrIntegrity
	}
	return s.externalInfo(ObjectInfo{Key: key, Size: written, SHA256: expectedOrDigest(expected, digest), ETag: trimQuotes(response.Header.Get("ETag")), LastModified: parseHTTPTime(response.Header.Get("Last-Modified"))}), nil
}

func (s *S3) Head(ctx context.Context, key string) (ObjectInfo, error) {
	key, err := s.fullKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	info, err := s.headFull(ctx, key)
	return s.externalInfo(info), err
}

func (s *S3) Delete(ctx context.Context, key string) error {
	key, err := s.fullKey(key)
	if err != nil {
		return err
	}
	response, err := s.doKey(ctx, http.MethodDelete, key, nil, nil, nil, 0)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	return nil
}

func (s *S3) InitiateMultipart(ctx context.Context, key string, totalSize int64, expectedSHA256 string) (MultipartUpload, error) {
	key, err := s.fullKey(key)
	if err != nil {
		return MultipartUpload{}, err
	}
	if totalSize < 0 || totalSize > maxObjectSize {
		return MultipartUpload{}, errors.New("multipart total size must be between zero and 5 TiB")
	}
	expected, err := expectedHash(expectedSHA256)
	if err != nil {
		return MultipartUpload{}, err
	}
	query := url.Values{}
	query.Set("uploads", "")
	headers := make(http.Header)
	if expected != "" {
		headers.Set("X-Amz-Meta-Sha256", expected)
	}
	response, err := s.doKey(ctx, http.MethodPost, key, query, headers, nil, 0)
	if err != nil {
		return MultipartUpload{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var result initiateMultipartResult
	if err := xml.NewDecoder(response.Body).Decode(&result); err != nil || strings.TrimSpace(result.UploadID) == "" {
		return MultipartUpload{}, fmt.Errorf("%w: invalid initiate response", ErrRemote)
	}
	upload := MultipartUpload{UploadID: strings.TrimSpace(result.UploadID), Key: key, TotalSize: totalSize, ExpectedSHA256: expected, InitiatedAt: time.Now().UTC()}
	if err := s.saveUploadState(s3UploadState{Upload: upload}); err != nil {
		_ = s.abortRemote(context.Background(), upload)
		return MultipartUpload{}, err
	}
	upload.Key = s.externalKey(upload.Key)
	return upload, nil
}

func (s *S3) UploadPart(ctx context.Context, uploadID string, number int, reader io.Reader, size int64, expectedSHA256 string) (PartInfo, error) {
	if number < 1 || number > 10000 || size < 0 || size > maxObjectSize || validateUploadID(uploadID) != nil {
		return PartInfo{}, ErrInvalidPart
	}
	state, err := s.loadUploadState(uploadID)
	if err != nil {
		return PartInfo{}, err
	}
	expected, err := expectedHash(expectedSHA256)
	if err != nil {
		return PartInfo{}, err
	}
	query := url.Values{}
	query.Set("partNumber", strconv.Itoa(number))
	query.Set("uploadId", uploadID)
	headers := make(http.Header)
	headers.Set("Content-Length", strconv.FormatInt(size, 10))
	hashing := &hashingReader{reader: reader, hasher: sha256.New()}
	response, err := s.doKey(ctx, http.MethodPut, state.Upload.Key, query, headers, hashing, size)
	if err != nil {
		return PartInfo{}, err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	part := PartInfo{Number: number, Size: size, SHA256: expected, ETag: trimQuotes(response.Header.Get("ETag"))}
	if part.ETag == "" {
		part.ETag = part.SHA256
	}
	if hashing.n != size {
		return PartInfo{}, ErrIntegrity
	}
	digest := hex.EncodeToString(hashing.hasher.Sum(nil))
	if expected != "" && digest != expected {
		return PartInfo{}, ErrIntegrity
	}
	if part.SHA256 == "" {
		part.SHA256 = digest
	}
	if err := s.recordPartState(uploadID, part); err != nil {
		return PartInfo{}, err
	}
	return part, nil
}

func (s *S3) ListParts(ctx context.Context, uploadID string) ([]PartInfo, error) {
	if validateUploadID(uploadID) != nil {
		return nil, ErrInvalidUpload
	}
	state, err := s.loadUploadState(uploadID)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("uploadId", uploadID)
	response, err := s.doKey(ctx, http.MethodGet, state.Upload.Key, query, nil, nil, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	var result listPartsResult
	if err := xml.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("%w: invalid list-parts response", ErrRemote)
	}
	parts := make([]PartInfo, 0, len(result.Parts))
	for _, part := range result.Parts {
		parts = append(parts, PartInfo{Number: part.Number, Size: part.Size, ETag: trimQuotes(part.ETag), SHA256: statePartDigest(state.Parts, part.Number)})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	return parts, nil
}

func (s *S3) CompleteMultipart(ctx context.Context, uploadID string, parts []PartInfo) (ObjectInfo, error) {
	if validateUploadID(uploadID) != nil {
		return ObjectInfo{}, ErrInvalidUpload
	}
	state, err := s.loadUploadState(uploadID)
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
	if existing, headErr := s.headFull(ctx, state.Upload.Key); headErr == nil {
		if state.Upload.ExpectedSHA256 != "" && existing.SHA256 == state.Upload.ExpectedSHA256 && existing.Size == state.Upload.TotalSize {
			_ = s.removeUploadState(uploadID)
			return s.externalInfo(existing), nil
		}
		return ObjectInfo{}, ErrConflict
	}
	var body bytes.Buffer
	_, _ = body.WriteString("<CompleteMultipartUpload>")
	for _, part := range parts {
		_, _ = fmt.Fprintf(&body, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", part.Number, xmlEscape(part.ETag))
	}
	_, _ = body.WriteString("</CompleteMultipartUpload>")
	query := url.Values{}
	query.Set("uploadId", uploadID)
	headers := make(http.Header)
	headers.Set("Content-Type", "application/xml")
	headers.Set("Content-Length", strconv.Itoa(body.Len()))
	response, err := s.doKey(ctx, http.MethodPost, state.Upload.Key, query, headers, bytes.NewReader(body.Bytes()), int64(body.Len()))
	if err != nil {
		return ObjectInfo{}, err
	}
	_ = response.Body.Close()
	info, err := s.headFull(ctx, state.Upload.Key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if info.Size != state.Upload.TotalSize || (state.Upload.ExpectedSHA256 != "" && info.SHA256 != state.Upload.ExpectedSHA256) {
		return ObjectInfo{}, ErrIntegrity
	}
	if err := s.removeUploadState(uploadID); err != nil {
		return ObjectInfo{}, err
	}
	return s.externalInfo(info), nil
}

func (s *S3) AbortMultipart(ctx context.Context, uploadID string) error {
	if validateUploadID(uploadID) != nil {
		return ErrInvalidUpload
	}
	state, err := s.loadUploadState(uploadID)
	if err != nil {
		return err
	}
	if err := s.abortRemote(ctx, state.Upload); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return s.removeUploadState(uploadID)
}

func (s *S3) ListMultipart(ctx context.Context, prefix string) ([]MultipartUpload, error) {
	fullPrefix := s.prefix
	if prefix != "" {
		if fullPrefix != "" {
			fullPrefix += "/"
		}
		fullPrefix += strings.Trim(prefix, "/")
	}
	var result []MultipartUpload
	keyMarker, uploadMarker := "", ""
	for {
		query := url.Values{}
		query.Set("uploads", "")
		if fullPrefix != "" {
			query.Set("prefix", fullPrefix)
		}
		if keyMarker != "" {
			query.Set("key-marker", keyMarker)
		}
		if uploadMarker != "" {
			query.Set("upload-id-marker", uploadMarker)
		}
		response, err := s.doBucket(ctx, http.MethodGet, query, nil, nil, 0)
		if err != nil {
			return nil, err
		}
		var page listUploadsResult
		decodeErr := xml.NewDecoder(response.Body).Decode(&page)
		_ = response.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("%w: invalid multipart-list response", ErrRemote)
		}
		for _, upload := range page.Uploads {
			state, stateErr := s.loadUploadState(upload.UploadID)
			value := MultipartUpload{UploadID: upload.UploadID, Key: upload.Key, InitiatedAt: upload.Initiated}
			if stateErr == nil {
				value.TotalSize = state.Upload.TotalSize
				value.ExpectedSHA256 = state.Upload.ExpectedSHA256
			}
			value.Key = s.externalKey(value.Key)
			result = append(result, value)
		}
		if !page.IsTruncated {
			break
		}
		keyMarker, uploadMarker = page.NextKeyMarker, page.NextUploadIDMarker
		if keyMarker == "" && uploadMarker == "" {
			break
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].InitiatedAt.Before(result[j].InitiatedAt) })
	return result, nil
}

func (s *S3) CleanupMultipart(ctx context.Context, prefix string, before time.Time) (int, error) {
	uploads, err := s.ListMultipart(ctx, prefix)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, upload := range uploads {
		if upload.InitiatedAt.Before(before) {
			if err := s.AbortMultipart(ctx, upload.UploadID); err != nil && !errors.Is(err, ErrNotFound) {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

// ListPrefix walks every stored object under prefix, reporting each with the
// store's own prefix stripped from the key, and stops early when visit
// returns an error. It is the metering primitive — the control plane counts
// actual stored bytes with it instead of trusting agent-reported totals —
// so it is deliberately not part of the Store interface the agent's mirror
// uses.
func (s *S3) ListPrefix(ctx context.Context, prefix string, visit func(ObjectInfo) error) error {
	fullPrefix := s.prefix
	if prefix != "" {
		if fullPrefix != "" {
			fullPrefix += "/"
		}
		fullPrefix += strings.Trim(prefix, "/")
	}
	token := ""
	for {
		query := url.Values{"list-type": {"2"}}
		if fullPrefix != "" {
			query.Set("prefix", fullPrefix)
		}
		if token != "" {
			query.Set("continuation-token", token)
		}
		response, err := s.doBucket(ctx, http.MethodGet, query, nil, nil, 0)
		if err != nil {
			return err
		}
		var page listObjectsResult
		decodeErr := xml.NewDecoder(response.Body).Decode(&page)
		_ = response.Body.Close()
		if decodeErr != nil {
			return fmt.Errorf("%w: invalid object-list response", ErrRemote)
		}
		for _, object := range page.Contents {
			if err := visit(ObjectInfo{Key: s.externalKey(object.Key), Size: object.Size, LastModified: object.LastModified}); err != nil {
				return err
			}
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return nil
		}
		token = page.NextContinuationToken
	}
}

func (s *S3) doKey(ctx context.Context, method, key string, query url.Values, headers http.Header, body io.Reader, size int64) (*http.Response, error) {
	return s.doURL(ctx, method, s.objectURL(key, query), headers, body, size)
}

func (s *S3) headFull(ctx context.Context, key string) (ObjectInfo, error) {
	response, err := s.doKey(ctx, http.MethodHead, key, nil, nil, nil, 0)
	if err != nil {
		return ObjectInfo{}, err
	}
	_ = response.Body.Close()
	expected := strings.ToLower(strings.TrimSpace(response.Header.Get("X-Amz-Meta-Sha256")))
	if expected != "" {
		if expected, err = expectedHash(expected); err != nil {
			return ObjectInfo{}, ErrIntegrity
		}
	}
	return ObjectInfo{Key: key, Size: response.ContentLength, SHA256: expected, ETag: trimQuotes(response.Header.Get("ETag")), LastModified: parseHTTPTime(response.Header.Get("Last-Modified"))}, nil
}

func (s *S3) doBucket(ctx context.Context, method string, query url.Values, headers http.Header, body io.Reader, size int64) (*http.Response, error) {
	return s.doURL(ctx, method, s.bucketURL(query), headers, body, size)
}

func (s *S3) doURL(ctx context.Context, method string, target *url.URL, headers http.Header, body io.Reader, size int64) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	if size >= 0 {
		request.ContentLength = size
	}
	s.sign(request)
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRemote, err)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, nil
	}
	err = parseS3Error(response)
	_ = response.Body.Close()
	return nil, err
}

func (s *S3) sign(request *http.Request) {
	s.signService(request, "s3", "")
}

// signService signs a request for an AWS service ("s3" or "sts"); both use
// the same SigV4 derivation with the service name in the scope. It snapshots
// the credentials under the mutex so a concurrent SetCredentials rotation
// cannot produce a signature mixed from two credential generations.
// payloadHash is the hex SHA-256 the server should verify the body against;
// the empty string means UNSIGNED-PAYLOAD, which S3 accepts but STS does not.
func (s *S3) signService(request *http.Request, service, payloadHash string) {
	if payloadHash == "" {
		payloadHash = "UNSIGNED-PAYLOAD"
	}
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	s.mu.Lock()
	accessKeyID, secretKey, sessionToken := s.accessKeyID, s.secretKey, s.sessionToken
	region := s.region
	s.mu.Unlock()
	if accessKeyID == "" || secretKey == "" {
		return
	}
	now := s.clock().UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	request.Header.Set("X-Amz-Date", amzDate)
	if sessionToken != "" {
		request.Header.Set("X-Amz-Security-Token", sessionToken)
	}
	canonicalHeaders, signedHeaders := canonicalHeaders(request)
	canonicalRequest := strings.Join([]string{request.Method, canonicalURI(request.URL), canonicalQuery(request.URL.Query()), canonicalHeaders, signedHeaders, payloadHash}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hashString(canonicalRequest)
	signingKey := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+secretKey), []byte(date)), []byte(region)), []byte(service)), []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKeyID+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}

// SetCredentials rotates the SigV4 credentials on an open store without
// rebuilding it, so multipart upload state and the state directory survive a
// refresh. The control plane's issued credentials expire; hosted-mode agents
// call this with each refresh.
func (s *S3) SetCredentials(accessKeyID, secretAccessKey, sessionToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accessKeyID = accessKeyID
	s.secretKey = secretAccessKey
	s.sessionToken = sessionToken
}

func canonicalHeaders(request *http.Request) (canonical, signed string) {
	values := map[string][]string{"host": {request.URL.Host}}
	for name, entries := range request.Header {
		lower := strings.ToLower(strings.TrimSpace(name))
		if lower == "authorization" || lower == "user-agent" {
			continue
		}
		values[lower] = append(values[lower], entries...)
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	for _, name := range names {
		entries := values[name]
		for index := range entries {
			entries[index] = strings.Join(strings.Fields(entries[index]), " ")
		}
		builder.WriteString(name)
		builder.WriteByte(':')
		builder.WriteString(strings.Join(entries, ","))
		builder.WriteByte('\n')
	}
	return builder.String(), strings.Join(names, ";")
}

func (s *S3) objectURL(key string, query url.Values) *url.URL {
	value := *s.endpoint
	pathKey := strings.TrimLeft(key, "/")
	if s.forcePathStyle || strings.Contains(value.Hostname(), "localhost") || strings.HasPrefix(value.Hostname(), "127.") {
		value.Path = strings.TrimRight(value.Path, "/") + "/" + s.bucket + "/" + pathKey
	} else {
		value.Host = s.bucket + "." + value.Host
		value.Path = strings.TrimRight(value.Path, "/") + "/" + pathKey
	}
	value.RawQuery = canonicalQuery(query)
	return &value
}

func (s *S3) bucketURL(query url.Values) *url.URL {
	value := *s.endpoint
	if s.forcePathStyle || strings.Contains(value.Hostname(), "localhost") || strings.HasPrefix(value.Hostname(), "127.") {
		value.Path = strings.TrimRight(value.Path, "/") + "/" + s.bucket
	} else {
		value.Host = s.bucket + "." + value.Host
	}
	value.RawQuery = canonicalQuery(query)
	return &value
}

func (s *S3) fullKey(key string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	if s.prefix == "" {
		return key, nil
	}
	return s.prefix + "/" + key, nil
}

func (s *S3) externalKey(key string) string {
	if s.prefix != "" && strings.HasPrefix(key, s.prefix+"/") {
		return strings.TrimPrefix(key, s.prefix+"/")
	}
	return key
}

func (s *S3) externalInfo(info ObjectInfo) ObjectInfo {
	info.Key = s.externalKey(info.Key)
	return info
}

func (s *S3) abortRemote(ctx context.Context, upload MultipartUpload) error {
	query := url.Values{}
	query.Set("uploadId", upload.UploadID)
	response, err := s.doKey(ctx, http.MethodDelete, upload.Key, query, nil, nil, 0)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	return nil
}

func (s *S3) statePath(uploadID string) string {
	return filepath.Join(s.stateDir, "multipart", uploadID+".json")
}

func (s *S3) saveUploadState(state s3UploadState) error {
	if s.stateDir == "" {
		return ErrNoStateDirectory
	}
	return writeJSONAtomic(s.statePath(state.Upload.UploadID), state)
}

func (s *S3) loadUploadState(uploadID string) (s3UploadState, error) {
	if s.stateDir == "" {
		return s3UploadState{}, ErrNoStateDirectory
	}
	if validateUploadID(uploadID) != nil {
		return s3UploadState{}, ErrInvalidUpload
	}
	file, err := os.Open(s.statePath(uploadID))
	if errors.Is(err, os.ErrNotExist) {
		return s3UploadState{}, ErrNotFound
	}
	if err != nil {
		return s3UploadState{}, err
	}
	defer func() { _ = file.Close() }()
	var state s3UploadState
	if err := json.NewDecoder(file).Decode(&state); err != nil || state.Upload.UploadID != uploadID {
		return s3UploadState{}, ErrInvalidUpload
	}
	return state, nil
}

func (s *S3) recordPartState(uploadID string, part PartInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadUploadState(uploadID)
	if err != nil {
		return err
	}
	for index, existing := range state.Parts {
		if existing.Number == part.Number {
			if existing.Size != part.Size || (existing.SHA256 != "" && part.SHA256 != "" && existing.SHA256 != part.SHA256) {
				return ErrConflict
			}
			state.Parts[index] = part
			return s.saveUploadState(state)
		}
	}
	state.Parts = append(state.Parts, part)
	sort.Slice(state.Parts, func(i, j int) bool { return state.Parts[i].Number < state.Parts[j].Number })
	return s.saveUploadState(state)
}

func (s *S3) removeUploadState(uploadID string) error {
	if s.stateDir == "" {
		return ErrNoStateDirectory
	}
	if err := os.Remove(s.statePath(uploadID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type initiateMultipartResult struct {
	UploadID string `xml:"UploadId"`
}

type listPartsResult struct {
	Parts []s3Part `xml:"Part"`
}

type s3Part struct {
	Number int    `xml:"PartNumber"`
	ETag   string `xml:"ETag"`
	Size   int64  `xml:"Size"`
}

type listUploadsResult struct {
	Uploads            []s3Upload `xml:"Upload"`
	IsTruncated        bool       `xml:"IsTruncated"`
	NextKeyMarker      string     `xml:"NextKeyMarker"`
	NextUploadIDMarker string     `xml:"NextUploadIdMarker"`
}

type listObjectsResult struct {
	Contents              []s3Object `xml:"Contents"`
	IsTruncated           bool       `xml:"IsTruncated"`
	NextContinuationToken string     `xml:"NextContinuationToken"`
}

type s3Object struct {
	Key          string    `xml:"Key"`
	Size         int64     `xml:"Size"`
	LastModified time.Time `xml:"LastModified"`
}

type s3Upload struct {
	Key       string    `xml:"Key"`
	UploadID  string    `xml:"UploadId"`
	Initiated time.Time `xml:"Initiated"`
}

func parseS3Error(response *http.Response) error {
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusNoContent {
		return ErrNotFound
	}
	var result struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result)
	if response.StatusCode == http.StatusPreconditionFailed || result.Code == "PreconditionFailed" {
		return ErrConflict
	}
	if result.Code == "NoSuchKey" || result.Code == "NoSuchUpload" || result.Code == "NoSuchBucket" {
		return ErrNotFound
	}
	if result.Message == "" {
		result.Message = response.Status
	}
	return fmt.Errorf("%w: %s: %s", ErrRemote, result.Code, result.Message)
}

// ValidateBucket reports whether name is a usable S3 bucket name: 3–63
// characters, no leading or trailing dot, no path separators.
func ValidateBucket(name string) error {
	if len(name) < 3 || len(name) > 63 || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, "/") {
		return errors.New("invalid S3 bucket name")
	}
	return nil
}

func validateUploadID(id string) error {
	if id == "" || len(id) > 256 || strings.ContainsRune(id, 0) || strings.ContainsAny(id, `/\\`) {
		return ErrInvalidUpload
	}
	return nil
}

func statePartDigest(parts []PartInfo, number int) string {
	for _, part := range parts {
		if part.Number == number {
			return part.SHA256
		}
	}
	return ""
}

func expectedOrDigest(expected, digest string) string {
	if expected != "" {
		return expected
	}
	return digest
}

func trimQuotes(value string) string { return strings.Trim(value, "\"") }

func parseHTTPTime(value string) time.Time {
	parsed, _ := http.ParseTime(value)
	return parsed
}

func xmlEscape(value string) string {
	var buffer bytes.Buffer
	_ = xml.EscapeText(&buffer, []byte(value))
	return buffer.String()
}

func hashString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func hmacSHA256(key, value []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(value)
	return mac.Sum(nil)
}

type hashingReader struct {
	reader io.Reader
	hasher hash.Hash
	n      int64
}

func (reader *hashingReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	if count > 0 {
		written, writeErr := reader.hasher.Write(buffer[:count])
		if writeErr != nil {
			return written, writeErr
		}
		if written != count {
			return written, io.ErrShortWrite
		}
		reader.n += int64(count)
	}
	return count, err
}

func canonicalURI(value *url.URL) string {
	path := value.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

func canonicalQuery(values url.Values) string {
	type pair struct{ key, value string }
	pairs := make([]pair, 0)
	for key, entries := range values {
		if len(entries) == 0 {
			entries = []string{""}
		}
		for _, value := range entries {
			pairs = append(pairs, pair{awsEncode(key), awsEncode(value)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key == pairs[j].key {
			return pairs[i].value < pairs[j].value
		}
		return pairs[i].key < pairs[j].key
	})
	valuesOut := make([]string, len(pairs))
	for index, value := range pairs {
		valuesOut[index] = value.key + "=" + value.value
	}
	return strings.Join(valuesOut, "&")
}

func awsEncode(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' || character == '~' {
			builder.WriteByte(character)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(hexDigits[character>>4])
		builder.WriteByte(hexDigits[character&0x0f])
	}
	return builder.String()
}

var _ Store = (*S3)(nil)
