package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

type finished struct {
	span *Span
	end  time.Time
}

// Exporter batches spans and posts them as OTLP/HTTP JSON. Export never
// blocks a request: when the queue is full, spans are dropped and counted.
type Exporter struct {
	url     string
	service string
	client  *http.Client
	queue   chan finished
	batch   int
	every   time.Duration

	// OnExport is called after each POST with how many spans were sent and
	// whether the collector took them. OnDrop is called with spans dropped
	// because the queue was full. Set both before Start.
	OnExport func(n int, err error)
	OnDrop   func(n int)

	done chan struct{}
	stop chan chan struct{}
	once sync.Once
}

// NewExporter posts to endpoint + "/v1/traces" (endpoint like
// http://otel-collector:4318).
func NewExporter(endpoint, service string) *Exporter {
	return &Exporter{
		url:     endpoint + "/v1/traces",
		service: service,
		client:  &http.Client{Timeout: 5 * time.Second},
		queue:   make(chan finished, 4096),
		batch:   512,
		every:   2 * time.Second,
		done:    make(chan struct{}),
		stop:    make(chan chan struct{}),
	}
}

func (e *Exporter) enqueue(s *Span, end time.Time) {
	select {
	case e.queue <- finished{s, end}:
	default:
		if e.OnDrop != nil {
			e.OnDrop(1)
		}
	}
}

// Start runs the export loop until Shutdown.
func (e *Exporter) Start() {
	go func() {
		defer close(e.done)
		t := time.NewTicker(e.every)
		defer t.Stop()
		var buf []finished
		flush := func() {
			if len(buf) > 0 {
				e.post(buf)
				buf = buf[:0]
			}
		}
		for {
			select {
			case f := <-e.queue:
				buf = append(buf, f)
				if len(buf) >= e.batch {
					flush()
				}
			case <-t.C:
				flush()
			case ack := <-e.stop:
			drain:
				for {
					select {
					case f := <-e.queue:
						buf = append(buf, f)
					default:
						break drain
					}
				}
				flush()
				close(ack)
				return
			}
		}
	}()
}

// Shutdown sends what is queued and stops, or gives up when ctx ends.
func (e *Exporter) Shutdown(ctx context.Context) {
	e.once.Do(func() {
		ack := make(chan struct{})
		select {
		case e.stop <- ack:
		case <-ctx.Done():
			return
		}
		select {
		case <-ack:
		case <-ctx.Done():
		}
	})
}

func (e *Exporter) post(spans []finished) {
	body, err := json.Marshal(e.payload(spans))
	if err == nil {
		var resp *http.Response
		resp, err = e.client.Post(e.url, "application/json", bytes.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) //nolint:errcheck
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				err = fmt.Errorf("collector answered %d", resp.StatusCode)
			}
		}
	}
	if e.OnExport != nil {
		e.OnExport(len(spans), err)
	}
}

// OTLP JSON. IDs are hex and nanosecond times are decimal strings, as the
// OTLP/HTTP JSON mapping requires.
type (
	otlpValue struct {
		StringValue *string  `json:"stringValue,omitempty"`
		BoolValue   *bool    `json:"boolValue,omitempty"`
		IntValue    *string  `json:"intValue,omitempty"`
		DoubleValue *float64 `json:"doubleValue,omitempty"`
	}
	otlpAttr struct {
		Key   string    `json:"key"`
		Value otlpValue `json:"value"`
	}
	otlpStatus struct {
		Code    int    `json:"code"`
		Message string `json:"message,omitempty"`
	}
	otlpSpan struct {
		TraceID           string     `json:"traceId"`
		SpanID            string     `json:"spanId"`
		ParentSpanID      string     `json:"parentSpanId,omitempty"`
		Name              string     `json:"name"`
		Kind              Kind       `json:"kind"`
		StartTimeUnixNano string     `json:"startTimeUnixNano"`
		EndTimeUnixNano   string     `json:"endTimeUnixNano"`
		Attributes        []otlpAttr `json:"attributes,omitempty"`
		Status            otlpStatus `json:"status"`
	}
)

func value(v any) otlpValue {
	switch x := v.(type) {
	case string:
		return otlpValue{StringValue: &x}
	case bool:
		return otlpValue{BoolValue: &x}
	case int:
		s := strconv.Itoa(x)
		return otlpValue{IntValue: &s}
	case int64:
		s := strconv.FormatInt(x, 10)
		return otlpValue{IntValue: &s}
	case float64:
		return otlpValue{DoubleValue: &x}
	default:
		s := fmt.Sprint(x)
		return otlpValue{StringValue: &s}
	}
}

func attrs(m map[string]any) []otlpAttr {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]otlpAttr, len(keys))
	for i, k := range keys {
		out[i] = otlpAttr{Key: k, Value: value(m[k])}
	}
	return out
}

func (e *Exporter) payload(spans []finished) any {
	out := make([]otlpSpan, len(spans))
	for i, f := range spans {
		s := f.span
		s.mu.Lock()
		o := otlpSpan{
			TraceID: hex.EncodeToString(s.traceID[:]), SpanID: hex.EncodeToString(s.spanID[:]),
			Name: s.name, Kind: s.kind,
			StartTimeUnixNano: strconv.FormatInt(s.start.UnixNano(), 10),
			EndTimeUnixNano:   strconv.FormatInt(f.end.UnixNano(), 10),
			Attributes:        attrs(s.attrs),
		}
		if s.parent != (SpanID{}) {
			o.ParentSpanID = hex.EncodeToString(s.parent[:])
		}
		if s.errMsg != "" {
			o.Status = otlpStatus{Code: 2, Message: s.errMsg}
		}
		s.mu.Unlock()
		out[i] = o
	}
	return map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": attrs(map[string]any{"service.name": e.service})},
		"scopeSpans": []any{map[string]any{
			"scope": map[string]any{"name": "github.com/sillykea/llm-gateway"},
			"spans": out,
		}},
	}}}
}
