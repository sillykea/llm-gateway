package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sillykea/llm-gateway/internal/api"
	"github.com/sillykea/llm-gateway/internal/cache"
	"github.com/sillykea/llm-gateway/internal/config"
	"github.com/sillykea/llm-gateway/internal/metrics"
	"github.com/sillykea/llm-gateway/internal/provider"
)

// fakeOpenAI stands in for a hosted API. status lets a test make it fail.
func fakeOpenAI(t *testing.T, status *atomic.Int32, calls *atomic.Int32) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if s := status.Load(); s != 0 {
			w.WriteHeader(int(s))
			w.Write([]byte(`{"error":"boom"}`))
			return
		}
		var req api.ChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"from \"}}]}\n\n"))
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"cloud\"}}]}\n\n"))
			w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n"))
			w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		json.NewEncoder(w).Encode(api.ChatResponse{
			ID: "cloud-1", Object: "chat.completion",
			Choices: []api.Choice{{Message: api.Message{Role: "assistant", Content: "from cloud"}, FinishReason: "stop"}},
			Usage:   api.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeOllama stands in for a local Ollama server.
func fakeOllama(t *testing.T, calls *atomic.Int32) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/chat" {
			t.Errorf("ollama got path %s", r.URL.Path)
		}
		var req struct{ Stream bool }
		json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			w.Write([]byte(`{"message":{"content":"from "},"done":false}` + "\n"))
			w.Write([]byte(`{"message":{"content":"local"},"done":true,"prompt_eval_count":3,"eval_count":2}` + "\n"))
			return
		}
		w.Write([]byte(`{"model":"llama3.1","message":{"role":"assistant","content":"from local"},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":5}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type harness struct {
	handler     http.Handler
	server      *Server
	cloudStatus *atomic.Int32
	cloudCalls  *atomic.Int32
	localCalls  *atomic.Int32
	providers   map[string]provider.Provider
}

func newHarness(t *testing.T, piiMode string, rpm int) *harness {
	return newHarnessWith(t, piiMode, rpm, nil)
}

func newHarnessWith(t *testing.T, piiMode string, rpm int, tweak func(*config.Config)) *harness {
	t.Helper()
	h := &harness{cloudStatus: &atomic.Int32{}, cloudCalls: &atomic.Int32{}, localCalls: &atomic.Int32{}}
	cloud := fakeOpenAI(t, h.cloudStatus, h.cloudCalls)
	local := fakeOllama(t, h.localCalls)

	cfg := &config.Config{
		APIKeys: []config.APIKey{{Name: "test", Key: "secret", RPM: rpm}},
		Providers: []config.Provider{
			{Name: "cloud", Type: "openai", BaseURL: cloud.URL, TimeoutSeconds: 5},
			{Name: "local", Type: "ollama", BaseURL: local.URL, Local: true, TimeoutSeconds: 5},
		},
		Models: map[string][]config.Target{
			"chat": {{Provider: "cloud", Model: "gpt-4o-mini"}, {Provider: "local", Model: "llama3.1"}},
		},
		PII:     config.PII{Mode: piiMode},
		Cache:   config.Cache{Backend: "memory", TTLSeconds: 60},
		Pricing: map[string]config.ModelPricing{"gpt-4o-mini": {InputPer1K: 0.15, OutputPer1K: 0.6}},
		Limits:  config.Limits{MaxInFlight: 10, BreakerThreshold: 2, BreakerCooldownSeconds: 60},
	}
	if tweak != nil {
		tweak(cfg)
	}
	providers := map[string]provider.Provider{}
	for _, pc := range cfg.Providers {
		p, err := provider.New(pc)
		if err != nil {
			t.Fatal(err)
		}
		providers[pc.Name] = p
	}
	srv, err := New(Options{
		Config: cfg, Providers: providers, Cache: cache.NewMemory(100),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.server, h.handler = srv, srv.Handler()
	h.providers = providers
	return h
}

func (h *harness) chat(t *testing.T, content string) (*httptest.ResponseRecorder, api.ChatResponse) {
	return h.send(t, content, false)
}

func (h *harness) send(t *testing.T, content string, stream bool) (*httptest.ResponseRecorder, api.ChatResponse) {
	t.Helper()
	body, _ := json.Marshal(api.ChatRequest{Model: "chat", Stream: stream, Messages: []api.Message{{Role: "user", Content: content}}})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var resp api.ChatResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

func TestRoutesToFirstProvider(t *testing.T) {
	h := newHarness(t, "route_local", 0)
	rec, resp := h.chat(t, "explain goroutines")
	if rec.Code != 200 || resp.Choices[0].Message.Content != "from cloud" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Gateway-Provider") != "cloud" {
		t.Errorf("provider header = %q", rec.Header().Get("X-Gateway-Provider"))
	}
	// 1000 in * 0.15/1k + 500 out * 0.6/1k = 0.45
	cost := h.server.Metrics().Value("llm_gateway_cost_usd_total", metrics.Labels{"provider": "cloud", "model": "gpt-4o-mini", "key": "test"})
	if cost < 0.449 || cost > 0.451 {
		t.Errorf("cost = %v, want 0.45", cost)
	}
}

func TestFallsBackWhenCloudFails(t *testing.T) {
	h := newHarness(t, "route_local", 0)
	h.cloudStatus.Store(503)
	rec, resp := h.chat(t, "hello")
	if rec.Code != 200 || resp.Choices[0].Message.Content != "from local" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if v := h.server.Metrics().Value("llm_gateway_fallbacks_total", metrics.Labels{"from": "cloud"}); v != 1 {
		t.Errorf("fallbacks = %v", v)
	}
}

func TestBadRequestDoesNotFallBack(t *testing.T) {
	h := newHarness(t, "route_local", 0)
	h.cloudStatus.Store(400)
	rec, _ := h.chat(t, "hello")
	if rec.Code != 400 || h.localCalls.Load() != 0 {
		t.Fatalf("a 400 should be returned as-is, got %d and %d local calls", rec.Code, h.localCalls.Load())
	}
}

func TestPersonalDataStaysLocal(t *testing.T) {
	h := newHarness(t, "route_local", 0)
	rec, resp := h.chat(t, "draft a reply to sarah@example.com")
	if rec.Code != 200 || resp.Choices[0].Message.Content != "from local" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if h.cloudCalls.Load() != 0 {
		t.Error("personal data must never reach the cloud provider")
	}
	if rec.Header().Get("X-Gateway-PII") != "email" {
		t.Errorf("pii header = %q", rec.Header().Get("X-Gateway-PII"))
	}
}

func TestBlockMode(t *testing.T) {
	h := newHarness(t, "block", 0)
	rec, _ := h.chat(t, "my card is 4111 1111 1111 1111")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestCacheHit(t *testing.T) {
	h := newHarness(t, "route_local", 0)
	h.chat(t, "what is a monad")
	rec, resp := h.chat(t, "what is a monad")
	if rec.Header().Get("X-Cache") != "HIT" || resp.Choices[0].Message.Content != "from cloud" {
		t.Fatalf("expected a cache hit, headers %v", rec.Header())
	}
	if h.cloudCalls.Load() != 1 {
		t.Errorf("cloud called %d times, want 1", h.cloudCalls.Load())
	}
}

func TestAuthAndRateLimit(t *testing.T) {
	h := newHarness(t, "route_local", 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad key: got %d", rec.Code)
	}

	h.chat(t, "one")
	rec, _ = h.chat(t, "two")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("second request: got %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := newHarness(t, "route_local", 0)
	h.chat(t, "hi")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{"llm_gateway_requests_total", "llm_gateway_request_duration_seconds_bucket"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

// streamText joins the content of every SSE chunk and checks the stream ends with [DONE].
func streamText(t *testing.T, body string) string {
	t.Helper()
	var text strings.Builder
	done := false
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var c api.ChatChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		text.WriteString(c.Choices[0].Delta.Content)
	}
	if !done {
		t.Error("stream did not end with [DONE]")
	}
	return text.String()
}

func TestStreaming(t *testing.T) {
	h := newHarness(t, "route_local", 0)
	rec, _ := h.send(t, "hello", true)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("got %d %v", rec.Code, rec.Header())
	}
	if got := streamText(t, rec.Body.String()); got != "from cloud" {
		t.Errorf("text = %q", got)
	}
	tokens := h.server.Metrics().Value("llm_gateway_tokens_total", metrics.Labels{"provider": "cloud", "model": "gpt-4o-mini", "direction": "input"})
	if tokens != 10 {
		t.Errorf("streamed usage not recorded: %v", tokens)
	}
}

func TestStreamingFallsBackBeforeFirstChunk(t *testing.T) {
	h := newHarness(t, "route_local", 0)
	h.cloudStatus.Store(502)
	rec, _ := h.send(t, "hello", true)
	if got := streamText(t, rec.Body.String()); got != "from local" {
		t.Errorf("text = %q", got)
	}
}

func TestStreamingKeepsPersonalDataLocal(t *testing.T) {
	h := newHarness(t, "route_local", 0)
	rec, _ := h.send(t, "email tom@example.org", true)
	if streamText(t, rec.Body.String()) != "from local" || h.cloudCalls.Load() != 0 {
		t.Error("personal data reached the cloud while streaming")
	}
}

func TestRequestID(t *testing.T) {
	h := newHarness(t, "route_local", 0)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "abc123")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-ID") != "abc123" {
		t.Error("caller's request ID should be echoed back")
	}

	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if len(rec.Header().Get("X-Request-ID")) != 16 {
		t.Error("a request ID should be generated when none is sent")
	}
}

func TestCircuitBreakerSkipsDeadProvider(t *testing.T) {
	h := newHarness(t, "route_local", 0) // breaker opens after 2 failures
	h.cloudStatus.Store(503)
	h.chat(t, "one")
	h.chat(t, "two")
	before := h.cloudCalls.Load()

	rec, resp := h.chat(t, "three")
	if rec.Code != 200 || resp.Choices[0].Message.Content != "from local" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if h.cloudCalls.Load() != before {
		t.Error("an open breaker should stop calls to the failing provider")
	}
	if v := h.server.Metrics().Value("llm_gateway_breaker_open", metrics.Labels{"provider": "cloud"}); v != 1 {
		t.Errorf("breaker gauge = %v", v)
	}
}

func TestAllBreakersOpen(t *testing.T) {
	h := newHarnessWith(t, "route_local", 0, func(c *config.Config) {
		c.Models["chat"] = c.Models["chat"][:1] // cloud only
	})
	h.cloudStatus.Store(500)
	h.chat(t, "a")
	h.chat(t, "b")
	rec, _ := h.chat(t, "c")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("got %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestClientErrorsDoNotTripBreaker(t *testing.T) {
	h := newHarness(t, "route_local", 0)
	h.cloudStatus.Store(400)
	for i := 0; i < 5; i++ {
		h.chat(t, "bad")
	}
	h.cloudStatus.Store(0)
	rec, _ := h.chat(t, "good")
	if rec.Header().Get("X-Gateway-Provider") != "cloud" {
		t.Error("400s are the caller's fault and must not open the breaker")
	}
}

func TestMonthlyBudget(t *testing.T) {
	h := newHarnessWith(t, "route_local", 0, func(c *config.Config) {
		c.APIKeys[0].MonthlyBudgetUSD = 0.5 // one cloud call costs 0.45
	})
	h.chat(t, "first")
	h.chat(t, "second") // takes spend to 0.90
	rec, _ := h.chat(t, "third")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "insufficient_quota") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestLoadShedding(t *testing.T) {
	h := newHarnessWith(t, "route_local", 0, func(c *config.Config) { c.Limits.MaxInFlight = 1 })
	rt := h.server.current()
	rt.slots <- struct{}{} // occupy the only slot
	defer func() { <-rt.slots }()

	rec, _ := h.chat(t, "hello")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestReloadKeepsStateAndSwapsRoutes(t *testing.T) {
	h := newHarness(t, "route_local", 0)
	h.chat(t, "warm the cache")

	cfg := *h.server.current().cfg
	cfg.Models = map[string][]config.Target{"chat": {{Provider: "local", Model: "llama3.1"}}}
	if err := h.server.Reload(&cfg, h.providers); err != nil {
		t.Fatal(err)
	}
	_, resp := h.chat(t, "new question")
	if resp.Choices[0].Message.Content != "from local" {
		t.Error("new routes should apply after reload")
	}
	if h.server.Metrics().Value("llm_gateway_requests_total", metrics.Labels{"alias": "chat", "provider": "cloud", "status": "ok"}) != 1 {
		t.Error("metrics must survive a reload")
	}

	bad := cfg
	bad.Models = map[string][]config.Target{"chat": {{Provider: "ghost"}}}
	if err := h.server.Reload(&bad, h.providers); err == nil {
		t.Fatal("a bad config should be rejected")
	}
	if _, resp := h.chat(t, "still working"); resp.Choices[0].Message.Content != "from local" {
		t.Error("a failed reload must leave the previous config in place")
	}
}

func TestPanicRecovery(t *testing.T) {
	h := newHarness(t, "route_local", 0)
	handler := h.server.withRecover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 500 || h.server.Metrics().Value("llm_gateway_panics_total", nil) != 1 {
		t.Fatalf("got %d", rec.Code)
	}
}
