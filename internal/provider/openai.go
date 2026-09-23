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
	"sync/atomic"
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

	// reasoningMandatory is set once the endpoint has refused to switch
	// reasoning off, so every later housekeeping call is sent the smallest
	// effort instead of the off switch and the 400 is paid once per process.
	reasoningMandatory atomic.Bool
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
//
// Reasoning is read for one reason: some endpoints put the answer in it. See
// answerOf.
type oaRespMessage struct {
	Content   string       `json:"content"`
	Reasoning string       `json:"reasoning"`
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
		// Caching on these dialects is implicit — the endpoint decides, and
		// PromptTokens already includes what it served from cache. There is
		// no write counter because there is no write premium to report.
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
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

// answerOf is what the model said to the user. Normally that is the content,
// and the reasoning beside it is the model thinking out loud — never
// something to repeat back.
//
// Some endpoints do not keep them apart. A reply that stopped cleanly with no
// content, no tool calls and a full paragraph in its reasoning field is an
// answer filed under the wrong name: observed here on one OpenRouter route
// for a reasoning model, where the whole spoken reply — "esa adrenalina de
// arrancar de nuevo…" — arrived as reasoning and the user heard silence. A
// turn with nothing to say is worse than one that says its working: the user
// cannot tell it from a crash, and asking it to repeat itself finds nothing
// in the transcript either, because the empty message is what was recorded.
//
// The fallback is deliberately narrow. Any tool call, any content at all, or
// a completion cut short mid-thought, and the reasoning stays private: a
// truncated chain of thought is not an answer, and a model that said
// something has already chosen what to say.
func answerOf(msg oaRespMessage, finishReason string) string {
	if content := stripReasoning(msg.Content); content != "" {
		return content
	}
	if len(msg.ToolCalls) > 0 || finishReason == "length" {
		return ""
	}
	return strings.TrimSpace(msg.Reasoning)
}

// mandatoryReasoningBudget is the thinking allowance a housekeeping call is
// given by an endpoint that will not switch reasoning off. It is added on
// top of the caller's cap rather than carved out of it: asked for effort
// "low" under a 1024-token cap, glm-5.3-flash spent all 1024 on reasoning
// and returned no content, so the cap has to cover the thinking as well.
const mandatoryReasoningBudget = 8192

// mandatoryReasoning is the reasoning object such a call is sent: a budget
// rather than an effort, excluded from the reply.
func mandatoryReasoning() map[string]any {
	return map[string]any{"max_tokens": mandatoryReasoningBudget, "exclude": true}
}

// withMandatoryReasoning rewrites a body that carried the off switch into one
// the endpoint will answer, with the cap raised so the answer still fits.
func withMandatoryReasoning(body oaRequest, cap int) oaRequest {
	body.Reasoning = mandatoryReasoning()
	if cap > 0 {
		body.MaxTokens = cap + mandatoryReasoningBudget
	}
	return body
}

// reasoningRefused reports a 400 that says reasoning cannot be turned off —
// observed on z-ai/glm-5.3-flash: "Reasoning is mandatory for this endpoint
// and cannot be disabled."
func reasoningRefused(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		return false
	}
	lower := strings.ToLower(apiErr.Body)
	return strings.Contains(lower, "reasoning") &&
		(strings.Contains(lower, "mandatory") || strings.Contains(lower, "cannot be disabled"))
}

func (p *OpenAI) Chat(ctx context.Context, req *Request) (*Response, error) {
	body := oaRequest{Model: p.model, MaxTokens: req.MaxTokens}
	disabled := false
	switch {
	case req.NoReasoning:
		// Silence is not enough for a gateway that was going to reason anyway,
		// so an object-dialect one is told to stop outright. The effort dialect
		// has no "off" to send, and leaving the field out is all it takes.
		if p.dialect == "object" && p.reasoning != nil {
			if p.reasoningMandatory.Load() {
				body = withMandatoryReasoning(body, req.MaxTokens)
			} else {
				body.Reasoning = map[string]any{"enabled": false}
				disabled = true
			}
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

	resp, err := p.send(ctx, body)
	if err != nil && disabled && reasoningRefused(err) {
		// The endpoint has said it cannot stop thinking, so it is asked to
		// think as little as it can instead, and remembered as such.
		p.reasoningMandatory.Store(true)
		return p.send(ctx, withMandatoryReasoning(body, req.MaxTokens))
	}
	return resp, err
}

func (p *OpenAI) send(ctx context.Context, body oaRequest) (*Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	Identify(httpReq.Header)
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
		Content:      answerOf(choice.Message, choice.FinishReason),
		FinishReason: choice.FinishReason,
		Usage: Usage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
			CacheReadTokens:  parsed.Usage.PromptTokensDetails.CachedTokens,
		},
		Model: p.model,
	}
	for _, tc := range choice.Message.ToolCalls {
		args := map[string]any{}
		malformed := false
		if tc.Function.Arguments != "" {
			// Arguments that do not decode are kept as a call, not dropped:
			// a tool_call with no result beside it is a request the next
			// turn rejects. Decoding may have filled part of the map before
			// it failed, and a half-read call is worse than an empty one.
			if json.Unmarshal([]byte(tc.Function.Arguments), &args) != nil {
				args = map[string]any{}
				malformed = true
			}
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args, Malformed: malformed})
	}
	return out, nil
}
