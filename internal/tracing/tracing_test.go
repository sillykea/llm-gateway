package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseTraceparent(t *testing.T) {
	const tid, pid = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	cases := []struct {
		header      string
		ok, sampled bool
	}{
		{"00-" + tid + "-" + pid + "-01", true, true},
		{"00-" + tid + "-" + pid + "-00", true, false},
		{"01-" + tid + "-" + pid + "-01-future", true, true}, // later versions may add fields
		{"00-" + tid + "-" + pid + "-01-extra", false, false},
		{"ff-" + tid + "-" + pid + "-01", false, false},
		{"00-" + strings.Repeat("0", 32) + "-" + pid + "-01", false, false},
		{"00-" + tid + "-" + strings.Repeat("0", 16) + "-01", false, false},
		{"00-" + tid[:30] + "-" + pid + "-01", false, false},
		{"00-" + strings.Replace(tid, "4", "x", 1) + "-" + pid + "-01", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		_, _, sampled, ok := ParseTraceparent(c.header)
		if ok != c.ok || sampled != c.sampled {
			t.Errorf("%q: ok=%v sampled=%v, want %v %v", c.header, ok, sampled, c.ok, c.sampled)
		}
	}
}

type collector struct {
	mu    sync.Mutex
	spans []map[string]any
	posts int
}

func newCollector(t *testing.T) (*collector, *httptest.Server) {
	c := &collector{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("collector got %s %s", r.URL.Path, r.Header.Get("Content-Type"))
		}
		var body struct {
			ResourceSpans []struct {
				Resource   struct{ Attributes []map[string]any }
				ScopeSpans []struct{ Spans []map[string]any }
			}
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("bad OTLP JSON: %v", err)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.posts++
		for _, rs := range body.ResourceSpans {
			if len(rs.Resource.Attributes) != 1 || rs.Resource.Attributes[0]["key"] != "service.name" {
				t.Errorf("resource attributes %v", rs.Resource.Attributes)
			}
			for _, ss := range rs.ScopeSpans {
				c.spans = append(c.spans, ss.Spans...)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

func attr(span map[string]any, key string) map[string]any {
	for _, a := range span["attributes"].([]any) {
		m := a.(map[string]any)
		if m["key"] == key {
			return m["value"].(map[string]any)
		}
	}
	return nil
}

func TestContinuesTraceAndExports(t *testing.T) {
	col, srv := newCollector(t)
	e := NewExporter(srv.URL, "test-gateway")
	e.Start()
	tr := New(e, 0) // ratio 0: only the caller's decision can sample this

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	ctx, server := tr.StartServer(req, "POST /v1/chat/completions")
	server.Set("http.response.status_code", 200)
	server.Set("gateway.cache_hit", false)
	cctx, client := Start(ctx, "chat llama3.1", Client)
	client.Set("gen_ai.usage.input_tokens", 12)
	client.Set("ratio", 0.5)
	client.Fail(errors.New("upstream 503"))

	h := http.Header{}
	Inject(cctx, h)
	if want := "00-4bf92f3577b34da6a3ce929d0e0e4736-" + client.spanIDHex() + "-01"; h.Get("traceparent") != want {
		t.Errorf("injected %q, want %q", h.Get("traceparent"), want)
	}
	client.End()
	client.End() // second End is a no-op
	server.End()
	ctxTimeout, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.Shutdown(ctxTimeout)

	col.mu.Lock()
	defer col.mu.Unlock()
	if len(col.spans) != 2 {
		t.Fatalf("exported %d spans, want 2", len(col.spans))
	}
	c, s := col.spans[0], col.spans[1]
	if s["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" || s["parentSpanId"] != "00f067aa0ba902b7" || s["kind"] != float64(2) {
		t.Errorf("server span %v", s)
	}
	if c["traceId"] != s["traceId"] || c["parentSpanId"] != s["spanId"] || c["kind"] != float64(3) {
		t.Errorf("client span is not a child of the server span: %v", c)
	}
	if c["status"].(map[string]any)["code"] != float64(2) || c["status"].(map[string]any)["message"] != "upstream 503" {
		t.Errorf("client status %v", c["status"])
	}
	if attr(c, "gen_ai.usage.input_tokens")["intValue"] != "12" || attr(c, "ratio")["doubleValue"] != 0.5 ||
		attr(s, "gateway.cache_hit")["boolValue"] != false {
		t.Errorf("attribute types wrong: %v / %v", c["attributes"], s["attributes"])
	}
	start, end := c["startTimeUnixNano"].(string), c["endTimeUnixNano"].(string)
	if len(start) < 19 || end < start {
		t.Errorf("times %s..%s", start, end)
	}
}

func (s *Span) spanIDHex() string { return s.Traceparent()[36:52] }

func TestSampling(t *testing.T) {
	col, srv := newCollector(t)
	e := NewExporter(srv.URL, "x")
	e.Start()

	never := New(e, 0)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	_, s := never.StartServer(req, "GET /v1/models")
	s.Set("k", "v")
	s.End()
	if !strings.HasSuffix(s.Traceparent(), "-00") || len(s.TraceID()) != 32 {
		t.Errorf("unsampled span must still propagate, flagged 00: %s", s.Traceparent())
	}

	// The caller said "not sampled": respected even at ratio 1.
	always := New(e, 1)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	_, s2 := always.StartServer(req, "GET /v1/models")
	s2.End()

	req.Header.Del("traceparent")
	for range 5 {
		_, s3 := always.StartServer(req, "GET /v1/models")
		s3.End()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.Shutdown(ctx)
	if len(col.spans) != 5 {
		t.Errorf("exported %d spans, want only the 5 sampled ones", len(col.spans))
	}
}

func TestFullQueueDropsInsteadOfBlocking(t *testing.T) {
	e := NewExporter("http://127.0.0.1:1", "x")
	e.queue = make(chan finished, 2) // never started: nothing drains it
	dropped := 0
	e.OnDrop = func(n int) { dropped += n }
	tr := New(e, 1)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	for range 5 {
		_, s := tr.StartServer(req, "x")
		s.End()
	}
	if dropped != 3 {
		t.Errorf("dropped %d, want 3", dropped)
	}
}

func TestExportFailureIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	e := NewExporter(srv.URL, "x")
	var got error
	e.OnExport = func(_ int, err error) { got = err }
	e.Start()
	_, s := New(e, 1).StartServer(httptest.NewRequest("GET", "/v1/models", nil), "x")
	s.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.Shutdown(ctx)
	if got == nil || !strings.Contains(got.Error(), "503") {
		t.Errorf("export error %v", got)
	}
}

func TestNilIsSafe(t *testing.T) {
	var tr *Tracer
	req := httptest.NewRequest("GET", "/", nil)
	ctx, s := tr.StartServer(req, "x")
	s.Set("a", 1)
	s.Fail(errors.New("x"))
	s.End()
	_, child := Start(ctx, "y", Client)
	child.End()
	h := http.Header{}
	Inject(ctx, h)
	if s != nil || child != nil || h.Get("traceparent") != "" || s.TraceID() != "" {
		t.Error("tracing off must do nothing")
	}
}
