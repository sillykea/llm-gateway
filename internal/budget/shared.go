package budget

import (
	"context"
	"strconv"
	"time"
)

// Evaler runs a Lua script on Redis. *cache.Redis implements it.
type Evaler interface {
	Eval(ctx context.Context, script string, keys []string, args ...string) (any, error)
}

// addSpend adds to a key's spend for the month and keeps the counter for 40
// days, so old months clean themselves up. Returns the new total.
const addSpend = `
local v = redis.call('INCRBYFLOAT', KEYS[1], ARGV[1])
redis.call('EXPIRE', KEYS[1], 3456000)
return v
`

const getSpend = `return redis.call('GET', KEYS[1])`

// Shared tracks spend in Redis, so a key's budget is one budget across every
// replica and survives restarts. On a Redis error it records and reads spend
// locally instead, and reports the error: budgets are a safety net, and a
// Redis blip shouldn't take the gateway down with it.
type Shared struct {
	redis   Evaler
	local   *Tracker
	timeout time.Duration
	onError func(error)
	now     func() time.Time
}

// NewShared returns a shared tracker. onError may be nil.
func NewShared(r Evaler, onError func(error)) *Shared {
	if onError == nil {
		onError = func(error) {}
	}
	return &Shared{redis: r, local: New(), timeout: 200 * time.Millisecond, onError: onError, now: time.Now}
}

func (s *Shared) key(name string) string {
	return "llmgw:spend:" + s.now().UTC().Format("2006-01") + ":" + name
}

// Add records spend for key.
func (s *Shared) Add(key string, usd float64) {
	if usd <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	if _, err := s.redis.Eval(ctx, addSpend, []string{s.key(key)}, strconv.FormatFloat(usd, 'f', -1, 64)); err != nil {
		s.onError(err)
		s.local.Add(key, usd)
	}
}

// Spent returns this month's spend for key.
func (s *Shared) Spent(key string) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	v, err := s.redis.Eval(ctx, getSpend, []string{s.key(key)})
	if err != nil {
		s.onError(err)
		return s.local.Spent(key)
	}
	str, _ := v.(string)
	if str == "" {
		return s.local.Spent(key) // nothing in Redis yet; local holds any spend recorded during an outage
	}
	f, err := strconv.ParseFloat(str, 64)
	if err != nil {
		s.onError(err)
		return s.local.Spent(key)
	}
	return f + s.local.Spent(key)
}

// Exceeded reports whether key has used up its limit. limit <= 0 means none.
// Only called when there is a limit, so keys without budgets cost no round trip.
func (s *Shared) Exceeded(key string, limit float64) bool {
	return limit > 0 && s.Spent(key) >= limit
}
