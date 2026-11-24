package breaker

import (
	"testing"
	"time"
)

func TestLifecycle(t *testing.T) {
	now := time.Now()
	b := New(3, 30*time.Second)
	b.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		b.Failure()
	}
	if b.State() != Closed || !b.Allow() {
		t.Fatal("should stay closed below the threshold")
	}
	b.Failure()
	if b.State() != Open || b.Allow() {
		t.Fatal("should open at the threshold and reject requests")
	}

	now = now.Add(31 * time.Second)
	if !b.Allow() {
		t.Fatal("first request after cooldown should be the probe")
	}
	if b.Allow() {
		t.Fatal("only one probe at a time")
	}
	b.Failure()
	if b.State() != Open {
		t.Fatal("a failed probe should reopen the breaker")
	}

	now = now.Add(31 * time.Second)
	b.Allow()
	b.Success()
	if b.State() != Closed || !b.Allow() {
		t.Fatal("a successful probe should close the breaker")
	}
}

func TestSuccessResetsCount(t *testing.T) {
	b := New(3, time.Minute)
	b.Failure()
	b.Failure()
	b.Success()
	b.Failure()
	b.Failure()
	if b.State() != Closed {
		t.Fatal("failures must be consecutive to trip")
	}
}
