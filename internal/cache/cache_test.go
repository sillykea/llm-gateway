package cache

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sillykea/llm-gateway/internal/api"
)

func TestMemoryExpiryAndEviction(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	m := NewMemory(2)
	m.now = func() time.Time { return now }

	m.Set(ctx, "a", []byte("1"), time.Minute)
	m.Set(ctx, "b", []byte("2"), time.Minute)
	m.Get(ctx, "a")                           // a is now most recently used
	m.Set(ctx, "c", []byte("3"), time.Minute) // evicts b

	if _, ok := m.Get(ctx, "b"); ok {
		t.Error("b should have been evicted")
	}
	if v, ok := m.Get(ctx, "a"); !ok || string(v) != "1" {
		t.Error("a should still be cached")
	}

	now = now.Add(2 * time.Minute)
	if _, ok := m.Get(ctx, "a"); ok {
		t.Error("a should have expired")
	}
}

func TestKeyAndCacheable(t *testing.T) {
	msgs := []api.Message{{Role: "user", Content: "hi"}}
	a := Key(api.ChatRequest{Model: "m", Messages: msgs})
	b := Key(api.ChatRequest{Model: "m", Messages: msgs})
	c := Key(api.ChatRequest{Model: "other", Messages: msgs})
	if a != b || a == c {
		t.Error("keys should be stable per model and conversation")
	}

	zero, warm := 0.0, 0.7
	if !Cacheable(api.ChatRequest{}) || !Cacheable(api.ChatRequest{Temperature: &zero}) {
		t.Error("deterministic requests should be cacheable")
	}
	if Cacheable(api.ChatRequest{Temperature: &warm}) {
		t.Error("sampled requests should not be cacheable")
	}
}

// fakeRedis is a tiny in-process server that understands GET, SET and PING,
// so the RESP client can be tested without a real Redis.
func fakeRedis(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	store := map[string]string{}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				rd := bufio.NewReader(c)
				for {
					args, err := readCommand(rd)
					if err != nil {
						return
					}
					mu.Lock()
					switch strings.ToUpper(args[0]) {
					case "PING":
						fmt.Fprint(c, "+PONG\r\n")
					case "SET":
						store[args[1]] = args[2]
						fmt.Fprint(c, "+OK\r\n")
					case "GET":
						if v, ok := store[args[1]]; ok {
							fmt.Fprintf(c, "$%d\r\n%s\r\n", len(v), v)
						} else {
							fmt.Fprint(c, "$-1\r\n")
						}
					}
					mu.Unlock()
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func readCommand(rd *bufio.Reader) ([]string, error) {
	line, err := rd.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
	args := make([]string, n)
	for i := range args {
		if _, err := rd.ReadString('\n'); err != nil { // $len line
			return nil, err
		}
		v, err := rd.ReadString('\n')
		if err != nil {
			return nil, err
		}
		args[i] = strings.TrimSuffix(v, "\r\n")
	}
	return args, nil
}

func TestRedis(t *testing.T) {
	ctx := context.Background()
	r := NewRedis(fakeRedis(t), RedisOptions{PoolSize: 4})

	if err := r.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, ok := r.Get(ctx, "missing"); ok {
		t.Error("missing key should be a miss")
	}
	r.Set(ctx, "k", []byte(`{"ok":true}`), time.Minute)
	if v, ok := r.Get(ctx, "k"); !ok || string(v) != `{"ok":true}` {
		t.Errorf("got %q, %v", v, ok)
	}
}

func TestRedisDownIsAMiss(t *testing.T) {
	r := NewRedis("127.0.0.1:1", RedisOptions{PoolSize: 1}) // nothing listens here
	if _, ok := r.Get(context.Background(), "k"); ok {
		t.Error("an unreachable Redis should behave like a cache miss")
	}
}

func TestRedisConcurrent(t *testing.T) {
	ctx := context.Background()
	r := NewRedis(fakeRedis(t), RedisOptions{PoolSize: 4})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := fmt.Sprintf("k%d", i)
			r.Set(ctx, k, []byte(k), time.Minute)
			if v, ok := r.Get(ctx, k); !ok || string(v) != k {
				t.Errorf("%s: got %q %v", k, v, ok)
			}
		}(i)
	}
	wg.Wait()
	if len(r.pool) > 4 {
		t.Errorf("pool grew past its size: %d", len(r.pool))
	}
}
