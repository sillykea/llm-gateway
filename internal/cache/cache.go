// Package cache stores responses for identical, deterministic requests.
package cache

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/sillykea/llm-gateway/internal/api"
)

// Cache is implemented by the memory and Redis backends.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)
}

// Key builds a stable cache key from the alias and the conversation.
func Key(req api.ChatRequest) string {
	payload, _ := json.Marshal(struct {
		Model     string        `json:"m"`
		Messages  []api.Message `json:"msgs"`
		MaxTokens *int          `json:"mt,omitempty"`
	}{req.Model, req.Messages, req.MaxTokens})
	sum := sha256.Sum256(payload)
	return "llmgw:" + hex.EncodeToString(sum[:])
}

// Cacheable reports whether a request is deterministic enough to cache.
func Cacheable(req api.ChatRequest) bool {
	return req.Temperature == nil || *req.Temperature == 0
}

// Memory is a size-bounded LRU cache with per-entry expiry.
type Memory struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[string]*list.Element
	now   func() time.Time
}

type entry struct {
	key     string
	value   []byte
	expires time.Time
}

// NewMemory creates an LRU cache holding at most max entries.
func NewMemory(max int) *Memory {
	return &Memory{max: max, ll: list.New(), items: map[string]*list.Element{}, now: time.Now}
}

func (m *Memory) Get(_ context.Context, key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*entry)
	if m.now().After(e.expires) {
		m.ll.Remove(el)
		delete(m.items, key)
		return nil, false
	}
	m.ll.MoveToFront(el)
	return e.value, true
}

func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		e := el.Value.(*entry)
		e.value, e.expires = value, m.now().Add(ttl)
		m.ll.MoveToFront(el)
		return
	}
	m.items[key] = m.ll.PushFront(&entry{key: key, value: value, expires: m.now().Add(ttl)})
	if m.ll.Len() > m.max {
		oldest := m.ll.Back()
		m.ll.Remove(oldest)
		delete(m.items, oldest.Value.(*entry).key)
	}
}
