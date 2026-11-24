package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sillykea/llm-gateway/internal/api"
	"github.com/sillykea/llm-gateway/internal/config"
)

var hello = api.ChatRequest{Messages: []api.Message{{Role: "user", Content: "hi"}}}

func newProvider(t *testing.T, typ string, h http.HandlerFunc) Provider {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	p, err := New(config.Provider{Name: typ, Type: typ, BaseURL: srv.URL, APIKey: "sk-test", TimeoutSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func collect(t *testing.T, p Provider) (string, api.Usage, error) {
	t.Helper()
	var text strings.Builder
	usage, err := p.Stream(context.Background(), "m", hello, func(c api.ChatChunk) error {
		text.WriteString(c.Choices[0].Delta.Content)
		return nil
	})
	return text.String(), usage, err
}

func TestOpenAIChatSendsKeyAndModel(t *testing.T) {
	p := newProvider(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Error("missing API key")
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"gpt-4o-mini"`) {
			t.Errorf("model not set: %s", body)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"yo"}}],"usage":{"total_tokens":3}}`)
	})
	resp, err := p.Chat(context.Background(), "gpt-4o-mini", hello)
	if err != nil || resp.Choices[0].Message.Content != "yo" {
		t.Fatalf("got %+v, %v", resp, err)
	}
}

func TestOpenAIStream(t *testing.T) {
	p := newProvider(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		fmt.Fprint(w, ": keep-alive comment\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	text, usage, err := collect(t, p)
	if err != nil || text != "Hello" || usage.TotalTokens != 6 {
		t.Fatalf("text %q usage %+v err %v", text, usage, err)
	}
}

func TestOllamaChatConvertsResponse(t *testing.T) {
	p := newProvider(t, "ollama", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"temperature":0`) {
			t.Errorf("temperature not passed as an option: %s", body)
		}
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"kia ora"},"done":true,"prompt_eval_count":7,"eval_count":3}`)
	})
	zero := 0.0
	req := hello
	req.Temperature = &zero
	resp, err := p.Chat(context.Background(), "llama3.1", req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content != "kia ora" || resp.Usage.TotalTokens != 10 || resp.Choices[0].FinishReason != "stop" {
		t.Errorf("bad conversion: %+v", resp)
	}
}

func TestOllamaStream(t *testing.T) {
	p := newProvider(t, "ollama", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"Kia "},"done":false}`)
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"ora"},"done":false}`)
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":5,"eval_count":2}`)
	})
	text, usage, err := collect(t, p)
	if err != nil || text != "Kia ora" || usage.TotalTokens != 7 {
		t.Fatalf("text %q usage %+v err %v", text, usage, err)
	}
}

func TestOllamaStreamCutOff(t *testing.T) {
	p := newProvider(t, "ollama", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"message":{"content":"half"},"done":false}`)
	})
	if _, _, err := collect(t, p); err == nil {
		t.Fatal("a stream that ends without done should be an error")
	}
}

func TestUpstreamErrors(t *testing.T) {
	p := newProvider(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	})
	_, err := p.Chat(context.Background(), "m", hello)
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Status != 503 {
		t.Fatalf("got %v", err)
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&UpstreamError{Status: 500}, true},
		{&UpstreamError{Status: 429}, true},
		{&UpstreamError{Status: 400}, false},
		{&UpstreamError{Status: 401}, false},
		{errors.New("connection refused"), true},
		{context.Canceled, false},
	}
	for _, c := range cases {
		if got := Retryable(c.err); got != c.want {
			t.Errorf("Retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
