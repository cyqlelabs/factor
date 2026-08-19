// Package provider defines the LLM provider seam: a minimal Chat interface,
// wire-format adapters (OpenAI-compatible, Anthropic), error classification,
// and a failover chain with per-candidate cooldowns.
package provider

import "context"

type Message struct {
	Role       string     `json:"role"` // system | user | assistant | tool
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// Images attach to user messages only — the one placement every
	// vision-capable wire dialect accepts. They live in the in-flight turn,
	// not in persisted history (the agent loop strips them before storage).
	Images []ImagePart `json:"images,omitempty"`
}

// ImagePart is one attached image, wire-format neutral.
type ImagePart struct {
	MediaType string `json:"media_type"` // e.g. "image/png"
	Data      string `json:"data"`       // base64 payload, no data: URI prefix
}

type ToolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema
}

type Request struct {
	Messages    []Message
	Tools       []ToolDefinition
	MaxTokens   int
	Temperature float64 // 0 = provider default
}

type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

type Response struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
	// Model names who actually answered. The chain fails over between
	// candidates, so the caller cannot infer it from configuration — and
	// pricing a turn against the wrong model is worse than not pricing it.
	Model string
}

// Provider is the single seam every LLM backend implements.
type Provider interface {
	Chat(ctx context.Context, req *Request) (*Response, error)
	Model() string
	Name() string
}
