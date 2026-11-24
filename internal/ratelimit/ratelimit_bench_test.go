package ratelimit

import (
	"strconv"
	"testing"
)

func BenchmarkAllowParallel(b *testing.B) {
	l := New()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			l.Allow("key-"+strconv.Itoa(i%16), 1_000_000)
			i++
		}
	})
}
