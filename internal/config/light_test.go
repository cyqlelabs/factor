package config

import "testing"

// An install that has to be configured before it will say anything is an
// install that says nothing, so the filler chain resolves to something on
// every backend rather than to nil.
func TestLightCandidatesDefaultToTheFastestReachableModel(t *testing.T) {
	openrouter := ProviderConfig{Type: "openrouter", APIKey: "k", Model: "big"}
	got := openrouter.LightCandidates()
	if len(got) != 1 || got[0].Model != DefaultLightModel {
		t.Fatalf("openrouter light = %+v, want %s", got, DefaultLightModel)
	}
	if got[0].APIKey != "k" || got[0].Type != "openrouter" {
		t.Errorf("credentials should be inherited: %+v", got[0])
	}

	// Everywhere else there is no catalogue to reach into, so the filler runs
	// on the model already configured — with the thinking off, which is most
	// of what made it slow.
	ollama := ProviderConfig{Type: "ollama", APIBase: "http://127.0.0.1:11434", Model: "qwen3:8b"}
	got = ollama.LightCandidates()
	if len(got) != 1 || got[0].Model != "qwen3:8b" {
		t.Fatalf("ollama light = %+v", got)
	}
	if got[0].Reasoning == nil || got[0].Reasoning.Effort != DefaultLightEffort {
		t.Errorf("light reasoning = %+v, want an explicit %s", got[0].Reasoning, DefaultLightEffort)
	}
}

// Leaving the effort out is not the same as asking for none of it: the models
// worth putting here reason by default, and on the OpenAI dialects the token
// cap covers the thinking, so an unstated effort spends the whole cap on a
// chain of thought and answers with no content at all.
func TestLightCandidatesStateTheirEffortRatherThanOmittingIt(t *testing.T) {
	got := ProviderConfig{Type: "openrouter", APIKey: "k", Model: "big"}.LightCandidates()[0]
	if got.Reasoning == nil || got.Reasoning.IsZero() {
		t.Fatalf("an omitted effort reaches the wire as no parameters at all: %+v", got.Reasoning)
	}
	// A user who asked for something else keeps it.
	p := ProviderConfig{Type: "openrouter", Model: "big",
		Light: &Candidate{Model: "vendor/tiny", Reasoning: &ReasoningConfig{Effort: "none"}}}
	if got := p.LightCandidates()[0]; got.Reasoning.Effort != "none" {
		t.Errorf("light reasoning = %+v, want the configured one", got.Reasoning)
	}
}

func TestLightCandidateNamedInConfigWins(t *testing.T) {
	p := ProviderConfig{Type: "openrouter", APIKey: "k", Model: "big", Light: &Candidate{Model: "vendor/tiny"}}
	got := p.LightCandidates()
	if len(got) != 1 || got[0].Model != "vendor/tiny" || got[0].APIKey != "k" {
		t.Fatalf("light = %+v", got)
	}
}

// A candidate that names only where to go still needs something to call.
func TestLightCandidateWithoutAModelFallsBackToTheDefault(t *testing.T) {
	p := ProviderConfig{Type: "openrouter", APIKey: "k", Model: "big", Light: &Candidate{}}
	if got := p.LightCandidates(); len(got) != 1 || got[0].Model != DefaultLightModel {
		t.Fatalf("light = %+v", got)
	}
}

// Another vendor's endpoint gets none of this one's credentials.
func TestLightCandidateOfAnotherTypeInheritsNoCredentials(t *testing.T) {
	p := ProviderConfig{Type: "openrouter", APIKey: "k", APIBase: "https://openrouter.ai/api/v1", Model: "big",
		Light: &Candidate{Type: "ollama", Model: "qwen3:8b"}}
	c := p.LightCandidates()[0]
	if c.APIKey != "" || c.APIBase != "" {
		t.Errorf("light candidate = %+v, want no inherited credentials", c)
	}
}

// With no provider at all there is nothing to fall back to, and the filler is
// off rather than pointed at a blank model name.
func TestLightCandidatesAreEmptyWithoutAProvider(t *testing.T) {
	if got := (ProviderConfig{}).LightCandidates(); len(got) != 0 {
		t.Fatalf("light = %+v", got)
	}
}
