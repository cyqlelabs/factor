package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Anthropic speaks the native Messages API.
type Anthropic struct {
	apiBase string
	apiKey  string
	model   string
	client  *http.Client

	reasoning *Reasoning
}

// WithReasoning turns on extended thinking with the configured budget.
func (p *Anthropic) WithReasoning(r *Reasoning) *Anthropic {
	p.reasoning = r
	return p
}

// anthThinking is the extended-thinking block.
type anthThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

func NewAnthropic(apiBase, apiKey, model string) *Anthropic {
	if apiBase == "" {
		apiBase = "https://api.anthropic.com"
	}
	return &Anthropic{
		apiBase: apiBase,
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 10 * time.Minute},
	}
}

func (p *Anthropic) Model() string { return p.model }
func (p *Anthropic) Name() string  { return "anthropic:" + p.model }

type anthContent struct {
	Type      string           `json:"type"`
	Text      string           `json:"text,omitempty"`
	ID        string           `json:"id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Input     map[string]any   `json:"input,omitempty"`
	ToolUseID string           `json:"tool_use_id,omitempty"`
	Content   string           `json:"content,omitempty"`
	Source    *anthImageSource `json:"source,omitempty"`
}

// anthImageSource is the Messages API image block payload.
type anthImageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthMessage struct {
	Role    string        `json:"role"`
	Content []anthContent `json:"content"`
}

type anthTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthRequest struct {
	Model       string        `json:"model"`
	MaxTokens   int           `json:"max_tokens"`
	System      string        `json:"system,omitempty"`
	Messages    []anthMessage `json:"messages"`
	Tools       []anthTool    `json:"tools,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	Thinking    *anthThinking `json:"thinking,omitempty"`
}

// applyThinking sets the thinking block, keeping the two invariants the
// Messages API enforces: max_tokens must exceed the budget, and temperature
// must be left at its default while thinking is on.
func (p *Anthropic) applyThinking(body *anthRequest) {
	budget := p.reasoning.budget()
	if budget <= 0 {
		return
	}
	body.Thinking = &anthThinking{Type: "enabled", BudgetTokens: budget}
	if body.MaxTokens <= budget {
		body.MaxTokens = budget + 4096
	}
	body.Temperature = nil
}

type anthResponse struct {
	Content    []anthContent `json:"content"`
	StopReason string        `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// toAnthropic converts the neutral message list: system messages merge into the
// system field; consecutive tool results merge into one user message.
func toAnthropic(messages []Message) (system string, out []anthMessage) {
	for _, m := range messages {
		switch m.Role {
		case "system":
			if system != "" {
				system += "\n\n"
			}
			system += m.Content
		case "user":
			blocks := []anthContent{{Type: "text", Text: m.Content}}
			for _, img := range m.Images {
				blocks = append(blocks, anthContent{Type: "image", Source: &anthImageSource{
					Type: "base64", MediaType: img.MediaType, Data: img.Data,
				}})
			}
			out = append(out, anthMessage{Role: "user", Content: blocks})
		case "assistant":
			var blocks []anthContent
			if m.Content != "" {
				blocks = append(blocks, anthContent{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := tc.Args
				if input == nil {
					input = map[string]any{}
				}
				blocks = append(blocks, anthContent{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
			}
			if len(blocks) == 0 {
				blocks = []anthContent{{Type: "text", Text: ""}}
			}
			out = append(out, anthMessage{Role: "assistant", Content: blocks})
		case "tool":
			block := anthContent{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content}
			if n := len(out); n > 0 && out[n-1].Role == "user" && out[n-1].Content[0].Type == "tool_result" {
				out[n-1].Content = append(out[n-1].Content, block)
			} else {
				out = append(out, anthMessage{Role: "user", Content: []anthContent{block}})
			}
		}
	}
	return system, out
}

func (p *Anthropic) Chat(ctx context.Context, req *Request) (*Response, error) {
	system, messages := toAnthropic(req.Messages)
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	body := anthRequest{Model: p.model, MaxTokens: maxTokens, System: system, Messages: messages}
	if req.Temperature != 0 {
		t := req.Temperature
		body.Temperature = &t
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, anthTool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters})
	}
	if !req.NoReasoning {
		p.applyThinking(&body)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

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

	var parsed anthResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, &APIError{Provider: p.Name(), Reason: ReasonFormat, Err: fmt.Errorf("decode response: %w", err)}
	}

	out := &Response{
		FinishReason: parsed.StopReason,
		Usage:        Usage{PromptTokens: parsed.Usage.InputTokens, CompletionTokens: parsed.Usage.OutputTokens},
		Model:        p.model,
	}
	for _, block := range parsed.Content {
		switch block.Type {
		case "text":
			out.Content += block.Text
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: block.ID, Name: block.Name, Args: block.Input})
		}
	}
	return out, nil
}
