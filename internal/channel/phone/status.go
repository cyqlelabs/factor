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

	// Speech describes the local speech stack when Factor runs one: what is
	// installed, and whether it is answering. A local tier whose server is
	// down still takes calls — on the cloud tier — so this is the difference
	// between "quieter than you asked for" and "broken".
	Speech          string
	SpeechInstalled bool
	SpeechHealthy   bool
}

// Describe reads a channels.phone section and probes the voice shell. It never
// fails: a section that cannot be used is reported, not returned as an error.
func Describe(ctx context.Context, raw json.RawMessage, home string) Status {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		// Enabled is a guess here — a section that will not parse cannot say
		// whether it is switched on — but reporting the parse error is the
		// whole point, and "disabled" would hide it.
		return Status{Configured: true, Enabled: true, Problem: fmt.Sprintf("unreadable section: %v", err)}
	}
	cfg.applyDefaults()

	status := Status{
		Configured: true,
		Enabled:    cfg.Enabled == nil || *cfg.Enabled,
		Number:     cfg.PhoneNumber,
		Tier:       cfg.TierLabel(),
	}
	if !status.Enabled {
		return status
	}
	if err := cfg.validate(); err != nil {
		status.Problem = err.Error()
		return status
	}
	if path, ok := FindVoiceShellPython(home); ok {
		status.Python = path
	} else if cfg.Command != "" {
		// An interpreter named in the config is the one the shell runs in,
		// whether or not Factor ever built the private virtualenv.
		status.Python = cfg.Command
	}
	if cfg.managedSpeech() {
		status.describeSpeech(ctx, cfg, home)
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

// describeSpeech reports the speech server Factor runs itself: what the
// installer left on disk, and whether it is answering now.
func (s *Status) describeSpeech(ctx context.Context, cfg Config, home string) {
	s.Speech = SpeechChoices{
		SttEngine:     cfg.SpeechServer.SttEngine,
		SttModel:      cfg.SpeechServer.SttModel,
		WhisperModel:  cfg.SpeechServer.WhisperModel,
		WhisperDevice: cfg.SpeechServer.WhisperDevice,
		PiperVoice:    cfg.SpeechServer.PiperVoice,
	}.Summary()

	// An interpreter named in the config counts as installed: the user pointed
	// Factor at one, so reporting the private virtualenv as missing would be
	// answering a question nobody asked.
	if _, found := FindSpeechPython(home); found || cfg.SpeechServer.Command != "" {
		s.SpeechInstalled = true
	}

	// Health is the ground truth, so it is probed either way. A server that is
	// answering is working, whatever this process can find on disk.
	client := newControlClient(fmt.Sprintf("http://127.0.0.1:%d", speechPort(cfg.SpeechServer)))
	if client.health(ctx) == nil {
		s.SpeechHealthy = true
		s.SpeechInstalled = true
	}
}

// Line renders the status as one terminal line.
func (s Status) Line() string {
	switch {
	case !s.Enabled:
		return "disabled in the config"
	case s.Problem != "":
		return fmt.Sprintf("%s · %s — %s", s.Number, s.Tier, s.Problem)
	case s.Healthy:
		return fmt.Sprintf("%s · %s — voice shell healthy%s", s.Number, s.Tier, s.speechSuffix())
	default:
		return fmt.Sprintf("%s · %s — voice shell not running%s", s.Number, s.Tier, s.speechSuffix())
	}
}

// speechSuffix says where the local speech stack stands, and only when there
// is one: a cloud tier has nothing to report.
func (s Status) speechSuffix() string {
	switch {
	case s.Speech == "":
		return ""
	case !s.SpeechInstalled:
		return " · speech not installed yet"
	case !s.SpeechHealthy:
		return fmt.Sprintf(" · speech (%s) not answering", s.Speech)
	default:
		return fmt.Sprintf(" · speech %s", s.Speech)
	}
}
