// Package provider talks to upstream model backends.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sillykea/llm-gateway/internal/api"
	"github.com/sillykea/llm-gateway/internal/config"
	"github.com/sillykea/llm-gateway/internal/tracing"
)

// Provider sends a chat request to one backend.
type Provider interface {
	Name() string
	Local() bool
	Chat(ctx context.Context, model string, req api.ChatRequest) (*api.ChatResponse, error)

	// Stream sends chunks to emit as they arrive and returns the final token
	// usage. If emit returns an error (the client went away), streaming stops.
	Stream(ctx context.Context, model string, req api.ChatRequest, emit func(api.ChatChunk) error) (api.Usage, error)
}

// UpstreamError is returned when a backend answers with a non-2xx status.
type UpstreamError struct {
	Provider string
	Status   int
	Body     string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("%s returned %d: %s", e.Provider, e.Status, e.Body)
}

// Retryable reports whether trying the next provider makes sense.
// Network errors, timeouts, rate limits and 5xx are worth a fallback;
// a 400 (bad request) would fail everywhere, so it is not.
func Retryable(err error) bool {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.Status == http.StatusTooManyRequests || ue.Status >= 500
	}
	return !errors.Is(err, context.Canceled)
}

// New builds a provider from config.
func New(p config.Provider) (Provider, error) {
	// The timeout covers connecting and waiting for the first byte, not the
	// whole body, so long streamed answers aren't cut off part way through.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = time.Duration(p.TimeoutSeconds) * time.Second
	client := &http.Client{Transport: transport}
	base := strings.TrimRight(p.BaseURL, "/")
	switch p.Type {
	case "openai":
		return &OpenAI{name: p.Name, baseURL: base, apiKey: p.APIKey, local: p.Local, client: client}, nil
	case "ollama":
		return &Ollama{name: p.Name, baseURL: base, local: p.Local, client: client}, nil
	}
	return nil, fmt.Errorf("unknown provider type %q", p.Type)
}

// send posts a JSON body and returns the response if the status is 2xx.
// The caller must close the body.
func send(ctx context.Context, client *http.Client, name, url string, headers map[string]string, body any) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	tracing.Inject(ctx, req.Header) // carry the trace into the model server
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &UpstreamError{Provider: name, Status: resp.StatusCode, Body: truncate(string(data), 300)}
	}
	return resp, nil
}

func postJSON(ctx context.Context, client *http.Client, name, url string, headers map[string]string, body, out any) error {
	resp, err := send(ctx, client, name, url, headers, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return fmt.Errorf("%s: read body: %w", name, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: decode response: %w", name, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
