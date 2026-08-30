package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// bodyCapture records the request body a handler received so the test
// goroutine can inspect it without racing the server goroutine.
type bodyCapture struct {
	mu   sync.Mutex
	body map[string]any
}

func (c *bodyCapture) record(t *testing.T, r *http.Request) {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Error(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = m
}

func (c *bodyCapture) get() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body
}

// jsonServer replies with a fixed status and body.
func jsonServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// deadEndpoint returns the URL of a server that is already shut down, so a
// request to it fails at the transport layer.
func deadEndpoint(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

func wantAPIError(t *testing.T, err error) *APIError {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	return apiErr
}

func TestOpenAIChatClassifiesNon2xx(t *testing.T) {
	srv := jsonServer(t, http.StatusTooManyRequests, `{"error":"slow down"}`)
	_, err := NewOpenAI(srv.URL, "k", "gpt-x").Chat(context.Background(), &Request{})
	apiErr := wantAPIError(t, err)
	if apiErr.Reason != ReasonRateLimit {
		t.Errorf("reason = %s, want %s", apiErr.Reason, ReasonRateLimit)
	}
	if apiErr.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d", apiErr.Status)
	}
	if apiErr.Provider != "openai:gpt-x" {
		t.Errorf("provider = %q", apiErr.Provider)
	}
}

func TestOpenAIChatMalformedBodyIsFormatError(t *testing.T) {
	srv := jsonServer(t, http.StatusOK, `{"choices": [ this is not json`)
	_, err := NewOpenAI(srv.URL, "k", "gpt-x").Chat(context.Background(), &Request{})
	apiErr := wantAPIError(t, err)
	if apiErr.Reason != ReasonFormat {
		t.Errorf("reason = %s, want %s", apiErr.Reason, ReasonFormat)
	}
	if apiErr.Err == nil || !strings.Contains(apiErr.Err.Error(), "decode response") {
		t.Errorf("want a decode cause, got %v", apiErr.Err)
	}
}

func TestOpenAIChatEmptyChoicesIsFormatError(t *testing.T) {
	srv := jsonServer(t, http.StatusOK, `{"choices":[],"usage":{"prompt_tokens":1}}`)
	_, err := NewOpenAI(srv.URL, "k", "gpt-x").Chat(context.Background(), &Request{})
	apiErr := wantAPIError(t, err)
	if apiErr.Reason != ReasonFormat {
		t.Errorf("reason = %s, want %s", apiErr.Reason, ReasonFormat)
	}
	if apiErr.Err == nil || !strings.Contains(apiErr.Err.Error(), "no choices") {
		t.Errorf("want a no-choices cause, got %v", apiErr.Err)
	}
	if !strings.Contains(apiErr.Body, "prompt_tokens") {
		t.Errorf("body should carry the raw payload for debugging, got %q", apiErr.Body)
	}
}

func TestOpenAIChatTransportFailureIsClassified(t *testing.T) {
	_, err := NewOpenAI(deadEndpoint(t), "k", "gpt-x").Chat(context.Background(), &Request{})
	apiErr := wantAPIError(t, err)
	if apiErr.Reason != ReasonNetwork {
		t.Errorf("reason = %s, want %s", apiErr.Reason, ReasonNetwork)
	}
	if apiErr.Err == nil {
		t.Error("transport failure must keep the underlying cause")
	}
}

func TestOpenAIChatRejectsUnbuildableRequest(t *testing.T) {
	_, err := NewOpenAI("http://exa\x7fmple.invalid/v1", "k", "gpt-x").Chat(context.Background(), &Request{})
	if err == nil {
		t.Fatal("want an error for an unparseable api_base")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Errorf("a request-construction failure is not a provider API error: %v", err)
	}
}

func TestOpenAIChatUnmarshalableToolSchemaFails(t *testing.T) {
	srv := jsonServer(t, http.StatusOK, `{"choices":[{"message":{"content":"hi"}}]}`)
	_, err := NewOpenAI(srv.URL, "k", "gpt-x").Chat(context.Background(), &Request{
		Tools: []ToolDefinition{{Name: "t", Parameters: map[string]any{"bad": make(chan int)}}},
	})
	if err == nil {
		t.Fatal("want an error for a tool schema that cannot be encoded")
	}
}

func TestOpenAIChatToleratesUnmarshalableToolCallArgs(t *testing.T) {
	captured := &bodyCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.record(t, r)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	resp, err := NewOpenAI(srv.URL, "k", "gpt-x").Chat(context.Background(), &Request{
		Messages: []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "t", Args: map[string]any{"ch": make(chan int)}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	msgs := captured.get()["messages"].([]any)
	fn := msgs[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != "{}" {
		t.Errorf("arguments = %v, want the empty-object fallback", fn["arguments"])
	}
}

func TestOpenAIChatSendsTemperatureOnlyWhenSet(t *testing.T) {
	captured := &bodyCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.record(t, r)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	p := NewOpenAI(srv.URL, "", "gpt-x")

	if _, err := p.Chat(context.Background(), &Request{}); err != nil {
		t.Fatal(err)
	}
	if _, present := captured.get()["temperature"]; present {
		t.Error("temperature 0 means 'provider default' and must be omitted")
	}

	if _, err := p.Chat(context.Background(), &Request{Temperature: 0.7}); err != nil {
		t.Fatal(err)
	}
	if got := captured.get()["temperature"]; got != 0.7 {
		t.Errorf("temperature = %v, want 0.7", got)
	}
}

func TestOpenAIChatOmitsAuthorizationWithoutKey(t *testing.T) {
	var authSeen struct {
		mu    sync.Mutex
		value string
		set   bool
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authSeen.mu.Lock()
		authSeen.value, authSeen.set = r.Header.Get("Authorization"), true
		authSeen.mu.Unlock()
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	if _, err := NewOpenAI(srv.URL, "", "gpt-x").Chat(context.Background(), &Request{}); err != nil {
		t.Fatal(err)
	}
	authSeen.mu.Lock()
	defer authSeen.mu.Unlock()
	if !authSeen.set {
		t.Fatal("handler never ran")
	}
	if authSeen.value != "" {
		t.Errorf("Authorization = %q, want no header for a keyless local endpoint", authSeen.value)
	}
}

// truncatedBodyServer promises more bytes than it delivers and hangs up, so
// reading the response body fails mid-stream.
func truncatedBodyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n{\"cho")
		_ = buf.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAIChatBodyReadFailureIsClassified(t *testing.T) {
	srv := truncatedBodyServer(t)
	_, err := NewOpenAI(srv.URL, "k", "gpt-x").Chat(context.Background(), &Request{})
	apiErr := wantAPIError(t, err)
	if apiErr.Reason != ReasonNetwork {
		t.Errorf("reason = %s, want %s for a truncated body", apiErr.Reason, ReasonNetwork)
	}
}

func TestOpenAIModelAndName(t *testing.T) {
	p := NewOpenAI("http://x/v1", "", "gpt-x")
	if p.Model() != "gpt-x" || p.Name() != "openai:gpt-x" {
		t.Errorf("Model() = %q, Name() = %q", p.Model(), p.Name())
	}
}

func TestAnthropicChatBodyReadFailureIsClassified(t *testing.T) {
	srv := truncatedBodyServer(t)
	_, err := NewAnthropic(srv.URL, "ak", "claude-x").Chat(context.Background(), &Request{})
	apiErr := wantAPIError(t, err)
	if apiErr.Reason != ReasonNetwork {
		t.Errorf("reason = %s, want %s for a truncated body", apiErr.Reason, ReasonNetwork)
	}
}

func TestAnthropicChatClassifiesNon2xx(t *testing.T) {
	srv := jsonServer(t, http.StatusUnauthorized, `{"error":{"message":"invalid x-api-key"}}`)
	_, err := NewAnthropic(srv.URL, "ak", "claude-x").Chat(context.Background(), &Request{})
	apiErr := wantAPIError(t, err)
	if apiErr.Reason != ReasonAuth {
		t.Errorf("reason = %s, want %s", apiErr.Reason, ReasonAuth)
	}
	if apiErr.Provider != "anthropic:claude-x" {
		t.Errorf("provider = %q", apiErr.Provider)
	}
}

func TestAnthropicChatContextOverflowIsClassified(t *testing.T) {
	srv := jsonServer(t, http.StatusBadRequest, `{"error":{"message":"prompt is too long: 210000 tokens"}}`)
	_, err := NewAnthropic(srv.URL, "ak", "claude-x").Chat(context.Background(), &Request{})
	if !IsContextOverflow(err) {
		t.Fatalf("err = %v, want a context-overflow classification", err)
	}
}

func TestAnthropicChatMalformedBodyIsFormatError(t *testing.T) {
	srv := jsonServer(t, http.StatusOK, `{"content": [ broken`)
	_, err := NewAnthropic(srv.URL, "ak", "claude-x").Chat(context.Background(), &Request{})
	apiErr := wantAPIError(t, err)
	if apiErr.Reason != ReasonFormat {
		t.Errorf("reason = %s, want %s", apiErr.Reason, ReasonFormat)
	}
}

func TestAnthropicChatTransportFailureIsClassified(t *testing.T) {
	_, err := NewAnthropic(deadEndpoint(t), "ak", "claude-x").Chat(context.Background(), &Request{})
	apiErr := wantAPIError(t, err)
	if apiErr.Reason != ReasonNetwork {
		t.Errorf("reason = %s, want %s", apiErr.Reason, ReasonNetwork)
	}
}

func TestAnthropicChatRejectsUnbuildableRequest(t *testing.T) {
	_, err := NewAnthropic("http://exa\x7fmple.invalid", "ak", "claude-x").Chat(context.Background(), &Request{})
	if err == nil {
		t.Fatal("want an error for an unparseable api_base")
	}
}

func TestAnthropicChatUnmarshalableToolSchemaFails(t *testing.T) {
	srv := jsonServer(t, http.StatusOK, `{"content":[]}`)
	_, err := NewAnthropic(srv.URL, "ak", "claude-x").Chat(context.Background(), &Request{
		Tools: []ToolDefinition{{Name: "t", Parameters: map[string]any{"bad": make(chan int)}}},
	})
	if err == nil {
		t.Fatal("want an error for a tool schema that cannot be encoded")
	}
}

func TestAnthropicChatDefaultsMaxTokens(t *testing.T) {
	captured := &bodyCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.record(t, r)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	if _, err := NewAnthropic(srv.URL, "ak", "claude-x").Chat(context.Background(), &Request{MaxTokens: 0}); err != nil {
		t.Fatal(err)
	}
	if got := captured.get()["max_tokens"]; got != 4096.0 {
		t.Errorf("max_tokens = %v, want the 4096 default", got)
	}
	if _, present := captured.get()["temperature"]; present {
		t.Error("temperature 0 must be omitted")
	}
}

func TestAnthropicChatSendsTemperatureAndTools(t *testing.T) {
	captured := &bodyCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.record(t, r)
		_, _ = w.Write([]byte(`{"content":[{"type":"other","text":"ignored"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	resp, err := NewAnthropic(srv.URL, "ak", "claude-x").Chat(context.Background(), &Request{
		MaxTokens:   64,
		Temperature: 0.25,
		Tools:       []ToolDefinition{{Name: "exec", Description: "d", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "" || len(resp.ToolCalls) != 0 {
		t.Errorf("unknown content blocks must be ignored, got %+v", resp)
	}
	if got := captured.get()["temperature"]; got != 0.25 {
		t.Errorf("temperature = %v", got)
	}
	tools := captured.get()["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "exec" {
		t.Errorf("tools = %v", tools)
	}
}

func TestToAnthropicMergesSystemMessages(t *testing.T) {
	system, out := toAnthropic([]Message{
		{Role: "system", Content: "first"},
		{Role: "system", Content: "second"},
		{Role: "user", Content: "hi"},
	})
	if system != "first\n\nsecond" {
		t.Errorf("system = %q", system)
	}
	if len(out) != 1 || out[0].Role != "user" {
		t.Fatalf("messages = %+v", out)
	}
}

func TestToAnthropicEmptyAssistantGetsPlaceholderBlock(t *testing.T) {
	_, out := toAnthropic([]Message{{Role: "assistant"}})
	if len(out) != 1 {
		t.Fatalf("messages = %+v", out)
	}
	blocks := out[0].Content
	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "" {
		t.Errorf("want one empty text block so the API never sees empty content, got %+v", blocks)
	}
}

func TestToAnthropicToolResultAfterPlainUserStartsNewMessage(t *testing.T) {
	_, out := toAnthropic([]Message{
		{Role: "user", Content: "hi"},
		{Role: "tool", ToolCallID: "a", Content: "r1"},
		{Role: "tool", ToolCallID: "b", Content: "r2"},
	})
	if len(out) != 2 {
		t.Fatalf("want the tool results in their own message, got %d messages: %+v", len(out), out)
	}
	if out[0].Content[0].Type != "text" {
		t.Errorf("first message = %+v, want the plain user text", out[0])
	}
	if len(out[1].Content) != 2 || out[1].Content[0].ToolUseID != "a" || out[1].Content[1].ToolUseID != "b" {
		t.Errorf("consecutive tool results must merge, got %+v", out[1].Content)
	}
}

func TestToAnthropicLeadingToolResultStartsUserMessage(t *testing.T) {
	_, out := toAnthropic([]Message{{Role: "tool", ToolCallID: "a", Content: "r"}})
	if len(out) != 1 || out[0].Role != "user" || out[0].Content[0].Type != "tool_result" {
		t.Fatalf("messages = %+v", out)
	}
}

func TestToAnthropicToolResultAfterAssistantStartsNewMessage(t *testing.T) {
	_, out := toAnthropic([]Message{
		{Role: "assistant", Content: "thinking"},
		{Role: "tool", ToolCallID: "a", Content: "r"},
	})
	if len(out) != 2 || out[1].Role != "user" || out[1].Content[0].Type != "tool_result" {
		t.Fatalf("messages = %+v", out)
	}
}

func TestToAnthropicAssistantToolCallsWithNilArgs(t *testing.T) {
	_, out := toAnthropic([]Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "exec", Args: nil}}},
	})
	if len(out) != 1 || len(out[0].Content) != 1 {
		t.Fatalf("messages = %+v", out)
	}
	block := out[0].Content[0]
	if block.Type != "tool_use" || block.Input == nil {
		t.Errorf("nil args must become an empty object, got %+v", block)
	}
}

func TestToAnthropicDropsUnknownRoles(t *testing.T) {
	system, out := toAnthropic([]Message{{Role: "function", Content: "legacy"}})
	if system != "" || len(out) != 0 {
		t.Errorf("unknown roles must be dropped, got system=%q out=%+v", system, out)
	}
}

// The leak this guards: OpenRouter lifts a Qwen <think> block into a reasoning
// field of its own and leaves the closing tag in the content, which the loop
// then sent to the chat as an interim note reading "</think>".
func TestOpenAIChatStripsReasoningDelimiters(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"orphan closing tag", "\n</think>\n\n", ""},
		{"orphan tag before an answer", "</think>\n\nHelium is in engine/.", "Helium is in engine/."},
		{"whole block", "<think>where does it live?</think>\n\nIn engine/.", "In engine/."},
		{"unterminated block", "<think>still reasoning when the budget ran out", ""},
		{"a tag the model is writing about", "The reply ended in </think>, which is the bug.", "The reply ended in </think>, which is the bug."},
		{"ordinary content is untouched", "  spaced out  ", "  spaced out  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"content": tc.content}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			srv := jsonServer(t, http.StatusOK, string(body))
			resp, err := NewOpenAI(srv.URL, "k", "qwen").Chat(context.Background(), &Request{})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Content != tc.want {
				t.Errorf("content = %q, want %q", resp.Content, tc.want)
			}
		})
	}
}
