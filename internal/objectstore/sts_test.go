package objectstore

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeSTS struct {
	policy        string
	duration      string
	sawSTS        bool
	authorization string
	contentSHA256 string
	body          []byte
	// expiration is what the fake issues; tests set it to exercise the
	// returned-lifetime checks. Zero means one hour from now — the lifetime
	// the callers below request.
	expiration time.Time
}

func (server *fakeSTS) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeS3Error(writer, http.StatusMethodNotAllowed, "MethodNotAllowed")
		return
	}
	body, _ := io.ReadAll(request.Body)
	if err := parseFormBytes(request, body); err != nil {
		writeS3Error(writer, http.StatusBadRequest, "InvalidForm")
		return
	}
	if request.Form.Get("Action") != "AssumeRole" || request.Form.Get("Version") != "2011-06-15" {
		writeS3Error(writer, http.StatusBadRequest, "InvalidAction")
		return
	}
	server.policy = request.Form.Get("Policy")
	server.duration = request.Form.Get("DurationSeconds")
	server.authorization = request.Header.Get("Authorization")
	server.contentSHA256 = request.Header.Get("X-Amz-Content-Sha256")
	server.body = body
	// The STS signing scope names the "sts" service, not "s3".
	server.sawSTS = strings.Contains(server.authorization, "/sts/aws4_request")
	expiration := server.expiration
	if expiration.IsZero() {
		expiration = time.Now().UTC().Add(time.Hour)
	}
	writer.Header().Set("Content-Type", "application/xml")
	_, _ = writer.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleResult>
    <Credentials>
      <AccessKeyId>STSROLED</AccessKeyId>
      <SecretAccessKey>STSROLESECRET</SecretAccessKey>
      <SessionToken>STSROLETOKEN</SessionToken>
      <Expiration>` + expiration.UTC().Format(time.RFC3339) + `</Expiration>
    </Credentials>
  </AssumeRoleResult>
</AssumeRoleResponse>`))
}

func TestAssumeRoleSignsForServiceSTSAndParsesCredentials(t *testing.T) {
	server := &fakeSTS{}
	httptestServer := httptest.NewServer(server)
	defer httptestServer.Close()
	before := time.Now().UTC()
	credentials, err := AssumeRole(context.Background(), httptestServer.URL, "test-1", "parent", "parent-secret", `{"Version":"2012-10-17"}`, time.Hour, httptestServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	if credentials.AccessKeyID != "STSROLED" || credentials.SecretAccessKey != "STSROLESECRET" || credentials.SessionToken != "STSROLETOKEN" {
		t.Fatalf("unexpected credentials: %#v", credentials)
	}
	if credentials.Expiration.Before(before.Add(59*time.Minute)) || credentials.Expiration.After(time.Now().UTC().Add(61*time.Minute)) {
		t.Fatalf("unexpected expiration: %v", credentials.Expiration)
	}
	if server.policy != `{"Version":"2012-10-17"}` {
		t.Fatalf("policy was not posted: %q", server.policy)
	}
	if server.duration != "3600" {
		t.Fatalf("DurationSeconds was %q, want 3600", server.duration)
	}
	if !server.sawSTS {
		t.Fatalf("request was not signed for the sts service: %q", server.authorization)
	}
	if !strings.HasPrefix(server.authorization, "AWS4-HMAC-SHA256 Credential=parent/") {
		t.Fatalf("parent credential was not used: %q", server.authorization)
	}
	// STS verifies the payload hash (unlike S3, which accepts
	// UNSIGNED-PAYLOAD): the header must be the hex SHA-256 of the body.
	if server.contentSHA256 != hashString(string(server.body)) {
		t.Fatalf("STS payload hash was %q, want the SHA-256 of the posted form", server.contentSHA256)
	}
}

// parseFormBytes parses a form body the server has already drained, mirroring
// http.Request.ParseForm for an in-memory body.
func parseFormBytes(request *http.Request, body []byte) error {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return err
	}
	if request.Form == nil {
		request.Form = url.Values{}
	}
	for key, entries := range values {
		for _, entry := range entries {
			request.Form.Add(key, entry)
		}
	}
	return nil
}
