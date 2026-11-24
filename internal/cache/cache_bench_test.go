package cache

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func BenchmarkMemoryGetSet(b *testing.B) {
	ctx := context.Background()
	m := NewMemory(10_000)
	v := []byte(`{"choices":[{"message":{"content":"cached answer"}}]}`)
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			k := strconv.Itoa(i % 20_000)
			if _, ok := m.Get(ctx, k); !ok {
				m.Set(ctx, k, v, time.Minute)
			}
			i++
		}
	})
}
