package objectstore

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestS3PutGetSignsAndVerifiesContent(t *testing.T) {
	serverState := newFakeS3()
	store, err := OpenS3(S3Config{
		Endpoint: "http://s3.test", Region: "test-1", Bucket: "shift-test", Prefix: "tenant-a",
		AccessKeyID: "access", SecretAccessKey: "secret", StateDir: t.TempDir(), ForcePathStyle: true,
		HTTPClient: &http.Client{Transport: handlerTransport{handler: serverState}},
	})
	if err != nil {
		t.Fatal(err)
	}
	store.clock = func() time.Time { return time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) }
	content := bytes.Repeat([]byte("ciphertext"), 1024)
	digest := sha256Hex(content)
	info, err := store.Put(context.Background(), "checkpoints/cp/chunk", bytes.NewReader(content), int64(len(content)), digest)
	if err != nil {
		t.Fatal(err)
	}
	if info.SHA256 != digest || info.Size != int64(len(content)) {
		t.Fatalf("unexpected S3 object info: %#v", info)
	}
	if info.Key != "checkpoints/cp/chunk" {
		t.Fatalf("S3 returned internal prefixed key %q", info.Key)
	}
	var output bytes.Buffer
	if _, err := store.Get(context.Background(), "checkpoints/cp/chunk", &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), content) {
		t.Fatal("S3 content did not round trip")
	}
	serverState.mu.Lock()
	defer serverState.mu.Unlock()
	if serverState.authorization == "" || !strings.HasPrefix(serverState.authorization, "AWS4-HMAC-SHA256 Credential=access/") {
		t.Fatalf("request was not SigV4 signed: %q", serverState.authorization)
	}
	if serverState.amzDate != "20260820T120000Z" {
		t.Fatalf("unexpected signing time %q", serverState.amzDate)
	}
	if _, exists := serverState.objects["tenant-a/checkpoints/cp/chunk"]; !exists {
		t.Fatalf("prefix was not applied: %#v", serverState.objects)
	}
}

func TestS3MultipartStateResumesAfterReopen(t *testing.T) {
	serverState := newFakeS3()
	stateDir := t.TempDir()
	configuration := S3Config{Endpoint: "http://s3.test", Region: "test-1", Bucket: "shift-test", StateDir: stateDir, ForcePathStyle: true, HTTPClient: &http.Client{Transport: handlerTransport{handler: serverState}}}
	store, err := OpenS3(configuration)
	if err != nil {
		t.Fatal(err)
	}
	first := bytes.Repeat([]byte("first"), 128<<10)
	second := bytes.Repeat([]byte("second"), 64<<10)
	whole := append(append([]byte(nil), first...), second...)
	upload, err := store.InitiateMultipart(context.Background(), "cp/state", int64(len(whole)), sha256Hex(whole))
	if err != nil {
		t.Fatal(err)
	}
	partOne, err := store.UploadPart(context.Background(), upload.UploadID, 1, bytes.NewReader(first), int64(len(first)), sha256Hex(first))
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenS3(configuration)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := reopened.ListParts(context.Background(), upload.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].Number != partOne.Number || parts[0].SHA256 != partOne.SHA256 {
		t.Fatalf("multipart state did not resume: %#v", parts)
	}
	partTwo, err := reopened.UploadPart(context.Background(), upload.UploadID, 2, bytes.NewReader(second), int64(len(second)), sha256Hex(second))
	if err != nil {
		t.Fatal(err)
	}
	info, err := reopened.CompleteMultipart(context.Background(), upload.UploadID, []PartInfo{partOne, partTwo})
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(whole)) || info.SHA256 != sha256Hex(whole) {
		t.Fatalf("unexpected multipart result: %#v", info)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "multipart", upload.UploadID+".json")); !os.IsNotExist(err) {
		t.Fatalf("completed upload state was not removed: %v", err)
	}
}

func TestCanonicalQueryAndAWSPercentEncoding(t *testing.T) {
	values := url.Values{"uploadId": {"id/+ value"}, "partNumber": {"10"}, "uploads": {""}}
	if got, want := canonicalQuery(values), "partNumber=10&uploadId=id%2F%2B%20value&uploads="; got != want {
		t.Fatalf("canonical query %q, want %q", got, want)
	}
}

type fakeS3 struct {
	mu            sync.Mutex
	objects       map[string]fakeObject
	uploads       map[string]*fakeUpload
	nextUpload    int
	authorization string
	amzDate       string
}

type handlerTransport struct {
	handler http.Handler
}

func (transport handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	transport.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

type fakeObject struct {
	content []byte
	digest  string
}

type fakeUpload struct {
	key    string
	digest string
	parts  map[int][]byte
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: make(map[string]fakeObject), uploads: make(map[string]*fakeUpload)}
}

func (server *fakeS3) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.authorization = request.Header.Get("Authorization")
	server.amzDate = request.Header.Get("X-Amz-Date")
	key := strings.TrimPrefix(request.URL.Path, "/shift-test/")
	uploadID := request.URL.Query().Get("uploadId")
	if request.Method == http.MethodPost && hasRawQueryKey(request.URL.RawQuery, "uploads") {
		server.nextUpload++
		uploadID = fmt.Sprintf("upload-%d", server.nextUpload)
		server.uploads[uploadID] = &fakeUpload{key: key, digest: request.Header.Get("X-Amz-Meta-Sha256"), parts: make(map[int][]byte)}
		writeXML(writer, http.StatusOK, struct {
			XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
			UploadID string   `xml:"UploadId"`
		}{UploadID: uploadID})
		return
	}
	if uploadID != "" {
		upload := server.uploads[uploadID]
		if upload == nil {
			writeS3Error(writer, http.StatusNotFound, "NoSuchUpload")
			return
		}
		switch request.Method {
		case http.MethodPut:
			number, _ := strconv.Atoi(request.URL.Query().Get("partNumber"))
			content, _ := io.ReadAll(request.Body)
			upload.parts[number] = content
			writer.Header().Set("ETag", fmt.Sprintf("\"etag-%d\"", number))
			writer.WriteHeader(http.StatusOK)
		case http.MethodGet:
			numbers := make([]int, 0, len(upload.parts))
			for number := range upload.parts {
				numbers = append(numbers, number)
			}
			sortInts(numbers)
			result := struct {
				XMLName xml.Name     `xml:"ListPartsResult"`
				Parts   []fakeS3Part `xml:"Part"`
			}{}
			for _, number := range numbers {
				result.Parts = append(result.Parts, fakeS3Part{Number: number, ETag: fmt.Sprintf("\"etag-%d\"", number), Size: int64(len(upload.parts[number]))})
			}
			writeXML(writer, http.StatusOK, result)
		case http.MethodPost:
			body, _ := io.ReadAll(request.Body)
			var completion struct {
				Parts []struct {
					Number int `xml:"PartNumber"`
				} `xml:"Part"`
			}
			_ = xml.Unmarshal(body, &completion)
			var content []byte
			for _, part := range completion.Parts {
				content = append(content, upload.parts[part.Number]...)
			}
			server.objects[upload.key] = fakeObject{content: content, digest: upload.digest}
			delete(server.uploads, uploadID)
			writeXML(writer, http.StatusOK, struct {
				XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
			}{})
		case http.MethodDelete:
			delete(server.uploads, uploadID)
			writer.WriteHeader(http.StatusNoContent)
		}
		return
	}

	object, exists := server.objects[key]
	switch request.Method {
	case http.MethodHead:
		if !exists {
			writeS3Error(writer, http.StatusNotFound, "NoSuchKey")
			return
		}
		writer.Header().Set("Content-Length", strconv.Itoa(len(object.content)))
		writer.Header().Set("X-Amz-Meta-Sha256", object.digest)
		writer.Header().Set("ETag", `"etag"`)
		writer.WriteHeader(http.StatusOK)
	case http.MethodPut:
		if exists && request.Header.Get("If-None-Match") == "*" {
			writeS3Error(writer, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		content, _ := io.ReadAll(request.Body)
		server.objects[key] = fakeObject{content: content, digest: request.Header.Get("X-Amz-Meta-Sha256")}
		writer.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if !exists {
			writeS3Error(writer, http.StatusNotFound, "NoSuchKey")
			return
		}
		writer.Header().Set("Content-Length", strconv.Itoa(len(object.content)))
		writer.Header().Set("X-Amz-Meta-Sha256", object.digest)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(object.content)
	case http.MethodDelete:
		delete(server.objects, key)
		writer.WriteHeader(http.StatusNoContent)
	}
}

type fakeS3Part struct {
	Number int    `xml:"PartNumber"`
	ETag   string `xml:"ETag"`
	Size   int64  `xml:"Size"`
}

func hasRawQueryKey(rawQuery, key string) bool {
	for _, part := range strings.Split(rawQuery, "&") {
		if part == key || strings.HasPrefix(part, key+"=") {
			return true
		}
	}
	return false
}

func writeXML(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/xml")
	writer.WriteHeader(status)
	_ = xml.NewEncoder(writer).Encode(value)
}

func writeS3Error(writer http.ResponseWriter, status int, code string) {
	writeXML(writer, status, struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: code, Message: code})
}

func sortInts(values []int) {
	for index := 1; index < len(values); index++ {
		for current := index; current > 0 && values[current] < values[current-1]; current-- {
			values[current], values[current-1] = values[current-1], values[current]
		}
	}
}
