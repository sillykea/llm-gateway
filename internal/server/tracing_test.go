package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sillykea/llm-gateway/internal/api"
	"github.com/sillykea/llm-gateway/internal/config"
	"github.com/sillykea/llm-gateway/internal/provider"
	"github.com/sillykea/llm-gateway/internal/tracing"
)

// A request that falls back from a failing provider produces one server span
// and one client span per attempt, all in the caller's trace, and each
// provider receives traceparent naming its own span.
func TestTracingFollowsAFallback(t *testing.T) {
	var mu sync.Mutex
	var spans []map[string]any
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ResourceSpans []struct {
				ScopeSpans []struct{ Spans []map[string]any }
			}
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		for _, rs := range body.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
	}))
	defer collector.Close()

	upstream := func(status int, seen *string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*seen = r.Header.Get("traceparent")
			if status != 200 {
				w.WriteHeader(status)
				return
			}
			json.NewEncoder(w).Encode(api.ChatResponse{Object: "chat.completion",
				Choices: []api.Choice{{Message: api.Message{Role: "assistant", Content: "hi"}}},
				Usage:   api.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}})
		}))
		t.Cleanup(s.Close)
		return s
	}
	var seenA, seenB string
	a, b := upstream(503, &seenA), upstream(200, &seenB)
	cfg := &config.Config{
		APIKeys: []config.APIKey{{Name: "team", Key: "secret"}},
		Providers: []config.Provider{
			{Name: "a", Type: "openai", BaseURL: a.URL, TimeoutSeconds: 5},
			{Name: "b", Type: "openai", BaseURL: b.URL, TimeoutSeconds: 5},
		},
		Models: map[string][]config.Target{"chat": {{Provider: "a", Model: "m-a"}, {Provider: "b", Model: "m-b"}}},
		PII:    config.PII{Mode: "off"}, Cache: config.Cache{Backend: "off"},
		Limits: config.Limits{MaxInFlight: 4, BreakerThreshold: 5, BreakerCooldownSeconds: 30},
	}
	providers := map[string]provider.Provider{}
	for _, pc := range cfg.Providers {
		p, _ := provider.New(pc)
		providers[pc.Name] = p
	}
	exp := tracing.NewExporter(collector.URL, "llm-gateway")
	exp.Start()
	srv, err := New(Options{Config: cfg, Providers: providers, Tracer: tracing.New(exp, 1),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(api.ChatRequest{Model: "chat", Messages: []api.Message{{Role: "user", Content: "hello"}}})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	const trace = "4bf92f3577b34da6a3ce929d0e0e4736"
	req.Header.Set("traceparent", "00-"+trace+"-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exp.Shutdown(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want server + 2 attempts", len(spans))
	}
	byName := map[string]map[string]any{}
	for _, s := range spans {
		if s["traceId"] != trace {
			t.Errorf("span %s is in trace %s", s["name"], s["traceId"])
		}
		byName[s["name"].(string)] = s
	}
	server, failed, ok := byName["POST /v1/chat/completions"], byName["chat m-a"], byName["chat m-b"]
	if server == nil || failed == nil || ok == nil {
		t.Fatalf("span names: %v", byName)
	}
	if failed["parentSpanId"] != server["spanId"] || ok["parentSpanId"] != server["spanId"] {
		t.Error("attempts are not children of the request span")
	}
	if failed["status"].(map[string]any)["code"] != float64(2) {
		t.Error("the failed attempt is not marked as an error")
	}
	if !strings.Contains(seenA, failed["spanId"].(string)) || !strings.Contains(seenB, ok["spanId"].(string)) {
		t.Errorf("providers got traceparent %q and %q", seenA, seenB)
	}
	want := map[string]string{"gen_ai.usage.output_tokens": "3", "gateway.attempt": "2"}
	for _, a := range ok["attributes"].([]any) {
		m := a.(map[string]any)
		if v, found := want[m["key"].(string)]; found && m["value"].(map[string]any)["intValue"] != v {
			t.Errorf("%s = %v", m["key"], m["value"])
		}
	}
}
