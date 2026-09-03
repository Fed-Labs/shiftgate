package controlplane

import (
	"testing"
	"time"
)

func TestRateLimiterWindow(t *testing.T) {
	limiter := NewRateLimiter(2)
	now := time.Unix(100, 0)
	limiter.now = func() time.Time { return now }
	if !limiter.Allow("client") || !limiter.Allow("client") || limiter.Allow("client") {
		t.Fatal("rate limit did not enforce the configured window")
	}
	now = now.Add(time.Minute)
	if !limiter.Allow("client") {
		t.Fatal("rate limit did not reset after the window")
	}
}
