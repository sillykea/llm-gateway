// Package breaker implements a circuit breaker per upstream provider.
//
// Without one, a provider that is down still costs every request a full
// timeout before the gateway falls back. The breaker notices repeated
// failures and skips that provider for a cooldown period, then lets a single
// probe request through to see whether it has recovered.
//
//	closed --(threshold failures)--> open --(cooldown)--> half-open
//	   ^                                                     |
//	   +-------------(probe succeeds)------------------------+
//	                 (probe fails) -> open again
package breaker

import (
	"sync"
	"time"
)

// State of a breaker.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	return [...]string{"closed", "open", "half_open"}[s]
}

// Breaker is safe for concurrent use.
type Breaker struct {
	mu        sync.Mutex
	state     State
	failures  int
	threshold int
	cooldown  time.Duration
	openedAt  time.Time
	probing   bool // a half-open probe is in flight
	now       func() time.Time
}

// New returns a closed breaker that opens after threshold consecutive
// failures and stays open for cooldown.
func New(threshold int, cooldown time.Duration) *Breaker {
	if threshold <= 0 {
		threshold = 5
	}
	return &Breaker{threshold: threshold, cooldown: cooldown, now: time.Now}
}

// Allow reports whether a request may be sent now. In the half-open state
// only one probe is allowed at a time, so a recovering provider isn't hit by
// a thundering herd.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return true
	case Open:
		if b.now().Sub(b.openedAt) < b.cooldown {
			return false
		}
		b.state = HalfOpen
		fallthrough
	default: // HalfOpen
		if b.probing {
			return false
		}
		b.probing = true
		return true
	}
}

// Success records a healthy response and closes the breaker.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state, b.failures, b.probing = Closed, 0, false
}

// Failure records a provider-side failure.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	if b.state == HalfOpen {
		b.trip()
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.trip()
	}
}

func (b *Breaker) trip() {
	b.state = Open
	b.openedAt = b.now()
	b.failures = 0
}

// State returns the current state, for metrics.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
