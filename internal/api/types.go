// Package api holds the OpenAI-compatible request and response types used by
// the gateway. Keeping the gateway's public API compatible with OpenAI means
// any existing client library can point at it without code changes.
package api

// Message is a single chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the body of POST /v1/chat/completions.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream,omitempty"`

	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

// StreamOptions asks OpenAI-compatible APIs to report token usage at the end
// of a stream, which the gateway needs for cost metrics.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatChunk is one server-sent event in a streamed response.
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// ChunkChoice carries the next piece of text in Delta.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Delta is the partial message in a chunk. Fields are omitted when empty,
// matching what OpenAI sends.
type Delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// ChatResponse is the body returned by POST /v1/chat/completions.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is one completion returned by a model.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage reports token counts for a request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ErrorResponse matches the OpenAI error shape.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody describes what went wrong.
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}
