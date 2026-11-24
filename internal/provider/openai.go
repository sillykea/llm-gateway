package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sillykea/llm-gateway/internal/api"
)

// OpenAI works with any OpenAI-compatible API: OpenAI itself, Azure OpenAI,
// Groq, Together, vLLM, LM Studio and so on.
type OpenAI struct {
	name    string
	baseURL string
	apiKey  string
	local   bool
	client  *http.Client
}

func (o *OpenAI) Name() string { return o.name }
func (o *OpenAI) Local() bool  { return o.local }

func (o *OpenAI) Chat(ctx context.Context, model string, req api.ChatRequest) (*api.ChatResponse, error) {
	req.Model = model
	req.Stream = false
	headers := map[string]string{}
	if o.apiKey != "" {
		headers["Authorization"] = "Bearer " + o.apiKey
	}
	var resp api.ChatResponse
	if err := postJSON(ctx, o.client, o.name, o.baseURL+"/v1/chat/completions", headers, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Stream reads OpenAI's server-sent events ("data: {...}" lines ending with
// "data: [DONE]") and passes each chunk on.
func (o *OpenAI) Stream(ctx context.Context, model string, req api.ChatRequest, emit func(api.ChatChunk) error) (api.Usage, error) {
	req.Model = model
	req.Stream = true
	req.StreamOptions = &api.StreamOptions{IncludeUsage: true}
	headers := map[string]string{"Accept": "text/event-stream"}
	if o.apiKey != "" {
		headers["Authorization"] = "Bearer " + o.apiKey
	}
	resp, err := send(ctx, o.client, o.name, o.baseURL+"/v1/chat/completions", headers, req)
	if err != nil {
		return api.Usage{}, err
	}
	defer resp.Body.Close()

	var usage api.Usage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // blank lines and comments
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			return usage, nil
		}
		var chunk api.ChatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return usage, fmt.Errorf("%s: bad stream chunk: %w", o.name, err)
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue // the usage-only chunk at the end
		}
		chunk.Model = model
		chunk.Usage = nil
		if err := emit(chunk); err != nil {
			return usage, err
		}
	}
	if err := scanner.Err(); err != nil {
		return usage, fmt.Errorf("%s: %w", o.name, err)
	}
	return usage, nil
}
