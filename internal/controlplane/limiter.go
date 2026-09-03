package controlplane

import (
	"sync"
	"time"
)

type rateBucket struct {
	started time.Time
	count   int
	seen    time.Time
}

type RateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	maxKeys int
	buckets map[string]rateBucket
	now     func() time.Time
}

func NewRateLimiter(limit int) *RateLimiter {
	if limit < 1 {
		limit = 1
	}
	return &RateLimiter{limit: limit, window: time.Minute, maxKeys: 10000, buckets: make(map[string]rateBucket), now: time.Now}
}

func (limiter *RateLimiter) Allow(key string) bool {
	if key == "" {
		key = "anonymous"
	}
	now := limiter.now()
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if bucket, ok := limiter.buckets[key]; ok && now.Sub(bucket.started) < limiter.window {
		bucket.seen = now
		if bucket.count >= limiter.limit {
			limiter.buckets[key] = bucket
			return false
		}
		bucket.count++
		limiter.buckets[key] = bucket
		return true
	}
	if len(limiter.buckets) >= limiter.maxKeys {
		limiter.evict(now)
		if len(limiter.buckets) >= limiter.maxKeys {
			return false
		}
	}
	limiter.buckets[key] = rateBucket{started: now, count: 1, seen: now}
	return true
}

func (limiter *RateLimiter) evict(now time.Time) {
	for key, bucket := range limiter.buckets {
		if now.Sub(bucket.seen) >= limiter.window {
			delete(limiter.buckets, key)
		}
	}
}
