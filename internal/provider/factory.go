package provider

import (
	"fmt"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

var defaultAPIBases = map[string]string{
	"openai":     "https://api.openai.com/v1",
	"openrouter": "https://openrouter.ai/api/v1",
	"groq":       "https://api.groq.com/openai/v1",
	"ollama":     "http://127.0.0.1:11434/v1",
	"lmstudio":   "http://127.0.0.1:1234/v1",
	"llamacpp":   "http://127.0.0.1:8080/v1",
}

// DefaultAPIBase returns the built-in endpoint for a provider type ("" when
// the type has none and api_base must be supplied).
func DefaultAPIBase(providerType string) string {
	if providerType == "anthropic" {
		return "https://api.anthropic.com/v1"
	}
	return defaultAPIBases[providerType]
}

// New builds a Provider from one candidate. Every type except "anthropic" is
// OpenAI-compatible; unknown types work too when api_base is set.
func New(c config.Candidate) (Provider, error) {
	if c.Model == "" {
		return nil, fmt.Errorf("provider %q: model is required", c.Type)
	}
	reasoning := reasoningFrom(c.Reasoning)
	if c.Type == "anthropic" {
		if c.APIKey == "" {
			return nil, fmt.Errorf("provider anthropic: api_key is required")
		}
		return NewAnthropic(c.APIBase, c.APIKey, c.Model).WithReasoning(reasoning), nil
	}
	base := c.APIBase
	if base == "" {
		base = defaultAPIBases[c.Type]
	}
	if base == "" {
		return nil, fmt.Errorf("provider %q: api_base is required for unknown types", c.Type)
	}
	return NewOpenAI(base, c.APIKey, c.Model).WithReasoning(reasoning, c.Type), nil
}

// BuildChain assembles the failover chain from configuration.
func BuildChain(cfg config.ProviderConfig) (*Chain, error) {
	var providers []Provider
	for _, cand := range cfg.Candidates() {
		p, err := New(cand)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	backoff := time.Duration(cfg.RetryBackoffSecs) * time.Second
	if backoff <= 0 {
		backoff = 2 * time.Second
	}
	return NewChain(providers, cfg.MaxRetries, backoff), nil
}

// BuildUtilityChain assembles the housekeeping chain, or returns nil when none
// is configured — the caller then bills those calls to the main chain, which
// is what Factor did before this existed.
func BuildUtilityChain(cfg config.ProviderConfig) (*Chain, error) {
	cands := cfg.UtilityCandidates()
	if len(cands) == 0 {
		return nil, nil
	}
	var providers []Provider
	for _, cand := range cands {
		p, err := New(cand)
		if err != nil {
			return nil, fmt.Errorf("utility provider: %w", err)
		}
		providers = append(providers, p)
	}
	backoff := time.Duration(cfg.RetryBackoffSecs) * time.Second
	if backoff <= 0 {
		backoff = 2 * time.Second
	}
	return NewChain(providers, cfg.MaxRetries, backoff), nil
}

// OpenAICompatibleEndpoint returns the chat-completions base URL and key of the
// first OpenAI-compatible candidate, for services that need a raw endpoint
// (e.g. smrti's entity extraction). ok is false when none qualifies.
func OpenAICompatibleEndpoint(cfg config.ProviderConfig) (base, key, model string, ok bool) {
	for _, cand := range cfg.Candidates() {
		if cand.Type == "anthropic" {
			continue
		}
		base := cand.APIBase
		if base == "" {
			base = defaultAPIBases[cand.Type]
		}
		if base != "" {
			return base, cand.APIKey, cand.Model, true
		}
	}
	return "", "", "", false
}
