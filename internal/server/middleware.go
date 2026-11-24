package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/sillykea/llm-gateway/internal/config"
	"github.com/sillykea/llm-gateway/internal/metrics"
)

type keyCtx struct{}
type requestIDKey struct{}

// withRequestID gives every request an ID (reusing X-Request-ID if the caller
// sent one), returns it in the response and adds it to log lines, so a single
// request can be followed through the logs.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 64 {
			b := make([]byte, 8)
			rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func requestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}

// statusRecorder remembers the status code for the access log. It must also
// pass Flush through, or wrapping the writer would silently break streaming.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			return // probes and scrapes would drown out real traffic
		}
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"ms", time.Since(start).Milliseconds(), "request_id", requestID(r))
	})
}

// withRecover turns a panic into a 500 for that one request instead of
// crashing the process and dropping every other in-flight request.
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v) // deliberate abort, let net/http handle it
				}
				s.metrics.Inc("llm_gateway_panics_total", nil)
				s.log.Error("panic", "value", fmt.Sprint(v), "stack", string(debug.Stack()), "request_id", requestID(r))
				writeError(w, http.StatusInternalServerError, "server_error", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withAuth checks the bearer token, then the key's rate limit and budget.
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rt := s.current()
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var key *config.APIKey
		for i := range rt.cfg.APIKeys {
			k := &rt.cfg.APIKeys[i]
			// Constant-time compare so response timing can't leak the key.
			if subtle.ConstantTimeCompare([]byte(token), []byte(k.Key)) == 1 {
				key = k
				break
			}
		}
		if key == nil {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		if ok, wait := s.limiter.Allow(key.Name, key.RPM); !ok {
			s.metrics.Inc("llm_gateway_rate_limited_total", metrics.Labels{"key": key.Name})
			w.Header().Set("Retry-After", fmt.Sprint(int(math.Ceil(wait.Seconds()))))
			writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "rate limit exceeded")
			return
		}
		if s.budgets.Exceeded(key.Name, key.MonthlyBudgetUSD) {
			s.metrics.Inc("llm_gateway_budget_exceeded_total", metrics.Labels{"key": key.Name})
			writeError(w, http.StatusTooManyRequests, "insufficient_quota",
				fmt.Sprintf("monthly budget of $%.2f reached", key.MonthlyBudgetUSD))
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), keyCtx{}, key.Name)))
	}
}
