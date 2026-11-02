package agentclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"crypto/tls"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/model"
)

func TestClientReturnsDecodedValues(t *testing.T) {
	client := testClient(func(request *http.Request) (*http.Response, error) {
		var value any
		switch request.URL.Path {
		case "/v1/health":
			value = map[string]any{"status": "ok", "machine_id": "machine-test"}
		case "/v1/workloads":
			value = []model.Workload{{Spec: model.WorkloadSpec{ID: "workload-test", Name: "test"}, Status: model.WorkloadRunning}}
		case "/v1/workloads/workload-test":
			value = model.Workload{Spec: model.WorkloadSpec{ID: "workload-test", Name: "test"}, Status: model.WorkloadRunning}
		case "/v1/checkpoints/checkpoint-test/mirror":
			if request.Method != http.MethodPost {
				return nil, fmt.Errorf("unexpected mirror method %s", request.Method)
			}
			value = checkpoint.MirrorResult{CheckpointID: "checkpoint-test", Objects: 3, Bytes: 4096}
		default:
			return nil, fmt.Errorf("unexpected request path %s", request.URL.Path)
		}
		return testResponse(http.StatusOK, value), nil
	})
	ctx := context.Background()
	health, err := client.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health["machine_id"] != "machine-test" {
		t.Fatalf("health response was not returned: %#v", health)
	}
	workloads, err := client.Workloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workloads) != 1 || workloads[0].Spec.ID != "workload-test" {
		t.Fatalf("workload list was not returned: %#v", workloads)
	}
	workload, err := client.Workload(ctx, "workload-test")
	if err != nil {
		t.Fatal(err)
	}
	if workload.Status != model.WorkloadRunning {
		t.Fatalf("workload response was not returned: %#v", workload)
	}
	mirrored, err := client.MirrorCheckpoint(ctx, "checkpoint-test")
	if err != nil {
		t.Fatal(err)
	}
	if mirrored.CheckpointID != "checkpoint-test" || mirrored.Objects != 3 || mirrored.Bytes != 4096 {
		t.Fatalf("mirror response was not returned: %#v", mirrored)
	}
}

func TestHTTPSClientUsesConfiguredMTLS(t *testing.T) {
	directory := t.TempDir()
	caCert, caKey := testCertificateAuthority(t, directory)
	serverCert, serverKey := testLeafCertificate(t, directory, caCert, caKey, "agent.example", true)
	clientCert, clientKey := testLeafCertificate(t, directory, caCert, caKey, "control.example", false)
	serverTLS, err := tls.LoadX509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(caCert)
	if err != nil {
		t.Fatal(err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		t.Fatal("append client CA")
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]string{"status": "ok"})
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverTLS}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs}
	server.StartTLS()
	defer server.Close()
	client, err := NewWithTLS(server.URL, time.Second, TLSOptions{CertificateFile: clientCert, PrivateKeyFile: clientKey, PeerCAFile: caCert, ServerName: "agent.example"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("mTLS request failed: %v", err)
	}
	if result["status"] != "ok" {
		t.Fatalf("unexpected health response: %#v", result)
	}
}

func TestHTTPSClientRequiresMTLSConfiguration(t *testing.T) {
	if _, err := NewWithTLS("https://agent.example:8443", time.Second, TLSOptions{}); err == nil || !strings.Contains(err.Error(), "client certificate") {
		t.Fatalf("missing HTTPS mTLS configuration returned %v", err)
	}
}

func TestClientDecodesAPIErrors(t *testing.T) {
	client := testClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(http.StatusNotFound, model.ErrorResponse{Code: "WORKLOAD_NOT_FOUND", Message: "missing workload"}), nil
	})
	_, err := client.Workload(context.Background(), "missing")
	var apiError *APIError
	if !errors.As(err, &apiError) {
		t.Fatalf("expected APIError, got %T: %v", err, err)
	}
	if apiError.Status != http.StatusNotFound || apiError.Code != "WORKLOAD_NOT_FOUND" {
		t.Fatalf("unexpected API error: %#v", apiError)
	}
}

func TestAgentHintNamesSocketFailures(t *testing.T) {
	dial := func(err error) error {
		return &url.Error{Op: "Get", URL: "http://shift-agent/v1/doctor", Err: &net.OpError{Op: "dial", Net: "unix", Err: &os.SyscallError{Syscall: "connect", Err: err}}}
	}
	if hint := agentHint(dial(syscall.ENOENT)); !strings.Contains(hint, "shift-agent running") {
		t.Fatalf("missing socket must hint at the service, got %q", hint)
	}
	if hint := agentHint(dial(syscall.ECONNREFUSED)); !strings.Contains(hint, "restart the agent") {
		t.Fatalf("refused socket must hint at a restart, got %q", hint)
	}
	if hint := agentHint(dial(syscall.EACCES)); !strings.Contains(hint, "shift' group") {
		t.Fatalf("denied socket must hint at the group, got %q", hint)
	}
	tcp := &url.Error{Op: "Get", URL: "http://shift-agent/v1/doctor", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}}
	if hint := agentHint(tcp); hint != "" {
		t.Fatalf("non-unix dial must carry no hint, got %q", hint)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func testClient(roundTrip roundTripFunc) *Client {
	return &Client{baseURL: "http://shift-agent", http: &http.Client{Transport: roundTrip, Timeout: time.Second}}
}

func testResponse(status int, value any) *http.Response {
	var body []byte
	if value != nil {
		body, _ = json.Marshal(value)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d", status),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func testCertificateAuthority(t *testing.T, directory string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: newSerial(t), Subject: pkix.Name{CommonName: "shift test CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(directory, "ca.pem"), filepath.Join(directory, "ca-key.pem")
	writePEM(t, certPath, "CERTIFICATE", der)
	writePEM(t, keyPath, "EC PRIVATE KEY", mustMarshalECKey(t, key))
	return certPath, keyPath
}

func testLeafCertificate(t *testing.T, directory, caCert, caKey, name string, server bool) (string, string) {
	t.Helper()
	caKeyBytes, err := os.ReadFile(caKey)
	if err != nil {
		t.Fatal(err)
	}
	caBlock, _ := pem.Decode(caKeyBytes)
	parentKey, err := x509.ParseECPrivateKey(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	caBytes, err := os.ReadFile(caCert)
	if err != nil {
		t.Fatal(err)
	}
	caBlock, _ = pem.Decode(caBytes)
	parent, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: newSerial(t), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if server {
		certificate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(directory, name+".pem"), filepath.Join(directory, name+"-key.pem")
	writePEM(t, certPath, "CERTIFICATE", der)
	writePEM(t, keyPath, "EC PRIVATE KEY", mustMarshalECKey(t, key))
	return certPath, keyPath
}

func newSerial(t *testing.T) *big.Int {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	return serial
}

func mustMarshalECKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	encoded, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func writePEM(t *testing.T, path, kind string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: data}), 0o600); err != nil {
		t.Fatal(err)
	}
}
