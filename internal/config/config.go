// Package config loads the gateway configuration from a JSON file.
// Secrets (API keys) are never stored in the file: the file only names the
// environment variable that holds each secret.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Config is the full gateway configuration.
type Config struct {
	Listen    string                  `json:"listen"`
	APIKeys   []APIKey                `json:"api_keys"`
	Providers []Provider              `json:"providers"`
	Models    map[string][]Target     `json:"models"`
	PII       PII                     `json:"pii"`
	Cache     Cache                   `json:"cache"`
	Pricing   map[string]ModelPricing `json:"pricing"`
	Limits    Limits                  `json:"limits"`
}

// Limits protect the gateway and its upstreams under load.
type Limits struct {
	// MaxInFlight caps concurrent chat requests. Past it the gateway answers
	// 503 straight away rather than queueing work it can't finish in time.
	MaxInFlight int `json:"max_in_flight"`
	// BreakerThreshold is how many consecutive failures open a provider's
	// circuit breaker; BreakerCooldownSeconds is how long it stays open.
	BreakerThreshold       int `json:"breaker_threshold"`
	BreakerCooldownSeconds int `json:"breaker_cooldown_seconds"`
}

// APIKey is a client key allowed to call the gateway.
type APIKey struct {
	Name   string `json:"name"`
	KeyEnv string `json:"key_env"`
	RPM    int    `json:"rpm"` // requests per minute, 0 = unlimited
	// MonthlyBudgetUSD stops the key once its estimated spend this month
	// reaches the limit. 0 = no budget.
	MonthlyBudgetUSD float64 `json:"monthly_budget_usd"`
	Key              string  `json:"-"` // resolved from KeyEnv at load time
}

// Provider is an upstream model backend.
type Provider struct {
	Name           string `json:"name"`
	Type           string `json:"type"` // "openai" or "ollama"
	BaseURL        string `json:"base_url"`
	APIKeyEnv      string `json:"api_key_env"`
	Local          bool   `json:"local"` // runs on our own hardware (safe for personal data)
	TimeoutSeconds int    `json:"timeout_seconds"`
	APIKey         string `json:"-"`
}

// Target is one provider/model pair a model alias can route to.
type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// PII controls what happens when a request contains personal data.
type PII struct {
	// Mode is one of: "route_local" (default), "redact", "block", "off".
	Mode string `json:"mode"`
}

// Cache configures response caching.
type Cache struct {
	Backend    string `json:"backend"` // "memory" (default) or "redis"
	RedisAddr  string `json:"redis_addr"`
	TTLSeconds int    `json:"ttl_seconds"`
	MaxEntries int    `json:"max_entries"` // memory backend only
	RedisPool  int    `json:"redis_pool"`  // redis backend only
}

// ModelPricing is the cost in USD per 1,000 tokens, used for cost metrics.
type ModelPricing struct {
	InputPer1K  float64 `json:"input_per_1k"`
	OutputPer1K float64 `json:"output_per_1k"`
}

// Load reads, defaults, resolves secrets and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	cfg.resolveSecrets()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.PII.Mode == "" {
		c.PII.Mode = "route_local"
	}
	if c.Cache.Backend == "" {
		c.Cache.Backend = "memory"
	}
	if c.Cache.TTLSeconds == 0 {
		c.Cache.TTLSeconds = 3600
	}
	if c.Cache.MaxEntries == 0 {
		c.Cache.MaxEntries = 1000
	}
	if c.Cache.RedisPool == 0 {
		c.Cache.RedisPool = 10
	}
	if c.Limits.MaxInFlight == 0 {
		c.Limits.MaxInFlight = 256
	}
	if c.Limits.BreakerThreshold == 0 {
		c.Limits.BreakerThreshold = 5
	}
	if c.Limits.BreakerCooldownSeconds == 0 {
		c.Limits.BreakerCooldownSeconds = 30
	}
	for i := range c.Providers {
		if c.Providers[i].TimeoutSeconds == 0 {
			c.Providers[i].TimeoutSeconds = 60
		}
	}
}

func (c *Config) resolveSecrets() {
	for i := range c.APIKeys {
		c.APIKeys[i].Key = os.Getenv(c.APIKeys[i].KeyEnv)
	}
	for i := range c.Providers {
		if c.Providers[i].APIKeyEnv != "" {
			c.Providers[i].APIKey = os.Getenv(c.Providers[i].APIKeyEnv)
		}
	}
}

// Validate checks the config for mistakes that would only show up at runtime.
func (c *Config) Validate() error {
	var errs []error

	names := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" || p.BaseURL == "" {
			errs = append(errs, fmt.Errorf("provider needs a name and base_url: %+v", p))
		}
		if p.Type != "openai" && p.Type != "ollama" {
			errs = append(errs, fmt.Errorf("provider %q: type must be openai or ollama", p.Name))
		}
		if names[p.Name] {
			errs = append(errs, fmt.Errorf("duplicate provider name %q", p.Name))
		}
		names[p.Name] = true
	}

	if len(c.Models) == 0 {
		errs = append(errs, errors.New("at least one model alias is required"))
	}
	for alias, targets := range c.Models {
		if len(targets) == 0 {
			errs = append(errs, fmt.Errorf("model %q has no targets", alias))
		}
		for _, t := range targets {
			if !names[t.Provider] {
				errs = append(errs, fmt.Errorf("model %q uses unknown provider %q", alias, t.Provider))
			}
		}
	}

	switch c.PII.Mode {
	case "route_local", "redact", "block", "off":
	default:
		errs = append(errs, fmt.Errorf("pii.mode %q is not valid", c.PII.Mode))
	}

	switch c.Cache.Backend {
	case "memory", "off":
	case "redis":
		if c.Cache.RedisAddr == "" {
			errs = append(errs, errors.New("cache.redis_addr is required for the redis backend"))
		}
	default:
		errs = append(errs, fmt.Errorf("cache.backend %q is not valid", c.Cache.Backend))
	}

	for _, k := range c.APIKeys {
		if k.Key == "" {
			errs = append(errs, fmt.Errorf("api key %q: environment variable %s is empty", k.Name, k.KeyEnv))
		}
	}

	return errors.Join(errs...)
}
