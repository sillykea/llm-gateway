package budget

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sillykea/llm-gateway/internal/cache"
)

func redisForTest(t *testing.T) *cache.Redis {
	t.Helper()
	addr := os.Getenv("GATEWAY_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("GATEWAY_TEST_REDIS_ADDR not set")
	}
	return cache.NewRedis(addr, cache.RedisOptions{PoolSize: 20})
}

func uniqueKey() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "team-" + hex.EncodeToString(b)
}

func TestSharedBudgetAcrossReplicas(t *testing.T) {
	r := redisForTest(t)
	key := uniqueKey()
	now := time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)
	a, b := NewShared(r, func(err error) { t.Error(err) }), NewShared(r, func(err error) { t.Error(err) })
	a.now, b.now = func() time.Time { return now }, func() time.Time { return now }

	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() { defer wg.Done(); a.Add(key, 0.125) }()
	}
	wg.Wait()
	if got := b.Spent(key); got != 5 {
		t.Errorf("replica b sees %v, want 5 (40 × 0.125 added on replica a)", got)
	}
	if !b.Exceeded(key, 5) || b.Exceeded(key, 5.01) || b.Exceeded(key, 0) {
		t.Error("Exceeded is wrong at the boundary")
	}
	now = now.Add(2 * time.Hour) // December
	if got := a.Spent(key); got != 0 {
		t.Errorf("new month starts at %v, want 0", got)
	}
}

type down struct{}

func (down) Eval(context.Context, string, []string, ...string) (any, error) {
	return nil, errors.New("timeout")
}

func TestSharedBudgetFallsBack(t *testing.T) {
	n := 0
	s := NewShared(down{}, func(error) { n++ })
	s.Add("k", 3)
	s.Add("k", 3)
	if !s.Exceeded("k", 5) {
		t.Error("spend during an outage must still count")
	}
	if n != 3 {
		t.Errorf("%d errors reported, want 3", n)
	}
	s.Add("k", 0) // nothing to record, no round trip
	if n != 3 {
		t.Error("zero spend went to Redis")
	}
}
