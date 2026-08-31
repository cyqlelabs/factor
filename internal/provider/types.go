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
	// CacheMark says this message ends a stretch of the request the next one
	// is likely to repeat byte for byte, so a dialect with an explicit prompt
	// cache should put a breakpoint here. It is a hint about the caller's
	// assembly order, which only the caller knows, and dialects that cache
	// implicitly ignore it. Never persisted: it describes one request, not
	// the conversation.
	CacheMark bool `json:"-"`
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

	// NoReasoning marks a housekeeping call — a summary, a classification —
	// whose budget has to reach the answer. On the OpenAI dialects MaxTokens
	// caps reasoning and content together, so a reasoning model handed a small
	// cap spends all of it thinking and returns nothing: measured against
	// qwen3.7-plus at effort xhigh, 1024 tokens bought 1024 reasoning tokens
	// and a null summary, which then replaced a session's whole history.
	NoReasoning bool
}

// Usage is one call's token traffic, normalized across dialects that disagree
// about what "prompt tokens" means. PromptTokens is always the whole input —
// the Anthropic API reports the uncached remainder there and counts the rest
// separately, so the adapter adds them back. CacheReadTokens and
// CacheWriteTokens are subsets of PromptTokens, never additions to it, which
// is what lets the meter discount one and surcharge the other without
// double-counting either.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	// CacheReadTokens is the part of the input served from a prompt cache.
	CacheReadTokens int
	// CacheWriteTokens is the part of the input written to the cache by this
	// call. Zero on dialects that cache implicitly and charge nothing for it.
	CacheWriteTokens int
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
