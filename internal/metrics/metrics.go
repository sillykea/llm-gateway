// Package metrics exposes counters and histograms in the Prometheus text
// format. It is small on purpose: the gateway only needs labelled counters
// and one latency histogram, so pulling in the full client library is not
// worth the extra dependency.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

var buckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// Registry holds every metric the gateway reports.
type Registry struct {
	mu         sync.Mutex
	counters   map[string]map[string]float64 // name -> labels -> value
	gauges     map[string]map[string]float64
	help       map[string]string
	histCounts map[string][]uint64 // labels -> per-bucket counts
	histSum    map[string]float64
	histTotal  map[string]uint64
}

// New creates a registry with help text for each metric.
func New() *Registry {
	return &Registry{
		counters: map[string]map[string]float64{},
		gauges:   map[string]map[string]float64{},
		help: map[string]string{
			"llm_gateway_requests_total":            "Chat requests by model alias, provider and status.",
			"llm_gateway_tokens_total":              "Tokens processed by provider, model and direction.",
			"llm_gateway_cost_usd_total":            "Estimated spend in USD by provider and model.",
			"llm_gateway_cache_hits_total":          "Responses served from cache.",
			"llm_gateway_cache_misses_total":        "Cacheable requests not found in cache.",
			"llm_gateway_fallbacks_total":           "Times a request moved on to the next provider.",
			"llm_gateway_pii_detections_total":      "Requests containing personal data, by type.",
			"llm_gateway_rate_limited_total":        "Requests rejected by the rate limiter, by key.",
			"llm_gateway_budget_exceeded_total":     "Requests rejected because a key used its monthly budget.",
			"llm_gateway_load_shed_total":           "Requests rejected because the gateway was at capacity.",
			"llm_gateway_breaker_skips_total":       "Targets skipped because their circuit breaker was open.",
			"llm_gateway_config_reloads_total":      "Config reload attempts, by result.",
			"llm_gateway_panics_total":              "Handler panics recovered.",
			"llm_gateway_shared_state_errors_total": "Redis failures for shared rate limits or budgets; the replica's own count was used instead.",
			"llm_gateway_in_flight":                 "Chat requests currently being handled.",
			"llm_gateway_breaker_open":              "1 if the provider's circuit breaker is open or half-open.",
		},
		histCounts: map[string][]uint64{},
		histSum:    map[string]float64{},
		histTotal:  map[string]uint64{},
	}
}

// Labels is a convenience for building label sets.
type Labels map[string]string

func (l Labels) String() string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(l[k])
		parts[i] = fmt.Sprintf(`%s="%s"`, k, v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Add increases a counter.
func (r *Registry) Add(name string, labels Labels, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counters[name] == nil {
		r.counters[name] = map[string]float64{}
	}
	r.counters[name][labels.String()] += v
}

// Inc increases a counter by one.
func (r *Registry) Inc(name string, labels Labels) { r.Add(name, labels, 1) }

// Set sets a gauge.
func (r *Registry) Set(name string, labels Labels, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gauges[name] == nil {
		r.gauges[name] = map[string]float64{}
	}
	r.gauges[name][labels.String()] = v
}

// AddGauge moves a gauge up or down.
func (r *Registry) AddGauge(name string, labels Labels, delta float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gauges[name] == nil {
		r.gauges[name] = map[string]float64{}
	}
	r.gauges[name][labels.String()] += delta
}

// Value returns a counter's current value (handy in tests).
func (r *Registry) Value(name string, labels Labels) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.counters[name][labels.String()]; ok {
		return v
	}
	return r.gauges[name][labels.String()]
}

// ObserveDuration records a request latency in seconds.
func (r *Registry) ObserveDuration(labels Labels, seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := labels.String()
	if r.histCounts[key] == nil {
		r.histCounts[key] = make([]uint64, len(buckets))
	}
	for i, b := range buckets {
		if seconds <= b {
			r.histCounts[key][i]++
		}
	}
	r.histSum[key] += seconds
	r.histTotal[key]++
}

// Write renders all metrics in Prometheus text format.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.writeFamily(w, r.counters, "counter")
	r.writeFamily(w, r.gauges, "gauge")

	const h = "llm_gateway_request_duration_seconds"
	fmt.Fprintf(w, "# HELP %s Upstream request latency.\n# TYPE %s histogram\n", h, h)
	keys := make([]string, 0, len(r.histCounts))
	for k := range r.histCounts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		inner := strings.TrimSuffix(strings.TrimPrefix(k, "{"), "}")
		sep := ""
		if inner != "" {
			sep = ","
		}
		for i, b := range buckets {
			fmt.Fprintf(w, "%s_bucket{%s%sle=\"%g\"} %d\n", h, inner, sep, b, r.histCounts[k][i])
		}
		fmt.Fprintf(w, "%s_bucket{%s%sle=\"+Inf\"} %d\n", h, inner, sep, r.histTotal[k])
		fmt.Fprintf(w, "%s_sum%s %g\n%s_count%s %d\n", h, k, r.histSum[k], h, k, r.histTotal[k])
	}
}

func (r *Registry) writeFamily(w io.Writer, family map[string]map[string]float64, typ string) {
	names := make([]string, 0, len(family))
	for n := range family {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", n, r.help[n], n, typ)
		series := make([]string, 0, len(family[n]))
		for l := range family[n] {
			series = append(series, l)
		}
		sort.Strings(series)
		for _, l := range series {
			fmt.Fprintf(w, "%s%s %g\n", n, l, family[n][l])
		}
	}
}
