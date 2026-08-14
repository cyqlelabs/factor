package phone

import (
	"context"
	"encoding/json"
	"fmt"
)

// Status is what `factor status` reports about the voice channel: whether it
// is configured, which speech tier it runs, whether the voice shell is
// installed, and whether it is answering right now.
type Status struct {
	Configured bool
	Enabled    bool
	Number     string
	Tier       string
	Python     string // interpreter the voice shell runs in; "" when not installed
	Healthy    bool
	Problem    string
}

// Describe reads a channels.phone section and probes the voice shell. It never
// fails: a section that cannot be used is reported, not returned as an error.
func Describe(ctx context.Context, raw json.RawMessage, home string) Status {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Status{Configured: true, Problem: fmt.Sprintf("unreadable section: %v", err)}
	}
	cfg.applyDefaults()

	status := Status{
		Configured: true,
		Enabled:    cfg.Enabled == nil || *cfg.Enabled,
		Number:     cfg.PhoneNumber,
		Tier:       cfg.TierLabel(),
	}
	if err := cfg.validate(); err != nil {
		status.Problem = err.Error()
		return status
	}
	if path, ok := FindVoiceShellPython(home); ok {
		status.Python = path
	}
	base := cfg.ControlAPIBase
	if base == "" {
		base = fmt.Sprintf("http://127.0.0.1:%d", cfg.SidecarPort)
	}
	if err := newControlClient(base).health(ctx); err != nil {
		status.Problem = "the voice shell is not answering (run `factor gateway`)"
		return status
	}
	status.Healthy = true
	return status
}

// Line renders the status as one terminal line.
func (s Status) Line() string {
	switch {
	case !s.Enabled:
		return "disabled in the config"
	case s.Problem != "":
		return fmt.Sprintf("%s · %s — %s", s.Number, s.Tier, s.Problem)
	case s.Healthy:
		return fmt.Sprintf("%s · %s — voice shell healthy", s.Number, s.Tier)
	default:
		return fmt.Sprintf("%s · %s — voice shell not running", s.Number, s.Tier)
	}
}
