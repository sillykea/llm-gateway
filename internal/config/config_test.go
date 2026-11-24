package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gateway.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAppliesDefaultsAndSecrets(t *testing.T) {
	t.Setenv("TEST_KEY", "abc")
	cfg, err := Load(write(t, `{
		"api_keys": [{"name": "k", "key_env": "TEST_KEY"}],
		"providers": [{"name": "local", "type": "ollama", "base_url": "http://x"}],
		"models": {"chat": [{"provider": "local", "model": "llama3.1"}]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || cfg.PII.Mode != "route_local" || cfg.Cache.Backend != "memory" {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if cfg.APIKeys[0].Key != "abc" {
		t.Error("api key not read from the environment")
	}
}

func TestValidateCatchesMistakes(t *testing.T) {
	_, err := Load(write(t, `{
		"providers": [{"name": "p", "type": "grpc", "base_url": "http://x"}],
		"models": {"chat": [{"provider": "missing", "model": "m"}]},
		"pii": {"mode": "maybe"}
	}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"type must be", "unknown provider", "pii.mode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}
