// Package ratelimit implements a per-key token bucket.
//
// Each API key gets a bucket that holds up to `rpm` tokens and refills at
// rpm/60 tokens per second. A request spends one token. This allows short
// bursts while holding the long-run rate to the configured limit.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

// idleAfter is how long a bucket can go unused before it is dropped. A bucket
// idle this long has fully refilled anyway, so recreating it later is identical.
const idleAfter = 10 * time.Minute

// Limiter tracks one bucket per key.
type Limiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	now       func() time.Time
	lastSweep time.Time
}

// New returns an empty limiter.
func New() *Limiter {
	return &Limiter{buckets: map[string]*bucket{}, now: time.Now}
}

// sweep removes idle buckets at most once a minute so memory stays bounded
// by the number of active keys, not every key ever seen. Caller holds mu.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if now.Sub(b.last) > idleAfter {
			delete(l.buckets, k)
		}
	}
}

// Len reports how many buckets are held (used in tests).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Allow spends a token for key. When the bucket is empty it returns false and
// how long the caller should wait before retrying. rpm <= 0 means unlimited.
func (l *Limiter) Allow(key string, rpm int) (bool, time.Duration) {
	if rpm <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	capacity := float64(rpm)
	perSecond := capacity / 60

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: capacity, last: now}
		l.buckets[key] = b
	}
	l.sweep(now)
	b.tokens = math.Min(capacity, b.tokens+now.Sub(b.last).Seconds()*perSecond)
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / perSecond * float64(time.Second))
	return false, wait
}
