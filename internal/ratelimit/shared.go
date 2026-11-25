package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Evaler runs a Lua script on Redis. *cache.Redis implements it.
type Evaler interface {
	Eval(ctx context.Context, script string, keys []string, args ...string) (any, error)
}

// tokenBucket is the same bucket as Limiter, run inside Redis so every
// replica spends from one bucket per key. It uses Redis's own clock (TIME),
// so replicas with drifting clocks still agree. The script runs atomically:
// two replicas can't both take the last token.
//
// Returns {allowed (0/1), milliseconds to wait}.
const tokenBucket = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local capacity = tonumber(ARGV[1])
local per_ms = capacity / 60000
local b = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(b[1]) or capacity
local ts = tonumber(b[2]) or now
tokens = math.min(capacity, tokens + math.max(0, now - ts) * per_ms)
local allowed, wait = 0, 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  wait = math.ceil((1 - tokens) / per_ms)
end
redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'ts', now)
redis.call('PEXPIRE', KEYS[1], 60000 + 1000)
return {allowed, wait}
`

// Shared is a rate limiter whose buckets live in Redis, so N replicas
// enforce one limit rather than N. If Redis can't be reached it falls back
// to a per-replica bucket: limits get looser for a while, but the gateway
// keeps serving instead of failing every request.
type Shared struct {
	redis    Evaler
	local    *Limiter
	timeout  time.Duration
	onError  func(error)
	keyspace string
}

// NewShared returns a shared limiter. onError (may be nil) is called each
// time Redis fails and the local fallback answers instead.
func NewShared(r Evaler, onError func(error)) *Shared {
	if onError == nil {
		onError = func(error) {}
	}
	return &Shared{redis: r, local: New(), timeout: 200 * time.Millisecond, onError: onError, keyspace: "llmgw:rl:"}
}

// Allow has the same contract as Limiter.Allow.
func (s *Shared) Allow(key string, rpm int) (bool, time.Duration) {
	if rpm <= 0 {
		return true, 0
	}
	ok, wait, err := s.allow(key, rpm)
	if err != nil {
		s.onError(err)
		return s.local.Allow(key, rpm)
	}
	return ok, wait
}

func (s *Shared) allow(key string, rpm int) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	v, err := s.redis.Eval(ctx, tokenBucket, []string{s.keyspace + key}, strconv.Itoa(rpm))
	if err != nil {
		return false, 0, err
	}
	arr, ok := v.([]any)
	if !ok || len(arr) != 2 {
		return false, 0, fmt.Errorf("rate limit script: unexpected reply %v", v)
	}
	allowed, ok1 := arr[0].(int64)
	waitMS, ok2 := arr[1].(int64)
	if !ok1 || !ok2 {
		return false, 0, errors.New("rate limit script: reply is not two integers")
	}
	return allowed == 1, time.Duration(waitMS) * time.Millisecond, nil
}
