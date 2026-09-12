package objectstore

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// STSCredentials is a temporary credential set minted by an STS-compatible
// service (MinIO or AWS) through AssumeRole.
type STSCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// AssumeRole exchanges the parent credential for a temporary credential
// scoped by policy, valid for ttl. It posts the STS AssumeRole action with a
// SigV4 signature for the "sts" service — the same derivation the S3 client
// uses with the service name in the signing scope — so no new signing code or
// dependency is introduced. endpoint is the STS endpoint (for MinIO, the same
// address as S3).
func AssumeRole(ctx context.Context, endpoint, region, accessKeyID, secretAccessKey, policy string, ttl time.Duration, client *http.Client) (STSCredentials, error) {
	if strings.TrimSpace(endpoint) == "" || strings.TrimSpace(region) == "" {
		return STSCredentials{}, errors.New("STS endpoint and region are required")
	}
	parsed, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.RawQuery != "" || parsed.User != nil {
		return STSCredentials{}, errors.New("STS endpoint must be an http(s) URL without credentials or query parameters")
	}
	if accessKeyID == "" || secretAccessKey == "" {
		return STSCredentials{}, errors.New("STS parent credentials are required")
	}
	if policy == "" {
		return STSCredentials{}, errors.New("STS policy is required")
	}
	if ttl < time.Minute || ttl > 7*24*time.Hour {
		return STSCredentials{}, fmt.Errorf("STS session duration must be between one minute and seven days, got %s", ttl)
	}
	seconds := strconv.Itoa(int(ttl / time.Second))
	form := url.Values{}
	form.Set("Action", "AssumeRole")
	form.Set("Version", "2011-06-15")
	form.Set("Policy", policy)
	form.Set("DurationSeconds", seconds)
	target := *parsed
	target.Path = strings.TrimRight(parsed.Path, "/") + "/"
	body := form.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), strings.NewReader(body))
	if err != nil {
		return STSCredentials{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.ContentLength = int64(len(body))
	// The S3 signer is reused with service "sts": it reads the parent
	// credentials from the store built below and signs with the form body's
	// SHA-256 — STS verifies the payload hash, unlike S3, which accepts
	// UNSIGNED-PAYLOAD.
	signer := &S3{
		accessKeyID: accessKeyID, secretKey: secretAccessKey,
		region: region, clock: time.Now,
	}
	signer.signService(request, "sts", hashString(body))
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return STSCredentials{}, fmt.Errorf("%w: %w", ErrRemote, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		// The error body carries the service's own code and message (for
		// example "Unsupported action AssumeRole"), which is the difference
		// between a signature problem and a deployment missing the STS API.
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		detail := strings.TrimSpace(errorCodeFromXML(body))
		if detail == "" {
			detail = strings.TrimSpace(string(body))
			if len(detail) > 300 {
				detail = detail[:300] + "…"
			}
		}
		if detail != "" {
			return STSCredentials{}, fmt.Errorf("%w: STS returned %s: %s", ErrRemote, response.Status, detail)
		}
		return STSCredentials{}, fmt.Errorf("%w: STS returned %s", ErrRemote, response.Status)
	}
	var result assumeRoleResult
	if err := xml.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return STSCredentials{}, fmt.Errorf("%w: invalid AssumeRole response", ErrRemote)
	}
	credentials := result.AssumedRoleUser.Credentials
	if credentials.AccessKeyID == "" || credentials.SecretAccessKey == "" || credentials.SessionToken == "" {
		return STSCredentials{}, fmt.Errorf("%w: AssumeRole response is missing credentials", ErrRemote)
	}
	if credentials.Expiration.IsZero() {
		return STSCredentials{}, fmt.Errorf("%w: AssumeRole response is missing the expiration", ErrRemote)
	}
	return STSCredentials(credentials), nil
}

type assumeRoleResult struct {
	AssumedRoleUser assumeRoleUser `xml:"AssumeRoleResult"`
}

type assumeRoleUser struct {
	Credentials stsCredentialsXML `xml:"Credentials"`
}

type stsCredentialsXML struct {
	AccessKeyID     string    `xml:"AccessKeyId"`
	SecretAccessKey string    `xml:"SecretAccessKey"`
	SessionToken    string    `xml:"SessionToken"`
	Expiration      time.Time `xml:"Expiration"`
}

// errorCodeFromXML pulls <Code> and <Message> out of an STS error document so
// failures name their cause without dumping the whole body.
func errorCodeFromXML(body []byte) string {
	var document struct {
		Code    string `xml:"Error>Code"`
		Message string `xml:"Error>Message"`
	}
	if err := xml.Unmarshal(body, &document); err != nil {
		return ""
	}
	if document.Code == "" && document.Message == "" {
		return ""
	}
	if document.Message == "" {
		return document.Code
	}
	if document.Code == "" {
		return document.Message
	}
	return document.Code + ": " + document.Message
}
