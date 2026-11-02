package controlplane

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shift.dev/shift/internal/config"
)

func trustList(entries ...string) func(net.IP) bool {
	networks := parseTrustedProxies(entries)
	return func(ip net.IP) bool {
		for index := range networks {
			if networks[index].Contains(ip) {
				return true
			}
		}
		return false
	}
}

func TestResolveClientIP(t *testing.T) {
	trusted := trustList("10.0.0.0/8", "192.0.2.1")
	for _, test := range []struct {
		name       string
		remoteAddr string
		forwarded  []string
		realIP     string
		trusted    func(net.IP) bool
		want       string
	}{
		{
			name:       "direct connection ignores headers",
			remoteAddr: "203.0.113.7:1234",
			forwarded:  []string{"1.2.3.4"},
			trusted:    trusted,
			want:       "203.0.113.7",
		},
		{
			name:       "no trusted proxies configured ignores headers",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"1.2.3.4"},
			trusted:    nil,
			want:       "10.0.0.5",
		},
		{
			name:       "trusted proxy yields rightmost untrusted hop",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"203.0.113.9, 10.0.0.7, 10.0.0.5"},
			trusted:    trusted,
			want:       "203.0.113.9",
		},
		{
			name:       "chained headers are concatenated right to left",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"203.0.113.9, 10.0.0.7", "10.0.0.5"},
			trusted:    trusted,
			want:       "203.0.113.9",
		},
		{
			name:       "client cannot forge its address through the proxy",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"1.2.3.4, 203.0.113.9"},
			trusted:    trusted,
			want:       "203.0.113.9",
		},
		{
			name:       "malformed junk left of the proxy-appended hop is ignored",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"not-an-ip, 203.0.113.9"},
			trusted:    trusted,
			want:       "203.0.113.9",
		},
		{
			name:       "wholly malformed chain falls back to the socket peer",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"not-an-ip"},
			trusted:    trusted,
			want:       "10.0.0.5",
		},
		{
			name:       "all hops trusted falls back to leftmost",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"10.0.0.7, 10.0.0.5"},
			trusted:    trusted,
			want:       "10.0.0.7",
		},
		{
			name:       "x-real-ip honored from trusted proxy without forwarded-for",
			remoteAddr: "192.0.2.1:1234",
			realIP:     "203.0.113.9",
			trusted:    trusted,
			want:       "203.0.113.9",
		},
		{
			name:       "x-real-ip ignored from untrusted peer",
			remoteAddr: "203.0.113.7:1234",
			realIP:     "1.2.3.4",
			trusted:    trusted,
			want:       "203.0.113.7",
		},
		{
			// The platform edge carries the client in X-Real-IP
			// while the forwarded chain ends in the edge POP address — a CDN
			// hop that shifts by region. The platform's own statement of the
			// client wins over the chain.
			name:       "x-real-ip preferred over the edge pop chain",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"203.0.113.9, 152.233.13.166"},
			realIP:     "203.0.113.9",
			trusted:    trustList("10.0.0.0/8"),
			want:       "203.0.113.9",
		},
		{
			name:       "pop chain without x-real-ip falls back to the walk",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"203.0.113.9, 152.233.13.166"},
			trusted:    trustList("10.0.0.0/8"),
			want:       "152.233.13.166",
		},
		{
			// An X-Real-IP naming another trusted proxy is an overwrite-chain
			// intermediate, not the end client; the chain walk decides.
			name:       "x-real-ip naming a trusted proxy defers to the chain walk",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"203.0.113.9, 10.0.0.7"},
			realIP:     "10.0.0.7",
			trusted:    trusted,
			want:       "203.0.113.9",
		},
		{
			name:       "x-real-ip with an optional port is unwrapped",
			remoteAddr: "10.0.0.5:1234",
			realIP:     "203.0.113.9:443",
			trusted:    trusted,
			want:       "203.0.113.9",
		},
		{
			name:       "malformed x-real-ip is ignored",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"203.0.113.9"},
			realIP:     "not-an-ip",
			trusted:    trusted,
			want:       "203.0.113.9",
		},
		{
			name:       "port-bearing hops are stripped",
			remoteAddr: "10.0.0.5:1234",
			forwarded:  []string{"203.0.113.9:443"},
			trusted:    trusted,
			want:       "203.0.113.9",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := resolveClientIP(test.remoteAddr, test.forwarded, test.realIP, test.trusted)
			if got != test.want {
				t.Fatalf("resolveClientIP() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestParseForwardedFor(t *testing.T) {
	hops := parseForwardedFor([]string{" 1.2.3.4 , 5.6.7.8:9 ", "", "9.9.9.9"})
	if len(hops) != 3 || hops[0] != "1.2.3.4" || hops[1] != "5.6.7.8" || hops[2] != "9.9.9.9" {
		t.Fatalf("parseForwardedFor() = %#v", hops)
	}
}

func TestClientIPAndRateLimitKeyUseTrustedProxies(t *testing.T) {
	server := &Server{config: config.DefaultControlPlane()}
	server.trustedProxies = parseTrustedProxies([]string{"10.0.0.0/8"})
	request := httptest.NewRequest(http.MethodGet, "/v1/plans", nil)
	request.RemoteAddr = "10.0.0.5:443"
	request.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := server.clientIP(request); got != "203.0.113.9" {
		t.Fatalf("clientIP() = %q, want the forwarded client", got)
	}
	if key := server.rateLimitKey(request); key != "ip:203.0.113.9" {
		t.Fatalf("rateLimitKey() = %q, want per-client bucket", key)
	}
	// A forged bearer must not escape the per-client bucket: the derived IP
	// stays in the key no matter what Authorization carries, and the
	// credential limb is only honored after that credential authenticates
	// (see TestLimiterCredentialBucketsRequireValidation).
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	key := server.rateLimitKey(request)
	if !strings.Contains(key, "203.0.113.9") || !strings.Contains(key, "|token:") {
		t.Fatalf("bearer rateLimitKey() = %q, want the client IP folded in alongside the credential digest", key)
	}
}
