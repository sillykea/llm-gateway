package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sillykea/llm-gateway/internal/cache"
)

// redisForTest returns a client for the Redis named by GATEWAY_TEST_REDIS_ADDR
// (CI runs one), or skips.
func redisForTest(t *testing.T) *cache.Redis {
	t.Helper()
	addr := os.Getenv("GATEWAY_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("GATEWAY_TEST_REDIS_ADDR not set")
	}
	r := cache.NewRedis(addr, cache.RedisOptions{PoolSize: 20})
	if err := r.Ping(context.Background()); err != nil {
		t.Fatalf("redis at %s: %v", addr, err)
	}
	return r
}

func replica(r Evaler, prefix string, errs *atomic.Int64) *Shared {
	s := NewShared(r, func(error) { errs.Add(1) })
	s.keyspace = prefix
	return s
}

func prefix() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "test:" + hex.EncodeToString(b) + ":"
}

// Three replicas sharing one Redis enforce one limit, not three.
func TestSharedLimitAcrossReplicas(t *testing.T) {
	r := redisForTest(t)
	p := prefix()
	var errs atomic.Int64
	replicas := []*Shared{replica(r, p, &errs), replica(r, p, &errs), replica(r, p, &errs)}

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := range 90 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := replicas[i%3].Allow("team-a", 30); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if errs.Load() != 0 {
		t.Fatalf("%d redis errors", errs.Load())
	}
	// 30 tokens, plus at most one refilled while the burst ran.
	if n := allowed.Load(); n < 30 || n > 31 {
		t.Errorf("allowed %d of 90 across three replicas, want 30 (one shared bucket)", n)
	}
	ok, wait := replicas[0].Allow("team-a", 30)
	if ok || wait <= 0 || wait > 2*time.Second {
		t.Errorf("empty bucket: ok=%v wait=%v, want refused with about 2s to wait", ok, wait)
	}
	if ok, _ := replicas[1].Allow("team-b", 30); !ok {
		t.Error("another key's bucket is separate")
	}
}

func TestSharedRefills(t *testing.T) {
	r := redisForTest(t)
	var errs atomic.Int64
	s := replica(r, prefix(), &errs)
	for range 600 {
		s.Allow("fast", 600) // drain 10 tokens a second
	}
	if ok, _ := s.Allow("fast", 600); ok {
		t.Fatal("bucket should be empty")
	}
	time.Sleep(150 * time.Millisecond) // 1.5 tokens back
	if ok, _ := s.Allow("fast", 600); !ok {
		t.Error("bucket did not refill")
	}
}

type failing struct{}

func (failing) Eval(context.Context, string, []string, ...string) (any, error) {
	return nil, errors.New("connection refused")
}

type odd struct{ reply any }

func (o odd) Eval(context.Context, string, []string, ...string) (any, error) { return o.reply, nil }

// Without Redis the gateway keeps limiting, per replica, and says so.
func TestSharedFallsBackToLocal(t *testing.T) {
	var errs atomic.Int64
	s := replica(failing{}, "x:", &errs)
	allowed := 0
	for range 20 {
		if ok, _ := s.Allow("k", 10); ok {
			allowed++
		}
	}
	if allowed != 10 || errs.Load() != 20 {
		t.Errorf("allowed %d (want 10 from the local bucket), %d errors reported (want 20)", allowed, errs.Load())
	}
	for _, reply := range []any{"OK", []any{int64(1)}, []any{"1", "0"}} {
		var e atomic.Int64
		if ok, _ := replica(odd{reply}, "y:", &e).Allow("k", 10); !ok || e.Load() != 1 {
			t.Errorf("reply %v: ok=%v errors=%d", reply, ok, e.Load())
		}
	}
	if ok, _ := replica(failing{}, "z:", &errs).Allow("k", 0); !ok {
		t.Error("rpm 0 means unlimited and needs no Redis")
	}
}
