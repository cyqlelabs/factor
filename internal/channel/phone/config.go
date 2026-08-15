// Package phone is the voice connector: the user talks to Factor on a real
// phone call, and Factor can call or text back. The voice shell is Patter
// (Python), supervised exactly like the smrti memory sidecar; Factor itself is
// the brain, plugged in through Patter's CustomLLM seam as an
// OpenAI-compatible endpoint on loopback. SMS goes straight to the carrier's
// REST API from Go. Config section:
//
//	"channels": {"phone": {"user_number": "+15550001234", ...}}
//
// Everything is optional: no channels.phone section, nothing runs.
package phone

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	// The voice shell owns two ports: sidecar_port for the control API Factor
	// drives, and the next one up for Patter's own carrier-facing server. The
	// bridge sits clear of both.
	defaultSidecarPort    = 8722
	defaultBridgePort     = 8724
	defaultMaxCallMinutes = 15
	defaultLanguage       = "en"

	// maxMessageLength keeps proactive messages inside a handful of SMS
	// segments; the channel manager chunks anything longer.
	maxMessageLength = 1500
)

// Audio provider identifiers. "local-openai" is any OpenAI-compatible server
// the user runs themselves (Speaches, whisper-server, …) reached over base_url.
const (
	providerDeepgram    = "deepgram"
	providerWhisper     = "whisper"
	providerElevenLabs  = "elevenlabs"
	providerLocalOpenAI = "local-openai"
)

// Proactive delivery modes for bus-delivered outbound messages.
const (
	proactiveSMS  = "sms"
	proactiveCall = "call"
	proactiveOff  = "off"
)

// AudioEndpoint selects one speech provider. BaseURL is only read for
// local-openai, where it points at the user's own server; Model and Voice name
// the artifacts that server serves (a faster-whisper size, a Piper/Kokoro
// voice).
type AudioEndpoint struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url,omitempty"`
	Model    string `json:"model,omitempty"`
	Voice    string `json:"voice,omitempty"`
}

// Config is the channels.phone section. Every secret is a top-level string so
// the config layer's (non-recursive) secret scrubber redacts it from tool
// output for free — do not nest them.
type Config struct {
	Enabled *bool `json:"enabled,omitempty"`

	UserNumber  string   `json:"user_number"`
	AllowFrom   []string `json:"allow_from,omitempty"`
	AllowCallTo []string `json:"allow_call_to,omitempty"`

	Carrier          string `json:"carrier"`
	PhoneNumber      string `json:"phone_number"`
	TwilioAccountSID string `json:"twilio_account_sid"`
	TwilioAuthToken  string `json:"twilio_auth_token"`

	ElevenLabsAPIKey string `json:"elevenlabs_api_key"`
	VoiceID          string `json:"voice_id,omitempty"`
	Language         string `json:"language"`

	STT       AudioEndpoint `json:"stt"`
	STTAPIKey string        `json:"stt_api_key"`
	TTS       AudioEndpoint `json:"tts"`

	// SpeechServer tunes the local speech server Factor runs itself. It is
	// what an empty base_url on a local tier resolves to: choosing a local
	// tier is a request for local speech, not for a server to go and install.
	SpeechServer SpeechConfig `json:"speech_server,omitempty"`

	// LocalAudioFallback falls back to the cloud tier when a configured local
	// speech server is unreachable at startup, instead of failing every call.
	// nil means enabled.
	LocalAudioFallback *bool `json:"local_audio_fallback,omitempty"`

	Proactive      string `json:"proactive"`
	MaxCallMinutes int    `json:"max_call_minutes"`

	Tunnel     string `json:"tunnel"`
	WebhookURL string `json:"webhook_url,omitempty"`

	SidecarPort int `json:"sidecar_port"`
	BridgePort  int `json:"bridge_port"`

	// Command overrides the Python interpreter that runs the voice shell.
	// AutoInstall (nil means on) creates a private venv and installs Patter.
	Command     string `json:"command,omitempty"`
	AutoInstall *bool  `json:"auto_install,omitempty"`

	// Test overrides, following the Telegram precedent: the Twilio REST base
	// and the voice shell's control API. A control_api_base skips spawning the
	// sidecar entirely and talks to whatever is already listening there.
	APIBase        string `json:"api_base,omitempty"`
	ControlAPIBase string `json:"control_api_base,omitempty"`
}

func (c Config) autoInstall() bool {
	return c.AutoInstall == nil || *c.AutoInstall
}

func (c Config) localAudioFallback() bool {
	return c.LocalAudioFallback == nil || *c.LocalAudioFallback
}

// e164 is the wire format every carrier expects: a plus, a non-zero country
// digit, and up to fourteen more digits.
var e164 = regexp.MustCompile(`^\+[1-9]\d{6,14}$`)

// normalizeNumber strips the punctuation humans type into phone numbers so
// "+1 (555) 000-1234" and "+15550001234" are the same number everywhere.
func normalizeNumber(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '+' && b.Len() == 0:
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func validNumber(s string) bool { return e164.MatchString(s) }

// applyDefaults fills every unset knob. It runs before validate so a section
// containing nothing but the two phone numbers and the carrier credentials is
// a complete configuration.
func (c *Config) applyDefaults() {
	c.UserNumber = normalizeNumber(c.UserNumber)
	c.PhoneNumber = normalizeNumber(c.PhoneNumber)
	for i, n := range c.AllowFrom {
		if strings.TrimSpace(n) == anyCaller {
			c.AllowFrom[i] = anyCaller
			continue
		}
		c.AllowFrom[i] = normalizeNumber(n)
	}
	for i, n := range c.AllowCallTo {
		c.AllowCallTo[i] = normalizeNumber(n)
	}
	if c.Carrier == "" {
		c.Carrier = "twilio"
	}
	if c.Language == "" {
		c.Language = defaultLanguage
	}
	if c.STT.Provider == "" {
		c.STT.Provider = providerDeepgram
	}
	if c.TTS.Provider == "" {
		c.TTS.Provider = providerElevenLabs
	}
	if c.Proactive == "" {
		c.Proactive = proactiveSMS
	}
	if c.MaxCallMinutes <= 0 {
		c.MaxCallMinutes = defaultMaxCallMinutes
	}
	if c.Tunnel == "" {
		c.Tunnel = "quick"
	}
	if c.SidecarPort <= 0 {
		c.SidecarPort = defaultSidecarPort
	}
	if c.BridgePort <= 0 {
		c.BridgePort = defaultBridgePort
	}
	c.STT.BaseURL = strings.TrimSuffix(strings.TrimSpace(c.STT.BaseURL), "/")
	c.TTS.BaseURL = strings.TrimSuffix(strings.TrimSpace(c.TTS.BaseURL), "/")
	// A local tier with no server named is Factor's own, which is the case the
	// wizard writes: pointing the endpoints at it here means everything
	// downstream — validation, the startup probe, the shell config — goes on
	// treating it as any other OpenAI-compatible server.
	if c.STT.Provider == providerLocalOpenAI && c.STT.BaseURL == "" {
		c.STT.BaseURL = speechBaseURL(c.SpeechServer)
	}
	if c.TTS.Provider == providerLocalOpenAI && c.TTS.BaseURL == "" {
		c.TTS.BaseURL = speechBaseURL(c.SpeechServer)
	}
	c.APIBase = strings.TrimSuffix(strings.TrimSpace(c.APIBase), "/")
	c.ControlAPIBase = strings.TrimSuffix(strings.TrimSpace(c.ControlAPIBase), "/")
	c.WebhookURL = strings.TrimSuffix(strings.TrimSpace(c.WebhookURL), "/")
}

// validate rejects a section that could only fail later, on a live call.
func (c Config) validate() error {
	if !validNumber(c.UserNumber) {
		return fmt.Errorf("user_number %q is not E.164 (e.g. +15550001234)", c.UserNumber)
	}
	if !validNumber(c.PhoneNumber) {
		return fmt.Errorf("phone_number %q is not E.164 (the number you bought at the carrier)", c.PhoneNumber)
	}
	for _, n := range c.AllowFrom {
		if n != anyCaller && !validNumber(n) {
			return fmt.Errorf("allow_from entry %q is neither E.164 nor %q", n, anyCaller)
		}
	}
	// There is deliberately no wildcard for dialling out: an agent that can be
	// talked into calling any number is a toll-fraud engine.
	for _, n := range c.AllowCallTo {
		if !validNumber(n) {
			return fmt.Errorf("allow_call_to entry %q is not E.164", n)
		}
	}
	if c.Carrier != "twilio" {
		return fmt.Errorf("carrier %q is not wired yet (only twilio is)", c.Carrier)
	}
	if c.TwilioAccountSID == "" || c.TwilioAuthToken == "" {
		return fmt.Errorf("twilio_account_sid and twilio_auth_token are required")
	}
	switch c.STT.Provider {
	case providerDeepgram, providerWhisper:
		if c.STTAPIKey == "" {
			return fmt.Errorf("stt_api_key is required for stt.provider %q", c.STT.Provider)
		}
	case providerLocalOpenAI:
		if c.STT.BaseURL == "" {
			return fmt.Errorf("stt.base_url is required for a local speech-to-text server")
		}
	default:
		return fmt.Errorf("unknown stt.provider %q (want %s, %s, or %s)",
			c.STT.Provider, providerDeepgram, providerWhisper, providerLocalOpenAI)
	}
	switch c.TTS.Provider {
	case providerElevenLabs:
		if c.ElevenLabsAPIKey == "" {
			return fmt.Errorf("elevenlabs_api_key is required for tts.provider %q", providerElevenLabs)
		}
	case providerLocalOpenAI:
		if c.TTS.BaseURL == "" {
			return fmt.Errorf("tts.base_url is required for a local text-to-speech server")
		}
	default:
		return fmt.Errorf("unknown tts.provider %q (want %s or %s)",
			c.TTS.Provider, providerElevenLabs, providerLocalOpenAI)
	}
	switch c.Proactive {
	case proactiveSMS, proactiveCall, proactiveOff:
	default:
		return fmt.Errorf("unknown proactive %q (want %s, %s, or %s)",
			c.Proactive, proactiveSMS, proactiveCall, proactiveOff)
	}
	switch c.Tunnel {
	case "quick":
	case "none":
		if c.WebhookURL == "" {
			return fmt.Errorf("webhook_url is required when tunnel is \"none\"")
		}
	default:
		return fmt.Errorf("unknown tunnel %q (want \"quick\" or \"none\")", c.Tunnel)
	}
	if c.BridgePort == c.SidecarPort || c.BridgePort == c.webhookPort() {
		return fmt.Errorf("bridge_port %d collides with the voice shell's control port %d or its webhook port %d",
			c.BridgePort, c.SidecarPort, c.webhookPort())
	}
	if c.managedSpeech() {
		speech := speechPort(c.SpeechServer)
		for _, taken := range [](struct {
			port int
			name string
		}){
			{c.SidecarPort, "the voice shell's control port"},
			{c.webhookPort(), "the voice shell's webhook port"},
			{c.BridgePort, "bridge_port"},
		} {
			if speech == taken.port {
				return fmt.Errorf("speech_server.port %d collides with %s", speech, taken.name)
			}
		}
	}
	return nil
}

// localSTT and localTTS report which halves of the pipeline run on this
// machine, which is what decides the weights the speech server has to load.
func (c Config) localSTT() bool { return c.STT.Provider == providerLocalOpenAI }
func (c Config) localTTS() bool { return c.TTS.Provider == providerLocalOpenAI }

// managedSpeech reports whether a local tier is served by the speech server
// Factor runs itself, rather than one the user pointed it at. Only the managed
// one is installed, supervised, and handed this boot's secret.
func (c Config) managedSpeech() bool {
	managed := speechBaseURL(c.SpeechServer)
	return (c.localSTT() && c.STT.BaseURL == managed) || (c.localTTS() && c.TTS.BaseURL == managed)
}

// webhookPort is where Patter's own server listens — the port the carrier
// reaches through the tunnel. It sits next to the control port so a user only
// ever has to think about one number.
func (c Config) webhookPort() int { return c.SidecarPort + 1 }

// Tier reports which of the documented voice pipeline tiers this config
// selects: 1 all-cloud audio, 2 local speech-to-text, 3 local text-to-speech,
// 4 fully local audio.
func (c Config) Tier() int {
	localSTT := c.STT.Provider == providerLocalOpenAI
	localTTS := c.TTS.Provider == providerLocalOpenAI
	switch {
	case localSTT && localTTS:
		return 4
	case localTTS:
		return 3
	case localSTT:
		return 2
	default:
		return 1
	}
}

// TierLabel describes the tier for status output and the setup wizard.
func (c Config) TierLabel() string {
	switch c.Tier() {
	case 4:
		return "tier 4 · fully local audio"
	case 3:
		return "tier 3 · local text-to-speech"
	case 2:
		return "tier 2 · local speech-to-text"
	default:
		return "tier 1 · cloud audio"
	}
}

// anyCaller opens the line to everyone — the Telegram-style "no allowlist"
// escape hatch, spelled explicitly because for a phone number the default has
// to be closed.
const anyCaller = "*"

// allowAnyCaller reports the wildcard, which is worth a security warning.
func (c Config) allowAnyCaller() bool {
	for _, n := range c.AllowFrom {
		if n == anyCaller {
			return true
		}
	}
	return false
}

// inboundAllowed reports whether a caller may reach the agent. The owner's
// number is always allowed; an otherwise empty allowlist means only the owner.
func (c Config) inboundAllowed(number string) bool {
	return c.allowAnyCaller() || c.allowed(number, c.AllowFrom)
}

// outboundAllowed reports whether the agent may dial a number.
func (c Config) outboundAllowed(number string) bool {
	return c.allowed(number, c.AllowCallTo)
}

func (c Config) allowed(number string, extra []string) bool {
	number = normalizeNumber(number)
	if number == "" {
		return false
	}
	if number == c.UserNumber {
		return true
	}
	for _, n := range extra {
		if n == number {
			return true
		}
	}
	return false
}

// inboundAllowlist is the effective set of numbers that may call in, owner
// first — what the voice shell enforces and what the logs report.
func (c Config) inboundAllowlist() []string {
	if c.allowAnyCaller() {
		return []string{anyCaller}
	}
	out := []string{c.UserNumber}
	for _, n := range c.AllowFrom {
		if n != c.UserNumber {
			out = append(out, n)
		}
	}
	return out
}
