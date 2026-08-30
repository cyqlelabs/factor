package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAI speaks the OpenAI chat-completions wire format, which also covers
// OpenRouter, Groq, Ollama, LM Studio, llama.cpp, vLLM, LiteLLM, etc.
type OpenAI struct {
	apiBase string
	apiKey  string
	model   string
	client  *http.Client

	reasoning *Reasoning
	dialect   string // "object" | "effort" | "" (send nothing)
}

// WithReasoning attaches reasoning parameters in the dialect the given
// provider type understands. It returns the receiver for chaining.
func (p *OpenAI) WithReasoning(r *Reasoning, providerType string) *OpenAI {
	p.reasoning, p.dialect = r, reasoningDialect(providerType)
	return p
}

func NewOpenAI(apiBase, apiKey, model string) *OpenAI {
	return &OpenAI{
		apiBase: apiBase,
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 10 * time.Minute},
	}
}

func (p *OpenAI) Model() string { return p.model }
func (p *OpenAI) Name() string  { return "openai:" + p.model }

// oaMessage's Content is a string for plain text and a content-parts array
// ([{type:"text"},{type:"image_url"}]) when a user message carries images —
// the multimodal form every OpenAI-compatible endpoint understands.
type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

// oaContent builds the content field: plain string normally, parts with images.
func oaContent(m Message) any {
	if len(m.Images) == 0 || m.Role != "user" {
		return m.Content
	}
	parts := make([]map[string]any, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, img := range m.Images {
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + img.MediaType + ";base64," + img.Data},
		})
	}
	return parts
}

type oaToolCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"`
	Function oaFunction `json:"function"`
}

type oaFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaRequest struct {
	Model           string         `json:"model"`
	Messages        []oaMessage    `json:"messages"`
	MaxTokens       int            `json:"max_tokens,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	Tools           []oaTool       `json:"tools,omitempty"`
	Reasoning       map[string]any `json:"reasoning,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// oaRespMessage keeps response decoding strict: models reply with string
// content, never parts.
type oaRespMessage struct {
	Content   string       `json:"content"`
	ToolCalls []oaToolCall `json:"tool_calls"`
}

type oaResponse struct {
	Choices []struct {
		Message      oaRespMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// A model that reasons inside <think> writes those delimiters into its content,
// and an endpoint that lifts the reasoning into a field of its own — OpenRouter
// does, for Qwen — leaves the closing tag behind on its own. Either way the tag
// is not the model addressing the user: unstripped, it reached a Telegram chat
// as a message reading "</think>". Only the head of the content is examined,
// because a tag further in is the model writing about tags.
func stripReasoning(content string) string {
	s := strings.TrimLeft(content, " \t\r\n")
	switch {
	case strings.HasPrefix(s, "<think>"):
		_, answer, closed := strings.Cut(s, "</think>")
		if !closed {
			// The block never ended, so the answer never started.
			return ""
		}
		return strings.TrimSpace(answer)
	case strings.HasPrefix(s, "</think>"):
		return strings.TrimSpace(strings.TrimPrefix(s, "</think>"))
	}
	return content
}

func (p *OpenAI) Chat(ctx context.Context, req *Request) (*Response, error) {
	body := oaRequest{Model: p.model, MaxTokens: req.MaxTokens}
	switch {
	case req.NoReasoning:
		// Silence is not enough for a gateway that was going to reason anyway,
		// so an object-dialect one is told to stop outright. The effort dialect
		// has no "off" to send, and leaving the field out is all it takes.
		if p.dialect == "object" && p.reasoning != nil {
			body.Reasoning = map[string]any{"enabled": false}
		}
	case p.dialect == "object":
		body.Reasoning = p.reasoning.object()
	case p.dialect == "effort":
		if p.reasoning != nil {
			body.ReasoningEffort = p.reasoning.Effort
		}
	}
	if req.Temperature != 0 {
		t := req.Temperature
		body.Temperature = &t
	}
	for _, m := range req.Messages {
		om := oaMessage{Role: m.Role, Content: oaContent(m), ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			args, err := json.Marshal(tc.Args)
			if err != nil {
				args = []byte("{}")
			}
			om.ToolCalls = append(om.ToolCalls, oaToolCall{
				ID: tc.ID, Type: "function",
				Function: oaFunction{Name: tc.Name, Arguments: string(args)},
			})
		}
		body.Messages = append(body.Messages, om)
	}
	for _, t := range req.Tools {
		var ot oaTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.Parameters
		body.Tools = append(body.Tools, ot)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, ClassifyTransport(p.Name(), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, ClassifyTransport(p.Name(), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, ClassifyStatus(p.Name(), resp.StatusCode, string(data))
	}

	var parsed oaResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, &APIError{Provider: p.Name(), Reason: ReasonFormat, Err: fmt.Errorf("decode response: %w", err)}
	}
	if len(parsed.Choices) == 0 {
		return nil, &APIError{Provider: p.Name(), Reason: ReasonFormat, Body: string(data), Err: fmt.Errorf("no choices in response")}
	}

	choice := parsed.Choices[0]
	out := &Response{
		Content:      stripReasoning(choice.Message.Content),
		FinishReason: choice.FinishReason,
		Usage:        Usage{PromptTokens: parsed.Usage.PromptTokens, CompletionTokens: parsed.Usage.CompletionTokens},
		Model:        p.model,
	}
	for _, tc := range choice.Message.ToolCalls {
		args := map[string]any{}
		if tc.Function.Arguments != "" {
			// Tolerate malformed arguments; the tool layer reports validation errors.
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args})
	}
	return out, nil
}
