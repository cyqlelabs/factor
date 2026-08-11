package provider

import (
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

func TestNewAnthropicCandidate(t *testing.T) {
	p, err := New(config.Candidate{Type: "anthropic", APIKey: "ak", Model: "claude-x"})
	if err != nil {
		t.Fatal(err)
	}
	a, ok := p.(*Anthropic)
	if !ok {
		t.Fatalf("provider type = %T, want *Anthropic", p)
	}
	if a.apiBase != "https://api.anthropic.com" {
		t.Errorf("apiBase = %q, want the Anthropic default", a.apiBase)
	}
	if a.apiKey != "ak" || p.Model() != "claude-x" || p.Name() != "anthropic:claude-x" {
		t.Errorf("built provider = %+v, name = %q", a, p.Name())
	}
}

func TestNewAnthropicKeepsExplicitAPIBase(t *testing.T) {
	p, err := New(config.Candidate{Type: "anthropic", APIKey: "ak", APIBase: "https://proxy.internal", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.(*Anthropic).apiBase; got != "https://proxy.internal" {
		t.Errorf("apiBase = %q, want the configured proxy", got)
	}
}

func TestNewAnthropicWithoutAPIKeyIsRejected(t *testing.T) {
	_, err := New(config.Candidate{Type: "anthropic", Model: "claude-x"})
	if err == nil {
		t.Fatal("want an error when anthropic has no api_key")
	}
	if !strings.Contains(err.Error(), "api_key is required") {
		t.Errorf("err = %v", err)
	}
}

func TestNewWithoutModelIsRejected(t *testing.T) {
	for _, typ := range []string{"anthropic", "openai", "totally-unknown"} {
		_, err := New(config.Candidate{Type: typ, APIKey: "k", APIBase: "http://x"})
		if err == nil {
			t.Fatalf("type %q: want an error when model is empty", typ)
		}
		if !strings.Contains(err.Error(), "model is required") {
			t.Errorf("type %q: err = %v", typ, err)
		}
	}
}

func TestNewUsesDefaultAPIBasePerKnownType(t *testing.T) {
	cases := []struct{ typ, want string }{
		{"openai", "https://api.openai.com/v1"},
		{"openrouter", "https://openrouter.ai/api/v1"},
		{"groq", "https://api.groq.com/openai/v1"},
		{"ollama", "http://127.0.0.1:11434/v1"},
		{"lmstudio", "http://127.0.0.1:1234/v1"},
		{"llamacpp", "http://127.0.0.1:8080/v1"},
	}
	for _, c := range cases {
		p, err := New(config.Candidate{Type: c.typ, APIKey: "k", Model: "m"})
		if err != nil {
			t.Fatalf("type %q: %v", c.typ, err)
		}
		oa, ok := p.(*OpenAI)
		if !ok {
			t.Fatalf("type %q: provider type = %T, want *OpenAI", c.typ, p)
		}
		if oa.apiBase != c.want {
			t.Errorf("type %q: apiBase = %q, want %q", c.typ, oa.apiBase, c.want)
		}
	}
}

func TestNewExplicitAPIBaseWinsOverDefault(t *testing.T) {
	p, err := New(config.Candidate{Type: "openai", APIKey: "k", APIBase: "http://localhost:9999/v1", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.(*OpenAI).apiBase; got != "http://localhost:9999/v1" {
		t.Errorf("apiBase = %q, want the configured base", got)
	}
}

func TestNewUnknownTypeNeedsAPIBase(t *testing.T) {
	_, err := New(config.Candidate{Type: "mystery", APIKey: "k", Model: "m"})
	if err == nil {
		t.Fatal("want an error for an unknown type with no api_base")
	}
	if !strings.Contains(err.Error(), "api_base is required") {
		t.Errorf("err = %v", err)
	}
}

func TestNewUnknownTypeWithAPIBaseIsOpenAICompatible(t *testing.T) {
	p, err := New(config.Candidate{Type: "vllm", APIBase: "http://gpu-box:8000/v1", Model: "qwen"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*OpenAI); !ok {
		t.Fatalf("provider type = %T, want *OpenAI", p)
	}
	if p.Name() != "openai:qwen" {
		t.Errorf("name = %q", p.Name())
	}
}

func TestBuildChainAssemblesEveryCandidate(t *testing.T) {
	chain, err := BuildChain(config.ProviderConfig{
		Type: "openai", APIKey: "k", Model: "gpt-x",
		Fallbacks: []config.Candidate{
			{Type: "anthropic", APIKey: "ak", Model: "claude-x"},
			{Type: "groq", APIKey: "gk", Model: "llama-x"},
		},
		MaxRetries:       3,
		RetryBackoffSecs: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.providers) != 3 {
		t.Fatalf("chain has %d providers, want 3", len(chain.providers))
	}
	names := []string{chain.providers[0].Name(), chain.providers[1].Name(), chain.providers[2].Name()}
	want := []string{"openai:gpt-x", "anthropic:claude-x", "openai:llama-x"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("provider %d = %q, want %q", i, names[i], want[i])
		}
	}
	if chain.maxRetries != 3 {
		t.Errorf("maxRetries = %d, want 3", chain.maxRetries)
	}
	if chain.backoff != 5*time.Second {
		t.Errorf("backoff = %v, want 5s", chain.backoff)
	}
}

func TestBuildChainPropagatesCandidateError(t *testing.T) {
	_, err := BuildChain(config.ProviderConfig{
		Type: "openai", APIKey: "k", Model: "gpt-x",
		Fallbacks: []config.Candidate{{Type: "anthropic", Model: "claude-x"}},
	})
	if err == nil {
		t.Fatal("want the bad fallback candidate to fail the whole build")
	}
	if !strings.Contains(err.Error(), "api_key is required") {
		t.Errorf("err = %v", err)
	}
}

func TestBuildChainDefaultsBackoffWhenUnset(t *testing.T) {
	for _, secs := range []int{0, -7} {
		chain, err := BuildChain(config.ProviderConfig{
			Type: "openai", APIKey: "k", Model: "m", RetryBackoffSecs: secs,
		})
		if err != nil {
			t.Fatal(err)
		}
		if chain.backoff != 2*time.Second {
			t.Errorf("retry_backoff_secs=%d → backoff %v, want the 2s default", secs, chain.backoff)
		}
	}
}

func TestOpenAICompatibleEndpointUsesFirstCandidate(t *testing.T) {
	base, key, model, ok := OpenAICompatibleEndpoint(config.ProviderConfig{
		Type: "openai", APIKey: "k1", Model: "gpt-x",
		Fallbacks: []config.Candidate{{Type: "groq", APIKey: "k2", Model: "llama-x"}},
	})
	if !ok {
		t.Fatal("ok = false, want the primary candidate to qualify")
	}
	if base != "https://api.openai.com/v1" || key != "k1" || model != "gpt-x" {
		t.Errorf("endpoint = %q, %q, %q", base, key, model)
	}
}

func TestOpenAICompatibleEndpointSkipsAnthropic(t *testing.T) {
	base, key, model, ok := OpenAICompatibleEndpoint(config.ProviderConfig{
		Type: "anthropic", APIKey: "ak", Model: "claude-x",
		Fallbacks: []config.Candidate{{Type: "groq", APIKey: "gk", Model: "llama-x"}},
	})
	if !ok {
		t.Fatal("ok = false, want the groq fallback to qualify")
	}
	if base != "https://api.groq.com/openai/v1" || key != "gk" || model != "llama-x" {
		t.Errorf("endpoint = %q, %q, %q", base, key, model)
	}
}

func TestOpenAICompatibleEndpointSkipsUnknownTypeWithoutBase(t *testing.T) {
	base, _, model, ok := OpenAICompatibleEndpoint(config.ProviderConfig{
		Type: "mystery", Model: "m1",
		Fallbacks: []config.Candidate{{Type: "openrouter", APIKey: "k", Model: "m2"}},
	})
	if !ok {
		t.Fatal("ok = false, want the openrouter fallback to qualify")
	}
	if base != "https://openrouter.ai/api/v1" || model != "m2" {
		t.Errorf("endpoint = %q, %q; a base-less unknown type must be skipped", base, model)
	}
}

func TestOpenAICompatibleEndpointFalseWhenOnlyAnthropic(t *testing.T) {
	base, key, model, ok := OpenAICompatibleEndpoint(config.ProviderConfig{
		Type: "anthropic", APIKey: "ak", Model: "claude-x",
		Fallbacks: []config.Candidate{{Type: "anthropic", APIKey: "ak2", Model: "claude-y"}},
	})
	if ok {
		t.Fatalf("ok = true (%q, %q, %q), want false with no OpenAI-compatible candidate", base, key, model)
	}
	if base != "" || key != "" || model != "" {
		t.Errorf("want zero values on miss, got %q, %q, %q", base, key, model)
	}
}
