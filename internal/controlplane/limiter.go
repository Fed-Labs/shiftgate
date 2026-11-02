package controlplane

import (
	"container/list"
	"fmt"
	"strings"
	"sync"
	"time"
)

// limiter.go is a per-key fixed-window request limiter with a bounded key
// space. Buckets live in an LRU keyed by their most recent observation: when
// the map is full the least-recently-seen bucket is evicted to admit the new
// key, so a flood of unique keys can only cost itself — never lock out new
// legitimate callers. Stale buckets are pruned after a full idle window.

// rateLimitCredentialPepper digests bearer credentials into rate-limit keys.
// A pepper of its own — never the token pepper — keeps rate-limit keys useless
// as oracle material for the session and API-key digest columns.
const rateLimitCredentialPepper = "rate-limit-key"

// validatedTTL bounds how long a successful authentication keeps a credential
// eligible for its own rate-limit bucket. Every authenticated request re-marks
// the credential, so live traffic never falls back to the shared per-IP
// bucket; a revoked credential does once its mark expires.
const validatedTTL = 5 * time.Minute

type rateBucket struct {
	key     string
	started time.Time
	count   int
	seen    time.Time
}

type RateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	maxKeys int
	buckets map[string]*list.Element
	// lru orders buckets by recency of use: front is most recently seen.
	lru *list.List
	// validated holds bearer credential ids (see rateLimitCredentialID) that
	// authenticated recently, with the time of their last successful
	// authentication. Only these earn a per-credential bucket.
	validated map[string]time.Time
	now       func() time.Time
}

func NewRateLimiter(limit int) *RateLimiter {
	if limit < 1 {
		limit = 1
	}
	return &RateLimiter{
		limit:     limit,
		window:    time.Minute,
		maxKeys:   10000,
		buckets:   make(map[string]*list.Element),
		lru:       list.New(),
		validated: make(map[string]time.Time),
		now:       time.Now,
	}
}

// rateLimitCredentialID is the rate-limit identity of a presented bearer
// credential: a short digest under the rate-limit pepper, formatted exactly
// the way compound keys carry it and MarkValidated records it.
func rateLimitCredentialID(token string) string {
	digest := tokenDigest(rateLimitCredentialPepper, token)
	return fmt.Sprintf("%x", digest[:8])
}

// MarkValidated records that a bearer credential just authenticated. Only a
// validated credential earns its own rate-limit bucket: an unauthenticated
// caller rotating forged Authorization headers always falls back to the
// shared per-client-IP bucket and cannot mint one bucket per forged value.
func (limiter *RateLimiter) MarkValidated(credentialID string) {
	if credentialID == "" {
		return
	}
	now := limiter.now()
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if limiter.validated == nil {
		limiter.validated = make(map[string]time.Time)
	}
	for key, seen := range limiter.validated {
		if now.Sub(seen) >= validatedTTL {
			delete(limiter.validated, key)
		}
	}
	if len(limiter.validated) >= limiter.maxKeys {
		oldestKey, oldestSeen := "", now
		for key, seen := range limiter.validated {
			if seen.Before(oldestSeen) {
				oldestKey, oldestSeen = key, seen
			}
		}
		delete(limiter.validated, oldestKey)
	}
	limiter.validated[credentialID] = now
}

// effectiveKey resolves the bucket a request must be charged to. A compound
// "ip:...|token:..." key — see rateLimitKey — folds in both the derived client
// IP and the presented credential's digest: the credential bucket is used only
// once that credential has authenticated; every unauthenticated presentation,
// including forged Authorization headers rotated per request, charges the
// per-IP bucket instead. Plain keys pass through unchanged.
func (limiter *RateLimiter) effectiveKey(key string) string {
	ipKey, credentialID, ok := strings.Cut(key, "|token:")
	if !ok {
		return key
	}
	if limiter.isValidated(credentialID) {
		return "token:" + credentialID
	}
	return ipKey
}

// isValidated reports whether a credential id authenticated within the
// validation window.
func (limiter *RateLimiter) isValidated(credentialID string) bool {
	now := limiter.now()
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	seen, ok := limiter.validated[credentialID]
	return ok && now.Sub(seen) < validatedTTL
}

func (limiter *RateLimiter) Allow(key string) bool {
	key = limiter.effectiveKey(key)
	if key == "" {
		key = "anonymous"
	}
	now := limiter.now()
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if element, ok := limiter.buckets[key]; ok {
		bucket := element.Value.(*rateBucket)
		bucket.seen = now
		limiter.lru.MoveToFront(element)
		if now.Sub(bucket.started) < limiter.window {
			if bucket.count >= limiter.limit {
				return false
			}
			bucket.count++
			return true
		}
		bucket.started = now
		bucket.count = 1
		return true
	}
	limiter.prune(now)
	for len(limiter.buckets) >= limiter.maxKeys {
		oldest := limiter.lru.Back()
		if oldest == nil {
			break
		}
		limiter.removeElement(oldest)
	}
	limiter.buckets[key] = limiter.lru.PushFront(&rateBucket{key: key, started: now, count: 1, seen: now})
	return true
}

// Size reports the number of tracked keys. It exists for tests and diagnostics.
func (limiter *RateLimiter) Size() int {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return len(limiter.buckets)
}

// prune drops every bucket unseen for at least one full window. The list is
// ordered by recency, so the walk can stop at the first bucket still inside
// the window.
func (limiter *RateLimiter) prune(now time.Time) {
	for element := limiter.lru.Back(); element != nil; {
		bucket := element.Value.(*rateBucket)
		if now.Sub(bucket.seen) < limiter.window {
			return
		}
		previous := element.Prev()
		limiter.removeElement(element)
		element = previous
	}
}

func (limiter *RateLimiter) removeElement(element *list.Element) {
	bucket := element.Value.(*rateBucket)
	delete(limiter.buckets, bucket.key)
	limiter.lru.Remove(element)
}

// apiBudget is the per-organization API request budget (plan 2 BR-11, plan 4
// BR-07): a daily budget (1k free / 50k pro / unlimited business) with an
// hourly burst ceiling (600 / 6000 / 30000). Machine-to-machine presence
// heartbeats are exempt — they would self-DoS the fleet. Counters live in
// memory per replica; the metered usage_records gauge is the durable record.
type apiBudget struct {
	mu   sync.Mutex
	seen map[string]*orgBudgetCounters
	now  func() time.Time
}

type orgBudgetCounters struct {
	dayKey    string
	dayCount  int
	hourKey   string
	hourCount int
	seen      time.Time
}

func newAPIBudget() *apiBudget {
	return &apiBudget{seen: map[string]*orgBudgetCounters{}, now: time.Now}
}

// Allow consumes one request against the organization's budgets. Unlimited
// (negative) budgets always pass. A false return means the daily budget or
// the hourly ceiling is exhausted; the caller answers 429 RATE_LIMIT_UPGRADE.
func (budget *apiBudget) Allow(organizationID string, dailyLimit, hourlyLimit int) bool {
	if dailyLimit < 0 && hourlyLimit < 0 {
		return true
	}
	now := budget.now().UTC()
	dayKey := now.Format("2006-01-02")
	hourKey := now.Format("2006-01-02T15")
	budget.mu.Lock()
	defer budget.mu.Unlock()
	budget.prune(now)
	counters, ok := budget.seen[organizationID]
	if !ok {
		counters = &orgBudgetCounters{}
		budget.seen[organizationID] = counters
	}
	counters.seen = now
	if counters.dayKey != dayKey {
		counters.dayKey = dayKey
		counters.dayCount = 0
	}
	if counters.hourKey != hourKey {
		counters.hourKey = hourKey
		counters.hourCount = 0
	}
	if dailyLimit >= 0 && counters.dayCount >= dailyLimit {
		return false
	}
	if hourlyLimit >= 0 && counters.hourCount >= hourlyLimit {
		return false
	}
	counters.dayCount++
	counters.hourCount++
	return true
}

// Remaining reports how much of the daily budget is left, for the entitlement
// readout and the usage summary. Unlimited budgets report -1.
func (budget *apiBudget) Remaining(organizationID string, dailyLimit int) int {
	if dailyLimit < 0 {
		return -1
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	counters, ok := budget.seen[organizationID]
	if !ok {
		return dailyLimit
	}
	left := dailyLimit - counters.dayCount
	if left < 0 {
		return 0
	}
	return left
}

// prune drops counters idle for over a day so a flood of orgs cannot grow the
// map without bound.
func (budget *apiBudget) prune(now time.Time) {
	for key, counters := range budget.seen {
		if now.Sub(counters.seen) > 24*time.Hour {
			delete(budget.seen, key)
		}
	}
}
