package router

import (
	"context"
	"testing"

	"github.com/sillykea/llm-gateway/internal/api"
	"github.com/sillykea/llm-gateway/internal/config"
	"github.com/sillykea/llm-gateway/internal/provider"
)

type stub struct {
	name  string
	local bool
}

func (s stub) Name() string { return s.name }
func (s stub) Local() bool  { return s.local }
func (s stub) Chat(context.Context, string, api.ChatRequest) (*api.ChatResponse, error) {
	return nil, nil
}
func (s stub) Stream(context.Context, string, api.ChatRequest, func(api.ChatChunk) error) (api.Usage, error) {
	return api.Usage{}, nil
}

func TestResolve(t *testing.T) {
	providers := map[string]provider.Provider{
		"cloud": stub{"cloud", false},
		"local": stub{"local", true},
	}
	r, err := New(map[string][]config.Target{
		"chat": {{Provider: "cloud", Model: "a"}, {Provider: "local", Model: "b"}},
	}, providers)
	if err != nil {
		t.Fatal(err)
	}

	all, ok := r.Resolve("chat", false)
	if !ok || len(all) != 2 || all[0].Model != "a" {
		t.Fatalf("fallback order not kept: %+v", all)
	}
	local, _ := r.Resolve("chat", true)
	if len(local) != 1 || local[0].Model != "b" {
		t.Fatalf("local only should keep just b: %+v", local)
	}
	if _, ok := r.Resolve("nope", false); ok {
		t.Error("unknown alias should not resolve")
	}
}

func TestUnknownProvider(t *testing.T) {
	_, err := New(map[string][]config.Target{"chat": {{Provider: "ghost"}}}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
}
