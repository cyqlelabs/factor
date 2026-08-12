package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
)

// captureBody serves one canned reply and records the request body, so the
// tests can assert on the exact JSON each backend receives.
func captureBody(t *testing.T, reply string) (*httptest.Server, *map[string]any) {
	t.Helper()
	got := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

const oaReply = `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
const anthReply = `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`

func TestOpenRouterSendsReasoningObject(t *testing.T) {
	srv, got := captureBody(t, oaReply)
	p, err := New(config.Candidate{
		Type: "openrouter", APIBase: srv.URL, APIKey: "k", Model: "google/gemini-pro-latest",
		Reasoning: &config.ReasoningConfig{Effort: "xhigh", Exclude: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	reasoning, ok := (*got)["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("no reasoning object in %v", *got)
	}
	if reasoning["effort"] != "xhigh" || reasoning["exclude"] != true {
		t.Errorf("reasoning = %v", reasoning)
	}
	if _, present := (*got)["reasoning_effort"]; present {
		t.Error("OpenRouter must not also receive reasoning_effort")
	}
}

func TestOpenRouterMaxTokensBudgetWinsOverEffort(t *testing.T) {
	srv, got := captureBody(t, oaReply)
	p, _ := New(config.Candidate{
		Type: "openrouter", APIBase: srv.URL, Model: "m",
		Reasoning: &config.ReasoningConfig{Effort: "high", MaxTokens: 5000},
	})
	if _, err := p.Chat(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	reasoning := (*got)["reasoning"].(map[string]any)
	if reasoning["max_tokens"] != float64(5000) {
		t.Errorf("reasoning = %v", reasoning)
	}
	if _, present := reasoning["effort"]; present {
		t.Error("an explicit budget and an effort must not be sent together")
	}
}

func TestOpenAIDialectUsesReasoningEffort(t *testing.T) {
	srv, got := captureBody(t, oaReply)
	p, _ := New(config.Candidate{
		Type: "openai", APIBase: srv.URL, APIKey: "k", Model: "gpt-5",
		Reasoning: &config.ReasoningConfig{Effort: "high"},
	})
	if _, err := p.Chat(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if (*got)["reasoning_effort"] != "high" {
		t.Errorf("body = %v", *got)
	}
	if _, present := (*got)["reasoning"]; present {
		t.Error("first-party OpenAI endpoints reject the reasoning object")
	}
}

// Local servers are the strictest about unknown fields and the least likely
// to support reasoning parameters at all, so nothing is sent to them.
func TestLocalProvidersSendNoReasoning(t *testing.T) {
	for _, typ := range []string{"ollama", "lmstudio", "llamacpp"} {
		srv, got := captureBody(t, oaReply)
		p, _ := New(config.Candidate{
			Type: typ, APIBase: srv.URL, Model: "qwen3:8b",
			Reasoning: &config.ReasoningConfig{Effort: "xhigh"},
		})
		if _, err := p.Chat(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
			t.Fatal(err)
		}
		if _, present := (*got)["reasoning"]; present {
			t.Errorf("%s received a reasoning object", typ)
		}
		if _, present := (*got)["reasoning_effort"]; present {
			t.Errorf("%s received reasoning_effort", typ)
		}
		if SupportsReasoning(typ) {
			t.Errorf("SupportsReasoning(%q) = true", typ)
		}
	}
}

func TestAnthropicThinkingBudget(t *testing.T) {
	srv, got := captureBody(t, anthReply)
	p, _ := New(config.Candidate{
		Type: "anthropic", APIBase: srv.URL, APIKey: "k", Model: "claude-sonnet-5",
		Reasoning: &config.ReasoningConfig{Effort: "xhigh"},
	})
	_, err := p.Chat(context.Background(), &Request{
		Messages:    []Message{{Role: "user", Content: "hi"}},
		MaxTokens:   4096,
		Temperature: 0.7,
	})
	if err != nil {
		t.Fatal(err)
	}
	thinking, ok := (*got)["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("no thinking block in %v", *got)
	}
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(32768) {
		t.Errorf("thinking = %v", thinking)
	}
	// max_tokens must exceed the budget, and temperature must be dropped.
	if mt := (*got)["max_tokens"].(float64); mt <= 32768 {
		t.Errorf("max_tokens = %v; it must leave room for the thinking budget", mt)
	}
	if _, present := (*got)["temperature"]; present {
		t.Error("temperature must not be sent alongside extended thinking")
	}
}

func TestReasoningOffAndUnset(t *testing.T) {
	for name, cfg := range map[string]*config.ReasoningConfig{
		"unset":        {},
		"nil":          nil,
		"explicit off": {Effort: "none"},
	} {
		srv, got := captureBody(t, oaReply)
		p, _ := New(config.Candidate{Type: "openrouter", APIBase: srv.URL, Model: "m", Reasoning: cfg})
		if _, err := p.Chat(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
			t.Fatal(err)
		}
		if _, present := (*got)["reasoning"]; present {
			t.Errorf("%s: reasoning was sent anyway: %v", name, *got)
		}
	}
}

// Fallback candidates inherit the provider's reasoning unless they set their
// own — otherwise a failover would silently drop the user's setting.
func TestFallbacksInheritReasoning(t *testing.T) {
	own := config.ReasoningConfig{Effort: "low"}
	cfg := config.ProviderConfig{
		Type: "openrouter", Model: "a", Reasoning: config.ReasoningConfig{Effort: "xhigh"},
		Fallbacks: []config.Candidate{
			{Type: "openrouter", Model: "b"},
			{Type: "openrouter", Model: "c", Reasoning: &own},
		},
	}
	cands := cfg.Candidates()
	if cands[0].Reasoning.Effort != "xhigh" || cands[1].Reasoning.Effort != "xhigh" {
		t.Fatalf("candidates = %+v, %+v", cands[0].Reasoning, cands[1].Reasoning)
	}
	if cands[2].Reasoning.Effort != "low" {
		t.Errorf("an explicit fallback setting was overwritten: %+v", cands[2].Reasoning)
	}
}

func TestDefaultConfigAsksForMaximumReasoning(t *testing.T) {
	def := config.Default()
	if def.Provider.Type != "openrouter" || def.Provider.Model != "google/gemini-3.1-pro-preview" {
		t.Errorf("default provider = %s / %s", def.Provider.Type, def.Provider.Model)
	}
	if def.Provider.Reasoning.Effort != "xhigh" {
		t.Errorf("default reasoning effort = %q", def.Provider.Reasoning.Effort)
	}
}
