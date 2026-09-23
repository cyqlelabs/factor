package provider

import (
	"context"
	"encoding/json"
	"errors"
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

// EffortBudget is what the wizard quotes when it explains that Anthropic
// takes a budget rather than an effort. It must be the very number the
// Anthropic request would carry, or setup promises one thing and the provider
// sends another.
func TestEffortBudgetMatchesWhatAnthropicIsSent(t *testing.T) {
	for _, effort := range []string{"minimal", "low", "medium", "high", "xhigh"} {
		quoted := EffortBudget(effort)
		if quoted <= 0 {
			t.Errorf("EffortBudget(%q) = %d; the wizard would quote a nonsense budget", effort, quoted)
			continue
		}
		r := &Reasoning{Effort: effort}
		if sent := r.budget(); sent != quoted {
			t.Errorf("%s: wizard quotes %d tokens but the request carries %d", effort, quoted, sent)
		}
	}
	if got := EffortBudget("turbo"); got != 0 {
		t.Errorf("an unknown effort must have no budget, got %d", got)
	}
}

// A housekeeping call must not be charged for thinking it cannot afford:
// max_tokens caps reasoning and content together on the OpenAI dialects, so a
// summary asked for with effort xhigh came back as 1024 reasoning tokens and no
// content at all.
func TestNoReasoningSuppressesEveryDialect(t *testing.T) {
	t.Run("openrouter is told to stop", func(t *testing.T) {
		srv, got := captureBody(t, oaReply)
		p, err := New(config.Candidate{
			Type: "openrouter", APIBase: srv.URL, APIKey: "k", Model: "qwen/qwen3.7-plus",
			Reasoning: &config.ReasoningConfig{Effort: "xhigh"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Chat(context.Background(), &Request{
			Messages: []Message{{Role: "user", Content: "summarize"}}, MaxTokens: 1024, NoReasoning: true,
		}); err != nil {
			t.Fatal(err)
		}
		reasoning, ok := (*got)["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("no reasoning object in %v", *got)
		}
		if reasoning["enabled"] != false {
			t.Errorf("reasoning = %v, want enabled:false", reasoning)
		}
		if _, present := reasoning["effort"]; present {
			t.Errorf("reasoning = %v, want no effort alongside the off switch", reasoning)
		}
	})

	t.Run("first-party OpenAI gets no effort", func(t *testing.T) {
		srv, got := captureBody(t, oaReply)
		p, _ := New(config.Candidate{
			Type: "openai", APIBase: srv.URL, APIKey: "k", Model: "gpt-x",
			Reasoning: &config.ReasoningConfig{Effort: "high"},
		})
		if _, err := p.Chat(context.Background(), &Request{
			Messages: []Message{{Role: "user", Content: "summarize"}}, NoReasoning: true,
		}); err != nil {
			t.Fatal(err)
		}
		if _, present := (*got)["reasoning_effort"]; present {
			t.Errorf("reasoning_effort survived in %v", *got)
		}
	})

	t.Run("anthropic gets no thinking block", func(t *testing.T) {
		srv, got := captureBody(t, anthReply)
		p, _ := New(config.Candidate{
			Type: "anthropic", APIBase: srv.URL, APIKey: "k", Model: "claude-sonnet-5",
			Reasoning: &config.ReasoningConfig{Effort: "xhigh"},
		})
		if _, err := p.Chat(context.Background(), &Request{
			Messages: []Message{{Role: "user", Content: "summarize"}}, MaxTokens: 1024, NoReasoning: true,
		}); err != nil {
			t.Fatal(err)
		}
		if _, present := (*got)["thinking"]; present {
			t.Errorf("thinking block survived in %v", *got)
		}
		if mt := (*got)["max_tokens"].(float64); mt != 1024 {
			t.Errorf("max_tokens = %v, want the 1024 the caller asked for", mt)
		}
	})

	t.Run("a gateway with no reasoning configured is left alone", func(t *testing.T) {
		srv, got := captureBody(t, oaReply)
		p, _ := New(config.Candidate{Type: "openrouter", APIBase: srv.URL, Model: "m"})
		if _, err := p.Chat(context.Background(), &Request{
			Messages: []Message{{Role: "user", Content: "summarize"}}, NoReasoning: true,
		}); err != nil {
			t.Fatal(err)
		}
		if _, present := (*got)["reasoning"]; present {
			t.Errorf("reasoning field appeared in %v", *got)
		}
	})
}

// The wizard asks about reasoning only where Factor can send it. A local
// server handed an unknown field usually rejects the whole request, so the
// question would offer a setting that breaks the provider it was asked for.
func TestSupportsReasoning(t *testing.T) {
	for _, kind := range []string{"openrouter", "openai", "anthropic", "groq", "", "something-new"} {
		if !SupportsReasoning(kind) {
			t.Errorf("%q was told it cannot reason", kind)
		}
	}
	for _, kind := range []string{"ollama", "lmstudio", "llamacpp"} {
		if SupportsReasoning(kind) {
			t.Errorf("%q was offered reasoning", kind)
		}
	}
}

// The filler's whole value is arriving quickly, and the models fast enough to
// be worth putting there reason by default. Leaving the effort unstated is
// how the parameter never reaches them: a chain built from an unconfigured
// light section must carry an explicit one to a gateway that understands it,
// and none at all to a local server that would reject the field.
func TestLightChainStatesItsEffortWhereItIsUnderstood(t *testing.T) {
	srv, got := captureBody(t, oaReply)
	chain, err := BuildLightChain(config.ProviderConfig{
		Type: "openrouter", APIKey: "k", APIBase: srv.URL, Model: "big"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chain.Chat(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "say what you are doing"}}}); err != nil {
		t.Fatal(err)
	}
	reasoning, ok := (*got)["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("no reasoning parameters reached the model: %v", *got)
	}
	if reasoning["effort"] != config.DefaultLightEffort {
		t.Errorf("effort = %v, want %q", reasoning["effort"], config.DefaultLightEffort)
	}

	// A local server is handed nothing: an unknown field there usually takes
	// the whole request down with it.
	local, got := captureBody(t, oaReply)
	chain, err = BuildLightChain(config.ProviderConfig{Type: "ollama", APIBase: local.URL, Model: "qwen3:8b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chain.Chat(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "say what you are doing"}}}); err != nil {
		t.Fatal(err)
	}
	if _, present := (*got)["reasoning"]; present {
		t.Errorf("a local server was handed reasoning parameters: %v", *got)
	}
	if _, present := (*got)["reasoning_effort"]; present {
		t.Errorf("a local server was handed reasoning_effort: %v", *got)
	}
}

// Some endpoints refuse the off switch outright — z-ai/glm-5.3-flash answers
// `reasoning: {enabled: false}` with a 400 "Reasoning is mandatory for this
// endpoint and cannot be disabled" — and a compaction summary that fails on
// it fails on every attempt after. The dialect is expected to notice, ask for
// the least thinking the endpoint allows, and remember not to try the switch
// again.
func TestMandatoryReasoningIsDetected(t *testing.T) {
	const refusal = `{"error":{"message":"Reasoning is mandatory for this endpoint and cannot be disabled.","code":400}}`
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got := map[string]any{}
		_ = json.Unmarshal(raw, &got)
		bodies = append(bodies, got)
		if reasoning, _ := got["reasoning"].(map[string]any); reasoning["enabled"] == false {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(refusal))
			return
		}
		_, _ = w.Write([]byte(oaReply))
	}))
	t.Cleanup(srv.Close)
	p, err := New(config.Candidate{
		Type: "openrouter", APIBase: srv.URL, APIKey: "k", Model: "z-ai/glm-5.3-flash",
		Reasoning: &config.ReasoningConfig{Effort: "xhigh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := &Request{Messages: []Message{{Role: "user", Content: "summarize"}}, MaxTokens: 1024, NoReasoning: true}
	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("the refusal must be retried, got %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q, want the retry's answer", resp.Content)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d requests sent, want the refused one and its retry", len(bodies))
	}
	retry := bodies[1]["reasoning"].(map[string]any)
	if retry["effort"] != "low" || retry["exclude"] != true {
		t.Errorf("retry reasoning = %v, want the smallest effort, excluded", retry)
	}
	if _, present := retry["enabled"]; present {
		t.Errorf("retry reasoning = %v, still carries the off switch", retry)
	}
	if mt := bodies[1]["max_tokens"].(float64); mt != 1024 {
		t.Errorf("retry max_tokens = %v, want the caller's cap kept", mt)
	}

	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Fatalf("%d requests after the second call, want one: the refusal is remembered", len(bodies))
	}
	if again := bodies[2]["reasoning"].(map[string]any); again["effort"] != "low" {
		t.Errorf("second call reasoning = %v, want the remembered low effort", again)
	}

	if _, err := p.Chat(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if conv := bodies[3]["reasoning"].(map[string]any); conv["effort"] != "xhigh" {
		t.Errorf("conversation reasoning = %v, want the configured effort untouched", conv)
	}
}

func TestReasoningRefusedMatchesOnlyTheRefusal(t *testing.T) {
	cases := map[error]bool{
		ClassifyStatus("p", 400, `{"error":{"message":"Reasoning is mandatory for this endpoint and cannot be disabled."}}`): true,
		ClassifyStatus("p", 400, `{"error":{"message":"reasoning cannot be disabled on this model"}}`):                       true,
		ClassifyStatus("p", 400, `{"error":{"message":"invalid model"}}`):                                                    false,
		ClassifyStatus("p", 500, `reasoning is mandatory`):                                                                   false,
		errors.New("reasoning is mandatory"):                                                                                 false,
	}
	for err, want := range cases {
		if got := reasoningRefused(err); got != want {
			t.Errorf("reasoningRefused(%v) = %v, want %v", err, got, want)
		}
	}
}
