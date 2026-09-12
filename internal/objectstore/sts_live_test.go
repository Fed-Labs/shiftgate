package objectstore

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveMinIOAssumeRole issues a real AssumeRole against a live MinIO
// (SHIFT_TEST_STS_ENDPOINT plus credentials) to verify the wire format end to
// end: SigV4 with service "sts" and the form body's SHA-256 as the payload
// hash — S3 accepts UNSIGNED-PAYLOAD, STS does not. Skipped without the
// environment so ordinary test runs need no MinIO.
func TestLiveMinIOAssumeRole(t *testing.T) {
	endpoint := os.Getenv("SHIFT_TEST_STS_ENDPOINT")
	accessKey := os.Getenv("SHIFT_TEST_STS_ACCESS_KEY")
	secretKey := os.Getenv("SHIFT_TEST_STS_SECRET_KEY")
	if endpoint == "" || accessKey == "" || secretKey == "" {
		t.Skip("set SHIFT_TEST_STS_ENDPOINT, SHIFT_TEST_STS_ACCESS_KEY, and SHIFT_TEST_STS_SECRET_KEY")
	}
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::shift-checkpoints/org/live/*"]}]}`
	credentials, err := AssumeRole(context.Background(), endpoint, "us-east-1", accessKey, secretKey, policy, 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("live AssumeRole failed: %v", err)
	}
	if credentials.AccessKeyID == "" || credentials.SecretAccessKey == "" || credentials.SessionToken == "" {
		t.Fatalf("live AssumeRole returned incomplete credentials: %+v", credentials)
	}
	if !credentials.Expiration.After(time.Now().UTC()) {
		t.Fatalf("live AssumeRole returned expired credentials: %v", credentials.Expiration)
	}
}

// TestLiveMinIOAssumeRoleWrongSigning documents the failure shape the payload
// hash fixed: a signature computed with UNSIGNED-PAYLOAD is rejected by MinIO
// STS, so the client must hash the form body.
func TestLiveMinIOAssumeRoleWrongSigning(t *testing.T) {
	endpoint := os.Getenv("SHIFT_TEST_STS_ENDPOINT")
	accessKey := os.Getenv("SHIFT_TEST_STS_ACCESS_KEY")
	secretKey := os.Getenv("SHIFT_TEST_STS_SECRET_KEY")
	if endpoint == "" || accessKey == "" || secretKey == "" {
		t.Skip("set SHIFT_TEST_STS_ENDPOINT, SHIFT_TEST_STS_ACCESS_KEY, and SHIFT_TEST_STS_SECRET_KEY")
	}
	form := url.Values{}
	form.Set("Action", "AssumeRole")
	form.Set("Version", "2011-06-15")
	form.Set("Policy", `{"Version":"2012-10-17"}`)
	form.Set("DurationSeconds", "900")
	body := form.Encode()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, strings.TrimRight(endpoint, "/")+"/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.ContentLength = int64(len(body))
	signer := &S3{accessKeyID: accessKey, secretKey: secretKey, region: "us-east-1", clock: time.Now}
	signer.signService(request, "sts", "")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
	response.Body.Close()
	if response.StatusCode == http.StatusOK {
		t.Fatal("MinIO accepted an UNSIGNED-PAYLOAD signature for STS; the payload-hash requirement documented here is stale")
	}
	if !strings.Contains(string(raw), "Signature") && !strings.Contains(string(raw), "signature") {
		t.Logf("note: rejection was not a signature mismatch: %s %s", response.Status, strings.TrimSpace(string(raw)))
	}
}
