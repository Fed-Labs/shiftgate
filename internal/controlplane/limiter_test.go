package controlplane

import (
	"testing"
	"time"
)

func TestRateLimiterWindow(t *testing.T) {
	limiter := NewRateLimiter(2)
	now := time.Unix(100, 0)
	limiter.now = func() time.Time { return now }
	// Each Allow has a side effect, so the three calls are hoisted into
	// named results rather than repeated inline.
	first, second, third := limiter.Allow("client"), limiter.Allow("client"), limiter.Allow("client")
	if !first || !second || third {
		t.Fatal("rate limit did not enforce the configured window")
	}
	now = now.Add(time.Minute)
	if !limiter.Allow("client") {
		t.Fatal("rate limit did not reset after the window")
	}
}
