package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"shift.dev/shift/internal/endpoint"
	"shift.dev/shift/internal/identity"
)

// ControlMessage binds proof of the registered machine key to the exact
// method/path/body. An organization API key alone cannot impersonate a host.
func ControlMessage(method, path, timestamp, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte("shift-relay-control-v1\n" + method + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + hex.EncodeToString(sum[:]))
}
func SignControl(req *http.Request, id *identity.Identity, body []byte) error {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	n := base64.RawURLEncoding.EncodeToString(nonce[:])
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("X-Shift-Machine-Key", base64.RawStdEncoding.EncodeToString([]byte(id.Machine.PublicKeyPEM)))
	req.Header.Set("X-Shift-Machine-Time", ts)
	req.Header.Set("X-Shift-Machine-Nonce", n)
	req.Header.Set("X-Shift-Machine-Proof", base64.RawStdEncoding.EncodeToString(id.Sign(ControlMessage(req.Method, req.URL.EscapedPath(), ts, n, body))))
	return nil
}
func VerifyControl(req *http.Request, machine string, body []byte) (string, error) {
	ts := req.Header.Get("X-Shift-Machine-Time")
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(seconds, 0)) > time.Minute || time.Until(time.Unix(seconds, 0)) > time.Minute {
		return "", errors.New("machine proof expired")
	}
	nonce := req.Header.Get("X-Shift-Machine-Nonce")
	raw, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil || len(raw) != 24 {
		return "", errors.New("machine nonce invalid")
	}
	key, err := base64.RawStdEncoding.DecodeString(req.Header.Get("X-Shift-Machine-Key"))
	if err != nil {
		return "", errors.New("machine key missing")
	}
	keyID, err := identity.PublicKeyID(string(key))
	if err != nil || keyID != machine {
		return "", errors.New("machine key mismatch")
	}
	proof, err := base64.RawStdEncoding.DecodeString(req.Header.Get("X-Shift-Machine-Proof"))
	if err != nil {
		return "", errors.New("machine proof missing")
	}
	if identity.Verify(string(key), ControlMessage(req.Method, req.URL.EscapedPath(), ts, nonce, body), proof) != nil {
		return "", errors.New("machine proof invalid")
	}
	return nonce, nil
}
func ControlRequest(ctx context.Context, client *http.Client, base, org, apiKey string, id *identity.Identity, suffix string, input, output any) (bool, error) {
	path := "/v1/organizations/" + url.PathEscape(org) + "/machines/" + url.PathEscape(id.Machine.ID) + suffix
	method := http.MethodGet
	var body []byte
	if input != nil {
		method = http.MethodPost
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			return false, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if err := SignControl(req, id, body); err != nil {
		return false, err
	}
	response, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("outbound control connection failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == 204 {
		return false, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, fmt.Errorf("relay control refused request: HTTP %d", response.StatusCode)
	}
	if output != nil {
		if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(output); err != nil {
			return false, err
		}
	}
	return true, nil
}

type PrepareRequest struct {
	MigrationID          string `json:"migration_id"`
	WorkloadID           string `json:"workload_id"`
	DestinationMachineID string `json:"destination_machine_id"`
	Mode                 string `json:"mode"`
	TTLSeconds           int    `json:"ttl_seconds,omitempty"`
	// PayloadVersion names the schema of this payload so a relay or agent
	// that does not understand it refuses the request instead of guessing.
	PayloadVersion int `json:"payload_version,omitempty"`
}
type TicketResponse struct {
	Token                string    `json:"token"`
	Endpoint             string    `json:"endpoint"`
	ExpiresAt            time.Time `json:"expires_at"`
	SourceMachineID      string    `json:"source_machine_id"`
	DestinationMachineID string    `json:"destination_machine_id"`
	MigrationID          string    `json:"migration_id"`
}
type JoinCommand struct {
	Ticket               string    `json:"ticket"`
	Endpoint             string    `json:"endpoint"`
	SourceMachineID      string    `json:"source_machine_id"`
	DestinationMachineID string    `json:"destination_machine_id"`
	MigrationID          string    `json:"migration_id"`
	ExpiresAt            time.Time `json:"expires_at"`
	// PayloadVersion names the schema of this payload. A receiver that does
	// not understand the version reports a version-mismatch failure (which
	// the command channel redelivers) rather than a terminal one.
	PayloadVersion int `json:"payload_version,omitempty"`
}
type Command struct {
	ID         string          `json:"id"`
	Action     string          `json:"action"`
	Payload    json.RawMessage `json:"payload"`
	ClaimToken string          `json:"claim_token"`
	// PayloadVersion names the schema of Payload. It is the command channel's
	// version-skew contract: 0 (absent) is the original schema, and a
	// receiver that gets a newer version than it knows must reject the
	// command for redelivery instead of mis-executing it.
	PayloadVersion int `json:"payload_version,omitempty"`
}

// SameEndpoint reports whether two relay endpoint spellings name the same
// relay. Joins bind their endpoint to the agent's configured relay endpoint;
// comparing by URL scheme and host (with the default port folded in) means a
// trailing slash or a default-port alias can never fail a join permanently.
func SameEndpoint(left, right string) bool {
	a, errA := endpoint.Parse(strings.TrimSpace(left))
	b, errB := endpoint.Parse(strings.TrimSpace(right))
	if errA != nil || errB != nil {
		return strings.TrimRight(strings.TrimSpace(left), "/") == strings.TrimRight(strings.TrimSpace(right), "/")
	}
	if !strings.EqualFold(a.Scheme, b.Scheme) || !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	return effectivePort(a) == effectivePort(b)
}

func effectivePort(parsed url.URL) string {
	if port := parsed.Port(); port != "" {
		return port
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "wss":
		return "443"
	case "http", "ws":
		return "80"
	default:
		return ""
	}
}
