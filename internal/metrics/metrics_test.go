package metrics

import (
	"strings"
	"testing"
)

func TestWrite(t *testing.T) {
	r := New()
	r.Inc("llm_gateway_requests_total", Labels{"provider": "ollama", "status": "ok"})
	r.Inc("llm_gateway_requests_total", Labels{"status": "ok", "provider": "ollama"}) // same series, any order
	r.ObserveDuration(Labels{"provider": "ollama"}, 0.3)

	var b strings.Builder
	r.Write(&b)
	out := b.String()

	for _, want := range []string{
		"# TYPE llm_gateway_requests_total counter",
		`llm_gateway_requests_total{provider="ollama",status="ok"} 2`,
		`llm_gateway_request_duration_seconds_bucket{provider="ollama",le="0.25"} 0`,
		`llm_gateway_request_duration_seconds_bucket{provider="ollama",le="0.5"} 1`,
		`llm_gateway_request_duration_seconds_count{provider="ollama"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestLabelEscaping(t *testing.T) {
	got := Labels{"model": `a"b`}.String()
	if got != `{model="a\"b"}` {
		t.Errorf("got %s", got)
	}
}
