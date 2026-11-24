package ratelimit

import (
	"testing"
	"time"
)

func TestTokenBucket(t *testing.T) {
	now := time.Now()
	l := New()
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("team-a", 3); !ok {
			t.Fatalf("request %d should be allowed by the initial burst", i+1)
		}
	}
	ok, wait := l.Allow("team-a", 3)
	if ok {
		t.Fatal("fourth request should be limited")
	}
	if wait <= 0 || wait > 20*time.Second {
		t.Errorf("wait = %v, want about 20s", wait)
	}

	if ok, _ := l.Allow("team-b", 3); !ok {
		t.Error("keys must have separate buckets")
	}

	now = now.Add(20 * time.Second) // 3 rpm refills one token every 20s
	if ok, _ := l.Allow("team-a", 3); !ok {
		t.Error("bucket should have refilled")
	}
}

func TestUnlimited(t *testing.T) {
	l := New()
	for i := 0; i < 1000; i++ {
		if ok, _ := l.Allow("k", 0); !ok {
			t.Fatal("rpm 0 means unlimited")
		}
	}
}

func TestIdleBucketsAreDropped(t *testing.T) {
	now := time.Now()
	l := New()
	l.now = func() time.Time { return now }

	for _, k := range []string{"a", "b", "c"} {
		l.Allow(k, 60)
	}
	now = now.Add(idleAfter + time.Minute)
	l.Allow("d", 60) // triggers a sweep

	if l.Len() != 1 {
		t.Errorf("buckets = %d, want 1 (only the active key)", l.Len())
	}
}
