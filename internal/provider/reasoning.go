package provider

import "github.com/cyqlelabs/factor/internal/config"

// Reasoning is the neutral form of "think before answering". One setting in
// the config reaches three different wire formats:
//
//	OpenRouter (and unknown OpenAI-compatible gateways)
//	    "reasoning": {"effort": "xhigh", "exclude": true}
//	OpenAI, Groq, and other first-party OpenAI endpoints
//	    "reasoning_effort": "xhigh"
//	Anthropic
//	    "thinking": {"type": "enabled", "budget_tokens": 32768}
//
// Sending the wrong one is a 400, so the dialect is decided per provider type
// at construction rather than guessed per request.
type Reasoning struct {
	Effort    string
	MaxTokens int
	Exclude   bool
}

// effortBudgets translate an effort name into a token budget for backends
// (Anthropic) that only speak in budgets.
var effortBudgets = map[string]int{
	"minimal": 1024,
	"low":     2048,
	"medium":  8192,
	"high":    16384,
	"xhigh":   32768,
}

// reasoningFrom converts config to the wire-neutral form; nil means "send no
// reasoning parameters at all".
func reasoningFrom(cfg *config.ReasoningConfig) *Reasoning {
	if cfg == nil || cfg.IsZero() || cfg.Off() {
		return nil
	}
	return &Reasoning{Effort: cfg.Effort, MaxTokens: cfg.MaxTokens, Exclude: cfg.Exclude}
}

// object renders the OpenRouter `reasoning` payload.
func (r *Reasoning) object() map[string]any {
	if r == nil {
		return nil
	}
	out := map[string]any{}
	switch {
	case r.MaxTokens > 0:
		out["max_tokens"] = r.MaxTokens
	case r.Effort != "":
		out["effort"] = r.Effort
	}
	if r.Exclude {
		out["exclude"] = true
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// budget returns the thinking budget in tokens (0 when unknown).
func (r *Reasoning) budget() int {
	if r == nil {
		return 0
	}
	if r.MaxTokens > 0 {
		return r.MaxTokens
	}
	return effortBudgets[r.Effort]
}

// EffortBudget exposes the token budget an effort maps to for budget-based
// backends (0 when the effort is unknown).
func EffortBudget(effort string) int { return effortBudgets[effort] }

// SupportsReasoning reports whether Factor can send reasoning parameters to a
// provider type. The wizard uses it to decide whether to ask.
func SupportsReasoning(providerType string) bool {
	switch providerType {
	case "ollama", "lmstudio", "llamacpp":
		return false
	}
	return true
}

// reasoningDialect picks the wire format for a provider type.
func reasoningDialect(providerType string) string {
	switch providerType {
	case "openai", "groq":
		return "effort" // first-party OpenAI-style: reasoning_effort
	case "ollama", "lmstudio", "llamacpp":
		return "" // local servers reject unknown fields more often than not
	default:
		return "object" // OpenRouter and compatible gateways
	}
}
