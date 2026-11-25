// Package tracing records request spans and exports them to an OpenTelemetry
// collector over OTLP/HTTP (JSON), using only the standard library (see
// docs/adr/0001).
//
// It does three things:
//
//   - continues the caller's trace from a W3C traceparent header, or starts one;
//   - passes traceparent on to providers, so a trace runs through the gateway
//     into vLLM, Ollama or anything else that understands it;
//   - batches finished spans and posts them to <endpoint>/v1/traces.
//
// A nil *Tracer and a nil *Span are valid and do nothing, so code paths don't
// need to check whether tracing is on.
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Kind is the OTLP span kind.
type Kind int

const (
	Internal Kind = 1
	Server   Kind = 2
	Client   Kind = 3
)

type (
	TraceID [16]byte
	SpanID  [8]byte
)

// Tracer starts spans and hands finished ones to the exporter.
type Tracer struct {
	exporter *Exporter
	// ratio of new traces (those without a sampled parent) to record.
	ratio float64
}

// New returns a tracer. ratio is the share of new traces to record, 0 to 1.
func New(e *Exporter, ratio float64) *Tracer {
	return &Tracer{exporter: e, ratio: ratio}
}

// Span is one timed operation. Safe for concurrent use.
type Span struct {
	tracer  *Tracer
	traceID TraceID
	spanID  SpanID
	parent  SpanID
	sampled bool
	name    string
	kind    Kind
	start   time.Time

	mu     sync.Mutex
	attrs  map[string]any
	errMsg string
	ended  bool
}

type spanKey struct{}

// FromContext returns the current span, or nil.
func FromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(spanKey{}).(*Span)
	return s
}

func randomBytes(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand doesn't fail on supported platforms
	}
}

// ParseTraceparent reads a W3C traceparent header
// (version-traceid-parentid-flags). ok is false for anything malformed or
// all-zero, in which case a new trace should start.
func ParseTraceparent(h string) (trace TraceID, parent SpanID, sampled, ok bool) {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) < 4 || len(parts[0]) != 2 || parts[0] == "ff" ||
		len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return trace, parent, false, false
	}
	if parts[0] == "00" && len(parts) != 4 {
		return trace, parent, false, false
	}
	t, err1 := hex.DecodeString(parts[1])
	p, err2 := hex.DecodeString(parts[2])
	f, err3 := hex.DecodeString(parts[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return trace, parent, false, false
	}
	copy(trace[:], t)
	copy(parent[:], p)
	if trace == (TraceID{}) || parent == (SpanID{}) {
		return trace, parent, false, false
	}
	return trace, parent, f[0]&1 == 1, true
}

// StartServer starts the span for an incoming request, continuing the
// caller's trace if it sent a valid traceparent.
func (t *Tracer) StartServer(r *http.Request, name string) (context.Context, *Span) {
	if t == nil {
		return r.Context(), nil
	}
	s := &Span{tracer: t, name: name, kind: Server, start: time.Now()}
	if trace, parent, sampled, ok := ParseTraceparent(r.Header.Get("traceparent")); ok {
		s.traceID, s.parent, s.sampled = trace, parent, sampled
	} else {
		randomBytes(s.traceID[:])
		// Sample on the trace ID, so every service making the same choice
		// from the same ID agrees.
		s.sampled = float64(binary.BigEndian.Uint64(s.traceID[8:])>>11)/(1<<53) < t.ratio
	}
	randomBytes(s.spanID[:])
	return context.WithValue(r.Context(), spanKey{}, s), s
}

// Start begins a child of the span in ctx. With no span in ctx it returns
// ctx and nil.
func Start(ctx context.Context, name string, kind Kind) (context.Context, *Span) {
	p := FromContext(ctx)
	if p == nil {
		return ctx, nil
	}
	s := &Span{tracer: p.tracer, traceID: p.traceID, parent: p.spanID, sampled: p.sampled,
		name: name, kind: kind, start: time.Now()}
	randomBytes(s.spanID[:])
	return context.WithValue(ctx, spanKey{}, s), s
}

// Traceparent is the header value that makes s the parent of a downstream span.
func (s *Span) Traceparent() string {
	if s == nil {
		return ""
	}
	flags := "00"
	if s.sampled {
		flags = "01"
	}
	return "00-" + hex.EncodeToString(s.traceID[:]) + "-" + hex.EncodeToString(s.spanID[:]) + "-" + flags
}

// TraceID is the trace's ID in hex, for logs. Empty for a nil span.
func (s *Span) TraceID() string {
	if s == nil {
		return ""
	}
	return hex.EncodeToString(s.traceID[:])
}

// Inject adds traceparent for the span in ctx to an outgoing request.
func Inject(ctx context.Context, h http.Header) {
	if s := FromContext(ctx); s != nil {
		h.Set("traceparent", s.Traceparent())
	}
}

// Set records an attribute. Values should be string, bool, int, int64 or float64.
func (s *Span) Set(key string, value any) {
	if s == nil || !s.sampled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attrs == nil {
		s.attrs = map[string]any{}
	}
	s.attrs[key] = value
}

// Fail marks the span as failed.
func (s *Span) Fail(err error) {
	if s == nil || err == nil || !s.sampled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errMsg = err.Error()
}

// End finishes the span and queues it for export. Calling End twice is harmless.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.mu.Unlock()
	if s.sampled && s.tracer.exporter != nil {
		s.tracer.exporter.enqueue(s, time.Now())
	}
}
