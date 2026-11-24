// Package budget tracks estimated spend per API key per calendar month, so a
// runaway script can't run up an unbounded bill.
//
// Spend is tracked in memory, per replica. See docs/adr/0003 for why that is
// an acceptable first step and what replacing it with Redis would involve.
package budget

import (
	"sync"
	"time"
)

// Tracker is safe for concurrent use.
type Tracker struct {
	mu    sync.Mutex
	month string
	spent map[string]float64
	now   func() time.Time
}

// New returns an empty tracker.
func New() *Tracker {
	return &Tracker{spent: map[string]float64{}, now: time.Now}
}

// rollover clears all spend when a new UTC month starts. Caller holds mu.
func (t *Tracker) rollover() {
	if m := t.now().UTC().Format("2006-01"); m != t.month {
		t.month = m
		t.spent = map[string]float64{}
	}
}

// Add records spend for key.
func (t *Tracker) Add(key string, usd float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover()
	t.spent[key] += usd
}

// Spent returns this month's spend for key.
func (t *Tracker) Spent(key string) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover()
	return t.spent[key]
}

// Exceeded reports whether key has used up its limit. limit <= 0 means none.
// Spend is only known after a response, so one in-flight request can take a
// key slightly over its limit; the next one is refused.
func (t *Tracker) Exceeded(key string, limit float64) bool {
	return limit > 0 && t.Spent(key) >= limit
}
