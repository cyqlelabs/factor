package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpenAIRoundTrip(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}}
			]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "test-key", "gpt-test")
	resp, err := p.Chat(context.Background(), &Request{
		Messages: []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c0", Name: "t", Args: map[string]any{"x": 1.0}}}},
			{Role: "tool", ToolCallID: "c0", Content: "result"},
		},
		Tools:     []ToolDefinition{{Name: "read_file", Description: "d", Parameters: map[string]any{"type": "object"}}},
		MaxTokens: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Args["path"] != "a.txt" {
		t.Errorf("args = %v", resp.ToolCalls[0].Args)
	}
	if resp.Usage.PromptTokens != 10 {
		t.Errorf("usage = %+v", resp.Usage)
	}

	msgs := captured["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("sent %d messages", len(msgs))
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "c0" {
		t.Errorf("tool message = %v", toolMsg)
	}
	asst := msgs[2].(map[string]any)
	tcs := asst["tool_calls"].([]any)
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"x":1}` {
		t.Errorf("arguments = %v", fn["arguments"])
	}
}

func TestAnthropicRoundTrip(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "ak" {
			t.Errorf("key = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content":[{"type":"text","text":"done"},{"type":"tool_use","id":"tu1","name":"exec","input":{"command":"ls"}}],
			"stop_reason":"tool_use","usage":{"input_tokens":7,"output_tokens":3}}`))
	}))
	defer srv.Close()

	p := NewAnthropic(srv.URL, "ak", "claude-test")
	resp, err := p.Chat(context.Background(), &Request{
		Messages: []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{{ID: "a", Name: "t1", Args: nil}, {ID: "b", Name: "t2", Args: nil}}},
			{Role: "tool", ToolCallID: "a", Content: "r1"},
			{Role: "tool", ToolCallID: "b", Content: "r2"},
		},
		MaxTokens: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "done" || len(resp.ToolCalls) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.ToolCalls[0].Args["command"] != "ls" {
		t.Errorf("args = %v", resp.ToolCalls[0].Args)
	}

	if captured["system"] != "sys" {
		t.Errorf("system = %v", captured["system"])
	}
	msgs := captured["messages"].([]any)
	// user, assistant, single merged tool_result user message
	if len(msgs) != 3 {
		t.Fatalf("sent %d messages, want 3 (tool results merged)", len(msgs))
	}
	last := msgs[2].(map[string]any)
	blocks := last["content"].([]any)
	if last["role"] != "user" || len(blocks) != 2 {
		t.Errorf("merged tool results = %v", last)
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   Reason
		retry  bool
	}{
		{401, "", ReasonAuth, false},
		{402, "", ReasonBilling, false},
		{429, "slow down", ReasonRateLimit, true},
		{400, "bad request", ReasonFormat, false},
		{400, "This model's maximum context length is 8192 tokens", ReasonContextOverflow, false},
		{413, "prompt is too long: 210000 tokens", ReasonContextOverflow, false},
		{503, "", ReasonOverloaded, true},
		{529, "", ReasonOverloaded, true},
		{500, "boom", ReasonUnknown, true},
	}
	for _, c := range cases {
		e := ClassifyStatus("p", c.status, c.body)
		if e.Reason != c.want {
			t.Errorf("status %d %q → %s, want %s", c.status, c.body, e.Reason, c.want)
		}
		if e.Retriable() != c.retry {
			t.Errorf("status %d retriable = %v, want %v", c.status, e.Retriable(), c.retry)
		}
	}
}

type scriptedProvider struct {
	name  string
	calls int
	fn    func(call int) (*Response, error)
}

func (s *scriptedProvider) Chat(_ context.Context, _ *Request) (*Response, error) {
	s.calls++
	return s.fn(s.calls)
}
func (s *scriptedProvider) Model() string { return s.name }
func (s *scriptedProvider) Name() string  { return s.name }

func fastChain(providers ...Provider) *Chain {
	c := NewChain(providers, 2, time.Millisecond)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func TestChainFailover(t *testing.T) {
	bad := &scriptedProvider{name: "bad", fn: func(int) (*Response, error) {
		return nil, &APIError{Provider: "bad", Reason: ReasonRateLimit}
	}}
	good := &scriptedProvider{name: "good", fn: func(int) (*Response, error) {
		return &Response{Content: "hello"}, nil
	}}
	resp, err := fastChain(bad, good).Chat(context.Background(), &Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello" {
		t.Errorf("content = %q", resp.Content)
	}
	if bad.calls != 1 || good.calls != 1 {
		t.Errorf("calls: bad=%d good=%d", bad.calls, good.calls)
	}
}

func TestChainCooldownSkipsFailingProvider(t *testing.T) {
	bad := &scriptedProvider{name: "bad", fn: func(int) (*Response, error) {
		return nil, &APIError{Provider: "bad", Reason: ReasonAuth}
	}}
	good := &scriptedProvider{name: "good", fn: func(int) (*Response, error) {
		return &Response{Content: "ok"}, nil
	}}
	chain := fastChain(bad, good)
	for range 3 {
		if _, err := chain.Chat(context.Background(), &Request{}); err != nil {
			t.Fatal(err)
		}
	}
	if bad.calls != 1 {
		t.Errorf("bad called %d times; hard cooldown should skip it", bad.calls)
	}
	if good.calls != 3 {
		t.Errorf("good calls = %d", good.calls)
	}
}

func TestChainContextOverflowAborts(t *testing.T) {
	over := &scriptedProvider{name: "over", fn: func(int) (*Response, error) {
		return nil, &APIError{Provider: "over", Reason: ReasonContextOverflow}
	}}
	next := &scriptedProvider{name: "next", fn: func(int) (*Response, error) {
		return &Response{Content: "should not reach"}, nil
	}}
	_, err := fastChain(over, next).Chat(context.Background(), &Request{})
	if !IsContextOverflow(err) {
		t.Fatalf("err = %v", err)
	}
	if next.calls != 0 {
		t.Error("chain advanced past a context overflow")
	}
	if over.calls != 1 {
		t.Errorf("over calls = %d, want 1 (no retry)", over.calls)
	}
}

func TestChainRecoversAfterCooldownOnFinalAttempt(t *testing.T) {
	// A single provider that fails twice then succeeds: the final attempt
	// ignores cooldowns, so the chain must recover.
	flaky := &scriptedProvider{name: "flaky", fn: func(call int) (*Response, error) {
		if call <= 2 {
			return nil, &APIError{Provider: "flaky", Reason: ReasonOverloaded}
		}
		return &Response{Content: "recovered"}, nil
	}}
	resp, err := fastChain(flaky).Chat(context.Background(), &Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "recovered" {
		t.Errorf("content = %q", resp.Content)
	}
}

func TestChainUnclassifiedErrorAborts(t *testing.T) {
	boom := errors.New("programming error")
	bad := &scriptedProvider{name: "bad", fn: func(int) (*Response, error) { return nil, boom }}
	next := &scriptedProvider{name: "next", fn: func(int) (*Response, error) { return &Response{}, nil }}
	_, err := fastChain(bad, next).Chat(context.Background(), &Request{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if next.calls != 0 {
		t.Error("unclassified error should abort, not fail over")
	}
}
