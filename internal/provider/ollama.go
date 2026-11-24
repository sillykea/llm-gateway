package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/sillykea/llm-gateway/internal/api"
)

// Ollama talks to Ollama's native /api/chat endpoint and converts the
// response into the OpenAI shape, so clients never see the difference.
type Ollama struct {
	name    string
	baseURL string
	local   bool
	client  *http.Client
}

type ollamaRequest struct {
	Model    string         `json:"model"`
	Messages []api.Message  `json:"messages"`
	Stream   bool           `json:"stream"`
	Options  map[string]any `json:"options,omitempty"`
}

type ollamaResponse struct {
	Model           string      `json:"model"`
	Message         api.Message `json:"message"`
	Done            bool        `json:"done"`
	DoneReason      string      `json:"done_reason"`
	PromptEvalCount int         `json:"prompt_eval_count"`
	EvalCount       int         `json:"eval_count"`
}

func (o *Ollama) Name() string { return o.name }
func (o *Ollama) Local() bool  { return o.local }

func buildOllamaRequest(model string, req api.ChatRequest, stream bool) ollamaRequest {
	body := ollamaRequest{Model: model, Messages: req.Messages, Stream: stream}
	opts := map[string]any{}
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if req.MaxTokens != nil {
		opts["num_predict"] = *req.MaxTokens
	}
	if len(opts) > 0 {
		body.Options = opts
	}
	return body
}

func (o *Ollama) Chat(ctx context.Context, model string, req api.ChatRequest) (*api.ChatResponse, error) {
	body := buildOllamaRequest(model, req, false)

	var out ollamaResponse
	if err := postJSON(ctx, o.client, o.name, o.baseURL+"/api/chat", nil, body, &out); err != nil {
		return nil, err
	}

	finish := out.DoneReason
	if finish == "" {
		finish = "stop"
	}
	now := time.Now()
	return &api.ChatResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", now.UnixNano()),
		Object:  "chat.completion",
		Created: now.Unix(),
		Model:   model,
		Choices: []api.Choice{{Index: 0, Message: out.Message, FinishReason: finish}},
		Usage: api.Usage{
			PromptTokens:     out.PromptEvalCount,
			CompletionTokens: out.EvalCount,
			TotalTokens:      out.PromptEvalCount + out.EvalCount,
		},
	}, nil
}

// Stream reads Ollama's newline-delimited JSON and converts each line into
// an OpenAI-style chunk, so streaming clients see the same format whichever
// backend answered.
func (o *Ollama) Stream(ctx context.Context, model string, req api.ChatRequest, emit func(api.ChatChunk) error) (api.Usage, error) {
	resp, err := send(ctx, o.client, o.name, o.baseURL+"/api/chat", nil, buildOllamaRequest(model, req, true))
	if err != nil {
		return api.Usage{}, err
	}
	defer resp.Body.Close()

	now := time.Now()
	id := fmt.Sprintf("chatcmpl-%d", now.UnixNano())
	first := true

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var line ollamaResponse
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			return api.Usage{}, fmt.Errorf("%s: bad stream line: %w", o.name, err)
		}

		delta := api.Delta{Content: line.Message.Content}
		if first {
			delta.Role = "assistant"
			first = false
		}
		var finish *string
		if line.Done {
			reason := line.DoneReason
			if reason == "" {
				reason = "stop"
			}
			finish = &reason
		}
		chunk := api.ChatChunk{
			ID: id, Object: "chat.completion.chunk", Created: now.Unix(), Model: model,
			Choices: []api.ChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		}
		if err := emit(chunk); err != nil {
			return api.Usage{}, err
		}
		if line.Done {
			return api.Usage{
				PromptTokens:     line.PromptEvalCount,
				CompletionTokens: line.EvalCount,
				TotalTokens:      line.PromptEvalCount + line.EvalCount,
			}, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return api.Usage{}, fmt.Errorf("%s: %w", o.name, err)
	}
	return api.Usage{}, fmt.Errorf("%s: stream ended without a final message", o.name)
}
