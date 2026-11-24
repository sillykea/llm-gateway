package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sillykea/llm-gateway/internal/api"
	"github.com/sillykea/llm-gateway/internal/breaker"
	"github.com/sillykea/llm-gateway/internal/cache"
	"github.com/sillykea/llm-gateway/internal/metrics"
	"github.com/sillykea/llm-gateway/internal/pii"
	"github.com/sillykea/llm-gateway/internal/provider"
	"github.com/sillykea/llm-gateway/internal/router"
)

// call is one chat request on its way through the pipeline.
type call struct {
	rt        *runtime
	req       api.ChatRequest
	key       string
	piiTypes  string
	localOnly bool
	targets   []router.Target
}

// handleChat runs the pipeline: admit -> validate -> scan for personal data
// -> route -> cache -> call providers with fallback.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	rt := s.current()

	// Load shedding: if the gateway is full, say so now. Queueing would only
	// make every request slow, and clients would time out and retry anyway.
	select {
	case rt.slots <- struct{}{}:
		defer func() { <-rt.slots }()
	default:
		s.metrics.Inc("llm_gateway_load_shed_total", nil)
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "overloaded", "gateway is at capacity, retry shortly")
		return
	}
	s.metrics.AddGauge("llm_gateway_in_flight", nil, 1)
	defer s.metrics.AddGauge("llm_gateway_in_flight", nil, -1)

	c := &call{rt: rt}
	c.key, _ = r.Context().Value(keyCtx{}).(string)

	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&c.req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON")
		return
	}
	if c.req.Model == "" || len(c.req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "model and messages are required")
		return
	}

	if !s.applyPII(w, c) {
		return
	}

	targets, ok := rt.router.Resolve(c.req.Model, c.localOnly)
	if !ok {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not configured", c.req.Model))
		return
	}
	if len(targets) == 0 {
		// Fail closed: never fall back to a hosted provider with personal data.
		writeError(w, http.StatusUnprocessableEntity, "no_local_provider",
			"request contains personal data and no local provider is configured for this model")
		return
	}
	c.targets = targets

	if c.req.Stream {
		s.streamChat(w, r, c)
		return
	}
	s.completeChat(w, r, c)
}

// applyPII scans the conversation and applies the configured policy.
// It returns false if it has already written a response.
func (s *Server) applyPII(w http.ResponseWriter, c *call) bool {
	c.piiTypes = "none"
	mode := c.rt.cfg.PII.Mode
	if mode == "off" {
		w.Header().Set("X-Gateway-PII", c.piiTypes)
		return true
	}

	var all strings.Builder
	for _, m := range c.req.Messages {
		all.WriteString(m.Content)
		all.WriteString("\n")
	}
	found := pii.Detect(all.String())
	if len(found) > 0 {
		names := make([]string, len(found))
		for i, t := range found {
			names[i] = string(t)
			s.metrics.Inc("llm_gateway_pii_detections_total", metrics.Labels{"type": string(t)})
		}
		c.piiTypes = strings.Join(names, ",")

		switch mode {
		case "block":
			writeError(w, http.StatusUnprocessableEntity, "pii_detected", "request contains personal data: "+c.piiTypes)
			return false
		case "redact":
			for i := range c.req.Messages {
				c.req.Messages[i].Content = pii.Redact(c.req.Messages[i].Content)
			}
		case "route_local":
			c.localOnly = true
		}
	}
	w.Header().Set("X-Gateway-PII", c.piiTypes)
	return true
}

// completeChat handles a normal (non-streamed) request.
func (s *Server) completeChat(w http.ResponseWriter, r *http.Request, c *call) {
	// The cache key includes the routing decision so a redacted or
	// local-only answer is never served for a request that wasn't.
	var cacheKey string
	if s.cache != nil && cache.Cacheable(c.req) {
		cacheKey = cache.Key(c.req) + fmt.Sprintf(":local=%t", c.localOnly)
		if data, hit := s.cache.Get(r.Context(), cacheKey); hit {
			s.metrics.Inc("llm_gateway_cache_hits_total", metrics.Labels{"model": c.req.Model})
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
			return
		}
		s.metrics.Inc("llm_gateway_cache_misses_total", metrics.Labels{"model": c.req.Model})
	}

	var resp *api.ChatResponse
	t, err := s.tryTargets(r, c, func(ctx context.Context, t router.Target) error {
		var err error
		resp, err = t.Provider.Chat(ctx, t.Model, c.req)
		if err == nil {
			s.recordUsage(c.key, t, resp.Usage)
		}
		return err
	})
	if err != nil {
		s.writeUpstreamError(w, c, err)
		return
	}

	data, _ := json.Marshal(resp)
	if cacheKey != "" {
		ttl := time.Duration(c.rt.cfg.Cache.TTLSeconds) * time.Second
		s.cache.Set(r.Context(), cacheKey, data, ttl)
		w.Header().Set("X-Cache", "MISS")
	}
	w.Header().Set("X-Gateway-Provider", t.Provider.Name())
	w.Header().Set("X-Gateway-Model", t.Model)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// streamChat relays a streamed answer as server-sent events.
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, c *call) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming not supported by this server")
		return
	}

	started := false
	t, err := s.tryTargets(r, c, func(ctx context.Context, t router.Target) error {
		emit := func(chunk api.ChatChunk) error {
			if !started {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("X-Gateway-Provider", t.Provider.Name())
				w.Header().Set("X-Gateway-Model", t.Model)
				w.WriteHeader(http.StatusOK)
				started = true
			}
			data, err := json.Marshal(chunk)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return err // client went away
			}
			flusher.Flush()
			return nil
		}
		usage, err := t.Provider.Stream(ctx, t.Model, c.req, emit)
		s.recordUsage(c.key, t, usage) // count partial streams too: they were paid for
		if err != nil && started {
			// Fallback is only possible before the first chunk. After that the
			// client has seen part of an answer, so mark the error as final.
			return finalError{err}
		}
		return err
	})

	switch {
	case err == nil:
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		s.log.Info("stream completed", "provider", t.Provider.Name(), "model", t.Model, "request_id", requestID(r))
	case started:
		// Headers are already sent, so report the failure inside the stream.
		msg, _ := json.Marshal(api.ErrorResponse{Error: api.ErrorBody{Message: "stream interrupted", Type: "upstream_error"}})
		fmt.Fprintf(w, "data: %s\n\n", msg)
		flusher.Flush()
	default:
		s.writeUpstreamError(w, c, err)
	}
}

// finalError stops tryTargets from falling back.
type finalError struct{ error }

func (e finalError) Unwrap() error { return e.error }

// errAllUnavailable means every target was skipped by its circuit breaker.
var errAllUnavailable = errors.New("all providers are temporarily unavailable")

// tryTargets calls do for each target in order until one succeeds, skipping
// providers whose breaker is open and falling back on retryable errors.
func (s *Server) tryTargets(r *http.Request, c *call, do func(context.Context, router.Target) error) (router.Target, error) {
	var lastErr error
	for i, t := range c.targets {
		name := t.Provider.Name()
		b := s.breaker(name)
		if !b.Allow() {
			s.metrics.Inc("llm_gateway_breaker_skips_total", metrics.Labels{"provider": name})
			continue
		}

		start := time.Now()
		err := do(r.Context(), t)
		elapsed := time.Since(start).Seconds()
		s.metrics.ObserveDuration(metrics.Labels{"provider": name, "model": t.Model}, elapsed)
		s.recordBreaker(b, name, err)

		status := "ok"
		if err != nil {
			status = "error"
		}
		s.metrics.Inc("llm_gateway_requests_total", metrics.Labels{"alias": c.req.Model, "provider": name, "status": status})

		if err == nil {
			s.log.Info("chat completed", "alias", c.req.Model, "provider", name, "model", t.Model,
				"seconds", elapsed, "pii", c.piiTypes, "key", c.key, "request_id", requestID(r))
			return t, nil
		}

		lastErr = err
		s.log.Warn("provider failed", "provider", name, "model", t.Model, "error", err, "request_id", requestID(r))
		var final finalError
		if errors.As(err, &final) || !provider.Retryable(err) {
			return t, err
		}
		if i < len(c.targets)-1 {
			s.metrics.Inc("llm_gateway_fallbacks_total", metrics.Labels{"from": name})
		}
	}
	if lastErr == nil {
		lastErr = errAllUnavailable
	}
	return router.Target{}, lastErr
}

// recordBreaker feeds a result into the provider's breaker. Only failures that
// are the provider's fault count: a 400 means our request was bad, and a
// cancelled context means the client left.
func (s *Server) recordBreaker(b *breaker.Breaker, name string, err error) {
	if err != nil && provider.Retryable(err) {
		b.Failure()
	} else {
		b.Success()
	}
	open := 0.0
	if b.State() != breaker.Closed {
		open = 1
	}
	s.metrics.Set("llm_gateway_breaker_open", metrics.Labels{"provider": name}, open)
}

func (s *Server) writeUpstreamError(w http.ResponseWriter, c *call, err error) {
	if errors.Is(err, errAllUnavailable) {
		w.Header().Set("Retry-After", fmt.Sprint(c.rt.cfg.Limits.BreakerCooldownSeconds))
		writeError(w, http.StatusServiceUnavailable, "providers_unavailable", err.Error())
		return
	}
	status := http.StatusBadGateway
	var ue *provider.UpstreamError
	if errors.As(err, &ue) && ue.Status < 500 && ue.Status != http.StatusTooManyRequests {
		status = ue.Status // pass client errors (e.g. 400) straight through
	}
	writeError(w, status, "upstream_error", "all providers failed: "+err.Error())
}

func (s *Server) recordUsage(key string, t router.Target, u api.Usage) {
	name := t.Provider.Name()
	s.metrics.Add("llm_gateway_tokens_total", metrics.Labels{"provider": name, "model": t.Model, "direction": "input"}, float64(u.PromptTokens))
	s.metrics.Add("llm_gateway_tokens_total", metrics.Labels{"provider": name, "model": t.Model, "direction": "output"}, float64(u.CompletionTokens))
	if p, ok := s.current().cfg.Pricing[t.Model]; ok {
		cost := float64(u.PromptTokens)/1000*p.InputPer1K + float64(u.CompletionTokens)/1000*p.OutputPer1K
		s.metrics.Add("llm_gateway_cost_usd_total", metrics.Labels{"provider": name, "model": t.Model, "key": key}, cost)
		s.budgets.Add(key, cost)
	}
}
