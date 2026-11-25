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

func TestRedisPasswordFromEnv(t *testing.T) {
	t.Setenv("REDIS_PW", "pw")
	cfg, err := Load(write(t, `{
		"providers": [{"name": "local", "type": "ollama", "base_url": "http://x"}],
		"models": {"chat": [{"provider": "local", "model": "m"}]},
		"cache": {"backend": "redis", "redis_addr": "r:6379", "redis_tls": true, "redis_password_env": "REDIS_PW"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Cache.RedisTLS || cfg.Cache.RedisPassword != "pw" {
		t.Errorf("got %+v", cfg.Cache)
	}

	t.Setenv("REDIS_PW", "")
	if _, err := Load(write(t, `{
		"providers": [{"name": "local", "type": "ollama", "base_url": "http://x"}],
		"models": {"chat": [{"provider": "local", "model": "m"}]},
		"cache": {"backend": "redis", "redis_addr": "r:6379", "redis_password_env": "REDIS_PW"}
	}`)); err == nil {
		t.Error("an empty password variable should fail validation")
	}
}

func TestKeySource(t *testing.T) {
	base := `"providers": [{"name": "local", "type": "ollama", "base_url": "http://x"}],
		"models": {"chat": [{"provider": "local", "model": "m"}]}`

	t.Setenv("SYNC_TOKEN", "tok")
	cfg, err := Load(write(t, `{`+base+`,
		"key_source": {"url": "http://console:8080/internal/keys", "token_env": "SYNC_TOKEN"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KeySource.Token != "tok" || cfg.KeySource.RefreshSeconds != 30 {
		t.Errorf("got %+v", cfg.KeySource)
	}

	for name, ks := range map[string]string{
		"bad scheme":   `{"url": "ftp://console/keys", "token_env": "SYNC_TOKEN"}`,
		"no host":      `{"url": "http:///keys", "token_env": "SYNC_TOKEN"}`,
		"empty token":  `{"url": "http://console/keys", "token_env": "UNSET_TOKEN"}`,
		"too frequent": `{"url": "http://console/keys", "token_env": "SYNC_TOKEN", "refresh_seconds": 1}`,
	} {
		if _, err := Load(write(t, `{`+base+`, "key_source": `+ks+`}`)); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestSharedLimitsNeedRedis(t *testing.T) {
	t.Setenv("TEST_KEY", "abc")
	base := `"api_keys": [{"name": "k", "key_env": "TEST_KEY"}],
		"providers": [{"name": "local", "type": "ollama", "base_url": "http://x"}],
		"models": {"chat": [{"provider": "local", "model": "llama3.1"}]},`
	_, err := Load(write(t, `{`+base+` "limits": {"shared": true}}`))
	if err == nil || !strings.Contains(err.Error(), "limits.shared") {
		t.Errorf("shared limits with the memory cache: %v", err)
	}
	cfg, err := Load(write(t, `{`+base+` "limits": {"shared": true}, "cache": {"backend": "redis", "redis_addr": "redis:6379"}}`))
	if err != nil || !cfg.Limits.Shared {
		t.Errorf("shared limits with redis: %v", err)
	}
}
