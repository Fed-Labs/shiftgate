package controlplane

import (
	"fmt"
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

func TestRateLimiterEvictsLeastRecentlyUsedInsteadOfDenying(t *testing.T) {
	limiter := NewRateLimiter(5)
	limiter.maxKeys = 8
	now := time.Unix(100, 0)
	limiter.now = func() time.Time { return now }
	// Fill the key space well past capacity with unique keys. None may be
	// denied just because the map is full — the oldest buckets get evicted.
	for index := 0; index < 100; index++ {
		if !limiter.Allow(fmt.Sprintf("attacker-%d", index)) {
			t.Fatalf("new key %d was denied while the key space was full", index)
		}
	}
	if limiter.Size() > 8 {
		t.Fatalf("key space grew to %d, want at most %d", limiter.Size(), 8)
	}
	// The first keys are long gone from the LRU tail; a fresh legitimate
	// client must still be admitted.
	if !limiter.Allow("legitimate-client") {
		t.Fatal("legitimate new client was locked out by key-space exhaustion")
	}
	// And the same client keeps its own budget afterwards.
	if !limiter.Allow("legitimate-client") {
		t.Fatal("existing key lost its bucket after eviction churn")
	}
}

func TestRateLimiterPrunesStaleKeys(t *testing.T) {
	limiter := NewRateLimiter(5)
	now := time.Unix(100, 0)
	limiter.now = func() time.Time { return now }
	for index := 0; index < 5; index++ {
		limiter.Allow(fmt.Sprintf("stale-%d", index))
	}
	if limiter.Size() != 5 {
		t.Fatalf("size = %d, want 5", limiter.Size())
	}
	// Keys unseen for a full window are dropped on the next admission, so a
	// slow flood of unique keys cannot accumulate forever.
	now = now.Add(2 * time.Minute)
	limiter.Allow("fresh")
	if limiter.Size() != 1 {
		t.Fatalf("stale keys survived pruning: size = %d, want 1", limiter.Size())
	}
}

func TestRateLimiterWindowAppliesPerKey(t *testing.T) {
	limiter := NewRateLimiter(1)
	now := time.Unix(100, 0)
	limiter.now = func() time.Time { return now }
	if !limiter.Allow("one") || limiter.Allow("one") {
		t.Fatal("per-key window did not enforce")
	}
	if !limiter.Allow("two") {
		t.Fatal("one key's budget must not affect another's")
	}
}
