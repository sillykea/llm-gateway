// Package keysync pulls API keys from a control plane (the gateway console)
// so keys can be created and revoked without editing config or restarting.
//
// The control plane only ever hands out SHA-256 hashes of keys, never the
// keys themselves. The gateway hashes the presented bearer token and looks
// the hash up. A hash of a 256-bit random key can't be reversed, so a leak
// of the key list (or of this process's memory) doesn't leak usable keys.
//
// The syncer is fail-static: if the control plane is down or returns
// something invalid, the last good key set stays in force. A gateway that has
// never synced successfully reports not ready, so a fresh pod doesn't take
// traffic and answer 401 to every caller. See docs/adr/0006.
package keysync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sillykea/llm-gateway/internal/config"
	"github.com/sillykea/llm-gateway/internal/metrics"
)

// maxBody caps the key list response. Tens of thousands of keys fit easily.
const maxBody = 4 << 20

// Key is one entry in the control plane's response.
type Key struct {
	Name             string  `json:"name"`
	SHA256           string  `json:"sha256"`
	RPM              int     `json:"rpm"`
	MonthlyBudgetUSD float64 `json:"monthly_budget_usd"`
}

type response struct {
	Keys []Key `json:"keys"`
}

// Syncer is safe for concurrent use.
type Syncer struct {
	url      string
	token    string
	interval time.Duration
	client   *http.Client
	metrics  *metrics.Registry
	log      *slog.Logger
	reserved func() map[string]bool

	keys   atomic.Pointer[map[[32]byte]config.APIKey]
	etagMu sync.Mutex
	etag   string
}

// Options configures a Syncer.
type Options struct {
	URL      string
	Token    string
	Interval time.Duration
	Client   *http.Client
	Metrics  *metrics.Registry
	Logger   *slog.Logger
	// Reserved returns the names of keys defined in the config file. Remote
	// keys can't reuse them: rate limits and budgets are tracked by name, so
	// a clash would merge two keys' spend.
	Reserved func() map[string]bool
}

// New returns a syncer that has not synced yet.
func New(o Options) *Syncer {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if o.Interval <= 0 {
		o.Interval = 30 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Metrics == nil {
		o.Metrics = metrics.New()
	}
	if o.Reserved == nil {
		o.Reserved = func() map[string]bool { return nil }
	}
	return &Syncer{
		url: o.URL, token: o.Token, interval: o.Interval, client: o.Client,
		metrics: o.Metrics, log: o.Logger, reserved: o.Reserved,
	}
}

// UseMetrics reports into r instead of the syncer's own registry, so sync
// metrics appear on the gateway's /metrics. Call it before Run.
func (s *Syncer) UseMetrics(r *metrics.Registry) { s.metrics = r }

// Lookup finds the key whose hash matches token.
func (s *Syncer) Lookup(token string) (config.APIKey, bool) {
	m := s.keys.Load()
	if m == nil || token == "" {
		return config.APIKey{}, false
	}
	k, ok := (*m)[sha256.Sum256([]byte(token))]
	return k, ok
}

// Ping reports ready once at least one sync has succeeded.
func (s *Syncer) Ping(context.Context) error {
	if s.keys.Load() == nil {
		return errors.New("api keys not synced from the console yet")
	}
	return nil
}

// Run syncs immediately, then every interval, until ctx is cancelled.
// Failed syncs are retried sooner, with backoff capped at the interval.
func (s *Syncer) Run(ctx context.Context) {
	retry := time.Second
	for {
		wait := s.interval
		if err := s.Sync(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("key sync failed, keeping the last good keys", "error", err)
			wait, retry = retry, min(retry*2, s.interval)
		} else {
			retry = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Sync fetches the key list once. On any error the current keys stay.
func (s *Syncer) Sync(ctx context.Context) error {
	err := s.sync(ctx)
	switch {
	case errors.Is(err, errNotModified):
		s.metrics.Inc("llm_gateway_key_syncs_total", metrics.Labels{"result": "not_modified"})
		s.markSuccess()
		return nil
	case err != nil:
		s.metrics.Inc("llm_gateway_key_syncs_total", metrics.Labels{"result": "error"})
		return err
	}
	s.metrics.Inc("llm_gateway_key_syncs_total", metrics.Labels{"result": "ok"})
	s.markSuccess()
	return nil
}

func (s *Syncer) markSuccess() {
	s.metrics.Set("llm_gateway_key_sync_last_success_seconds", nil, float64(time.Now().Unix()))
}

var errNotModified = errors.New("not modified")

func (s *Syncer) sync(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/json")
	s.etagMu.Lock()
	etag := s.etag
	s.etagMu.Unlock()
	// Only send the ETag once we hold keys, or a 304 would leave us with none.
	if etag != "" && s.keys.Load() != nil {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return errNotModified
	default:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("key list: unexpected status %d", resp.StatusCode)
	}

	var body response
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxBody))
	if err := dec.Decode(&body); err != nil {
		return fmt.Errorf("key list: %w", err)
	}
	keys, err := s.build(body.Keys)
	if err != nil {
		return err
	}
	s.keys.Store(&keys)
	s.metrics.Set("llm_gateway_remote_keys", nil, float64(len(keys)))

	s.etagMu.Lock()
	s.etag = resp.Header.Get("ETag")
	s.etagMu.Unlock()
	return nil
}

// build validates the whole list before anything is swapped in, so a
// half-broken response can't revoke keys that should still work.
func (s *Syncer) build(in []Key) (map[[32]byte]config.APIKey, error) {
	reserved := s.reserved()
	out := make(map[[32]byte]config.APIKey, len(in))
	names := make(map[string]bool, len(in))
	for _, k := range in {
		if k.Name == "" {
			return nil, errors.New("key list: key with no name")
		}
		if names[k.Name] {
			return nil, fmt.Errorf("key list: duplicate name %q", k.Name)
		}
		names[k.Name] = true
		if k.RPM < 0 || k.MonthlyBudgetUSD < 0 {
			return nil, fmt.Errorf("key list: %q has a negative limit", k.Name)
		}
		raw, err := hex.DecodeString(k.SHA256)
		if err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("key list: %q has an invalid sha256", k.Name)
		}
		if reserved[k.Name] {
			// Not fatal: one bad entry shouldn't block every other change.
			s.log.Warn("remote key name clashes with a config file key, skipping it", "name", k.Name)
			continue
		}
		var h [32]byte
		copy(h[:], raw)
		out[h] = config.APIKey{Name: k.Name, RPM: k.RPM, MonthlyBudgetUSD: k.MonthlyBudgetUSD}
	}
	return out, nil
}
