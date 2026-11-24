// Package server wires everything together behind the HTTP API.
//
// Config-dependent state (keys, routes, limits) lives in a runtime value
// that is swapped atomically on reload. State that must survive a reload
// (rate limit buckets, budgets, breakers, metrics, cache) lives on Server.
// Each request reads the runtime once at the start, so a reload never
// changes the rules half way through a request.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sillykea/llm-gateway/internal/api"
	"github.com/sillykea/llm-gateway/internal/breaker"
	"github.com/sillykea/llm-gateway/internal/budget"
	"github.com/sillykea/llm-gateway/internal/cache"
	"github.com/sillykea/llm-gateway/internal/config"
	"github.com/sillykea/llm-gateway/internal/metrics"
	"github.com/sillykea/llm-gateway/internal/provider"
	"github.com/sillykea/llm-gateway/internal/ratelimit"
	"github.com/sillykea/llm-gateway/internal/router"
)

// Pinger is implemented by dependencies the readiness probe should check.
type Pinger interface {
	Ping(ctx context.Context) error
}

// runtime is everything derived from one version of the config.
type runtime struct {
	cfg    *config.Config
	router *router.Router
	slots  chan struct{} // semaphore for max in-flight requests
}

// Server handles gateway HTTP traffic.
type Server struct {
	rt atomic.Pointer[runtime]

	cache   cache.Cache // nil when caching is off
	limiter *ratelimit.Limiter
	budgets *budget.Tracker
	metrics *metrics.Registry
	log     *slog.Logger
	ready   []Pinger

	breakersMu sync.Mutex
	breakers   map[string]*breaker.Breaker
}

// Options lets callers (main and the tests) inject dependencies.
type Options struct {
	Config    *config.Config
	Providers map[string]provider.Provider
	Cache     cache.Cache
	Logger    *slog.Logger
	Ready     []Pinger
}

// New builds a server from options.
func New(o Options) (*Server, error) {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cache:    o.Cache,
		limiter:  ratelimit.New(),
		budgets:  budget.New(),
		metrics:  metrics.New(),
		log:      log,
		ready:    o.Ready,
		breakers: map[string]*breaker.Breaker{},
	}
	if err := s.Reload(o.Config, o.Providers); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload swaps in a new config. In-flight requests finish under the old one.
// On error the current config stays in place.
func (s *Server) Reload(cfg *config.Config, providers map[string]provider.Provider) error {
	r, err := router.New(cfg.Models, providers)
	if err != nil {
		s.metrics.Inc("llm_gateway_config_reloads_total", metrics.Labels{"result": "error"})
		return err
	}
	s.rt.Store(&runtime{cfg: cfg, router: r, slots: make(chan struct{}, cfg.Limits.MaxInFlight)})
	s.metrics.Inc("llm_gateway_config_reloads_total", metrics.Labels{"result": "ok"})
	return nil
}

// ReloadFailed records a reload that failed before reaching Reload, such as
// a config file that doesn't parse, so the alert on failed reloads sees it.
func (s *Server) ReloadFailed() {
	s.metrics.Inc("llm_gateway_config_reloads_total", metrics.Labels{"result": "error"})
}

func (s *Server) current() *runtime { return s.rt.Load() }

// Metrics exposes the registry (used in tests).
func (s *Server) Metrics() *metrics.Registry { return s.metrics }

// breaker returns the circuit breaker for a provider, creating it on first use.
// Breakers are keyed by provider name so they survive config reloads.
func (s *Server) breaker(name string) *breaker.Breaker {
	s.breakersMu.Lock()
	defer s.breakersMu.Unlock()
	b, ok := s.breakers[name]
	if !ok {
		lim := s.current().cfg.Limits
		b = breaker.New(lim.BreakerThreshold, time.Duration(lim.BreakerCooldownSeconds)*time.Second)
		s.breakers[name] = b
	}
	return b
}

// Handler returns the HTTP routes wrapped in the middleware chain.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.withAuth(s.handleChat))
	mux.HandleFunc("GET /v1/models", s.withAuth(s.handleModels))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.metrics.Write(w)
	})
	// Outermost first: every request gets an ID, panics are caught and
	// everything is access-logged with its final status.
	return withRequestID(s.withAccessLog(s.withRecover(mux)))
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list"}
	for _, a := range s.current().router.Aliases() {
		out.Data = append(out.Data, model{ID: a, Object: "model", OwnedBy: "gateway"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	for _, p := range s.ready {
		if err := p.Ping(ctx); err != nil {
			http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.Write([]byte("ready"))
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(api.ErrorResponse{Error: api.ErrorBody{Message: msg, Type: typ}})
}
