package budget

import (
	"testing"
	"time"
)

func TestBudget(t *testing.T) {
	now := time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)
	tr := New()
	tr.now = func() time.Time { return now }

	tr.Add("team", 4)
	if tr.Exceeded("team", 5) {
		t.Fatal("4 of 5 is not exceeded")
	}
	tr.Add("team", 1.5)
	if !tr.Exceeded("team", 5) {
		t.Fatal("5.5 of 5 is exceeded")
	}
	if tr.Exceeded("team", 0) {
		t.Fatal("a zero limit means no limit")
	}

	now = now.Add(2 * time.Hour) // into December
	if tr.Spent("team") != 0 {
		t.Fatal("spend should reset each month")
	}
}
