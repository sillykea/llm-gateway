// Command gateway runs the LLM gateway HTTP server.
//
// Signals:
//
//	SIGTERM, SIGINT  stop accepting requests, finish in-flight ones, exit
//	SIGHUP           reload the config file without dropping connections
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sillykea/llm-gateway/internal/cache"
	"github.com/sillykea/llm-gateway/internal/config"
	"github.com/sillykea/llm-gateway/internal/keysync"
	"github.com/sillykea/llm-gateway/internal/provider"
	"github.com/sillykea/llm-gateway/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "configs/gateway.json", "path to the config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("version", version)
	if err := run(*configPath, log); err != nil {
		log.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func buildProviders(cfg *config.Config) (map[string]provider.Provider, error) {
	providers := map[string]provider.Provider{}
	for _, pc := range cfg.Providers {
		p, err := provider.New(pc)
		if err != nil {
			return nil, err
		}
		providers[pc.Name] = p
	}
	return providers, nil
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	providers, err := buildProviders(cfg)
	if err != nil {
		return err
	}

	var c cache.Cache
	var ready []server.Pinger
	switch cfg.Cache.Backend {
	case "memory":
		c = cache.NewMemory(cfg.Cache.MaxEntries)
	case "redis":
		opts := cache.RedisOptions{PoolSize: cfg.Cache.RedisPool, Password: cfg.Cache.RedisPassword}
		if cfg.Cache.RedisTLS {
			opts.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		r := cache.NewRedis(cfg.Cache.RedisAddr, opts)
		c, ready = r, append(ready, r)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Keys managed in the console. The syncer needs the server (for the
	// config file's key names) and the server needs the syncer, so the
	// reserved-names hook is bound after both exist.
	var keys *keysync.Syncer
	var srv *server.Server
	opts := server.Options{Config: cfg, Providers: providers, Cache: c, Logger: log, Ready: ready}
	if ks := cfg.KeySource; ks.URL != "" {
		keys = keysync.New(keysync.Options{
			URL: ks.URL, Token: ks.Token,
			Interval: time.Duration(ks.RefreshSeconds) * time.Second,
			Logger:   log,
			Reserved: func() map[string]bool { return srv.ConfigKeyNames() },
		})
		opts.Keys = keys
		opts.Ready = append(opts.Ready, keys)
	}
	srv, err = server.New(opts)
	if err != nil {
		return err
	}
	if keys != nil {
		keys.UseMetrics(srv.Metrics())
		go keys.Run(ctx)
	}

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: it would cut off long streamed answers. Each
		// upstream call has its own timeout instead.
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			reload(configPath, cfg, srv, log)
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		log.Info("gateway listening", "addr", cfg.Listen, "providers", len(providers), "pii_mode", cfg.PII.Mode)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
	return nil
}

// reload re-reads the config. Settings that need new sockets (listen address,
// cache backend) are only picked up on restart, and a warning says so.
func reload(path string, original *config.Config, srv *server.Server, log *slog.Logger) {
	cfg, err := config.Load(path)
	var providers map[string]provider.Provider
	if err == nil {
		providers, err = buildProviders(cfg)
	}
	if err != nil {
		srv.ReloadFailed()
		log.Error("config reload failed, keeping the current config", "error", err)
		return
	}
	if err := srv.Reload(cfg, providers); err != nil { // counts its own failures
		log.Error("config reload failed, keeping the current config", "error", err)
		return
	}
	if cfg.Listen != original.Listen || cfg.Cache.Backend != original.Cache.Backend || cfg.Cache.RedisAddr != original.Cache.RedisAddr {
		log.Warn("listen address and cache settings change only on restart")
	}
	log.Info("config reloaded", "models", len(cfg.Models), "providers", len(cfg.Providers))
}
