package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Status is what `factor status` reports about the PC voice channel: whether
// it is configured, which speech tier and activation it runs, whether the
// audio helpers exist, and whether a process is listening right now.
type Status struct {
	Configured bool
	Enabled    bool
	Tier       string
	Activation string
	Problem    string

	// MissingHelpers names audio helpers the machine lacks; empty means both
	// directions of audio have one.
	MissingHelpers []string

	// Listening reports whether a running factor answers on the control
	// endpoint; Reason is what it says is wrong when it cannot listen.
	Listening bool
	Reason    string
}

// Describe reads a channels.voice section and probes the control endpoint. It
// never fails: a section that cannot be used is reported, not returned as an
// error.
func Describe(ctx context.Context, raw json.RawMessage, env Env) Status {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Status{Configured: true, Enabled: true, Problem: fmt.Sprintf("unreadable section: %v", err)}
	}
	cfg.applyDefaults()

	status := Status{
		Configured: true,
		Enabled:    cfg.Enabled == nil || *cfg.Enabled,
		Tier:       cfg.TierLabel(),
		Activation: cfg.Activation,
	}
	if !status.Enabled {
		return status
	}
	if err := cfg.validate(); err != nil {
		status.Problem = err.Error()
		return status
	}
	for _, helper := range MissingHelpers(env) {
		status.MissingHelpers = append(status.MissingHelpers, helper.Bin)
	}

	health, err := probeControl(ctx, cfg.ControlPort)
	if err != nil {
		return status
	}
	status.Listening = health.Status == "ok"
	status.Reason = health.Reason
	return status
}

type controlHealth struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func probeControl(ctx context.Context, port int) (controlHealth, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
	if err != nil {
		return controlHealth{}, err
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return controlHealth{}, err
	}
	defer resp.Body.Close()
	var health controlHealth
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&health); err != nil {
		return controlHealth{}, err
	}
	return health, nil
}

// Line renders the status as one terminal line.
func (s Status) Line() string {
	switch {
	case !s.Enabled:
		return "disabled in the config"
	case s.Problem != "":
		return fmt.Sprintf("%s — %s", s.Tier, s.Problem)
	case len(s.MissingHelpers) > 0:
		return fmt.Sprintf("%s · %s — missing %s", s.Tier, s.Activation, strings.Join(s.MissingHelpers, ", "))
	case s.Listening:
		return fmt.Sprintf("%s · %s — listening", s.Tier, s.Activation)
	case s.Reason != "":
		return fmt.Sprintf("%s · %s — %s", s.Tier, s.Activation, s.Reason)
	default:
		return fmt.Sprintf("%s · %s — not listening (run `factor` or `factor gateway`)", s.Tier, s.Activation)
	}
}

// Talk arms push-to-talk on whatever process is running this channel — how
// `factor talk` in a terminal reaches the daemon or a chat session.
func Talk(ctx context.Context, raw json.RawMessage) error {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("voice config: %w", err)
	}
	cfg.applyDefaults()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/ptt", cfg.ControlPort), nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("the voice channel is not listening (start `factor` or `factor gateway` first): %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("the voice channel refused: HTTP %d", resp.StatusCode)
	}
	return nil
}
