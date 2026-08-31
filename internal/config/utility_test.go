package config

import "testing"

// Naming a cheaper model of the same vendor should be one line, so a utility
// candidate inherits the type, key and base it did not state.
func TestUtilityCandidatesInheritTheMainProvider(t *testing.T) {
	p := ProviderConfig{
		Type: "anthropic", APIKey: "k", APIBase: "https://example.test", Model: "big",
		Utility: []Candidate{{Model: "small"}},
	}
	got := p.UtilityCandidates()
	if len(got) != 1 {
		t.Fatalf("got %d candidates", len(got))
	}
	c := got[0]
	if c.Type != "anthropic" || c.APIKey != "k" || c.APIBase != "https://example.test" || c.Model != "small" {
		t.Errorf("candidate = %+v", c)
	}
}

// A different vendor must not inherit the first one's credentials: sending an
// Anthropic key to somebody else's endpoint is how a key leaks.
func TestUtilityCandidateOfAnotherTypeInheritsNoCredentials(t *testing.T) {
	p := ProviderConfig{
		Type: "anthropic", APIKey: "secret", APIBase: "https://anthropic.test", Model: "big",
		Utility: []Candidate{{Type: "ollama", Model: "small"}},
	}
	c := p.UtilityCandidates()[0]
	if c.APIKey != "" || c.APIBase != "" {
		t.Errorf("credentials crossed vendors: %+v", c)
	}
}

func TestUtilityCandidatesSkipEntriesWithNoModel(t *testing.T) {
	p := ProviderConfig{Type: "openai", Model: "big", Utility: []Candidate{{}, {Model: "small"}}}
	got := p.UtilityCandidates()
	if len(got) != 1 || got[0].Model != "small" {
		t.Errorf("candidates = %+v", got)
	}
}

func TestNoUtilityConfiguredMeansNoChain(t *testing.T) {
	p := ProviderConfig{Type: "openai", Model: "big"}
	if got := p.UtilityCandidates(); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// Reasoning defaults off. A summary that spends its budget thinking is a
// summary that never arrives, and it is what replaces the session's history.
func TestUtilityCandidatesDefaultReasoningOff(t *testing.T) {
	p := ProviderConfig{
		Type: "openai", Model: "big",
		Reasoning: ReasoningConfig{Effort: "high"},
		Utility:   []Candidate{{Model: "small"}},
	}
	c := p.UtilityCandidates()[0]
	if c.Reasoning == nil {
		t.Fatal("reasoning config should be set, not left nil")
	}
	if c.Reasoning.Effort != "" {
		t.Errorf("utility inherited reasoning effort %q", c.Reasoning.Effort)
	}
}
