package phone

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Everything the voice shell needs arrives as one JSON blob in its
// environment: credentials, the speech tier, the allowlists, and where to
// reach the bridge. Rendering it here — in Go, with tests — keeps the Python
// side a thin, dumb executor of decisions Factor already made.

// shellCarrier carries whichever credentials the chosen carrier needs; the
// other carrier's fields are left out entirely rather than sent empty.
type shellCarrier struct {
	Name        string `json:"name"`
	PhoneNumber string `json:"phone_number"`

	AccountSID string `json:"account_sid,omitempty"`
	AuthToken  string `json:"auth_token,omitempty"`

	APIKey       string `json:"api_key,omitempty"`
	ConnectionID string `json:"connection_id,omitempty"`
	PublicKey    string `json:"public_key,omitempty"`
}

type shellSTT struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url,omitempty"`
	Model    string `json:"model,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	Language string `json:"language,omitempty"`
}

type shellTTS struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url,omitempty"`
	Model    string `json:"model,omitempty"`
	Voice    string `json:"voice,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	Format   string `json:"format"`
}

type shellConfig struct {
	ControlHost string `json:"control_host"`
	ControlPort int    `json:"control_port"`
	// WebhookPort is Patter's own server, which the carrier reaches through
	// the tunnel; the control port next door is Factor's alone.
	WebhookPort int `json:"webhook_port"`

	BridgeURL   string `json:"bridge_url"`
	BridgeToken string `json:"bridge_token"`

	Carrier shellCarrier `json:"carrier"`
	STT     shellSTT     `json:"stt"`
	TTS     shellTTS     `json:"tts"`

	Language    string   `json:"language"`
	UserNumber  string   `json:"user_number"`
	AllowFrom   []string `json:"allow_from"`
	AllowCallTo []string `json:"allow_call_to"`

	MaxCallSeconds     int    `json:"max_call_seconds"`
	BargeInThresholdMs int    `json:"barge_in_threshold_ms"`
	FirstMessage       string `json:"first_message"`
	LongTurnMessage    string `json:"long_turn_message"`
	MachineDetection   bool   `json:"machine_detection"`
	VoicemailMessage   string `json:"voicemail_message"`

	Tunnel     string `json:"tunnel"`
	WebhookURL string `json:"webhook_url,omitempty"`
	Dashboard  bool   `json:"dashboard"`

	Tier int `json:"tier"`
}

// Speech defaults. flash_v2_5 is ElevenLabs' lowest-latency model, and the
// format is whatever the carrier negotiates, so the shell hands it bytes it can
// send without transcoding: Twilio media streams are μ-law at 8 kHz, Telnyx
// takes linear PCM at 16 kHz.
const (
	deepgramModel   = "nova-3"
	whisperModel    = "whisper-1"
	elevenLabsModel = "eleven_flash_v2_5"
	telephonyFormat = "ulaw_8000"
	telnyxFormat    = "pcm_16000"

	// The local tier's wire format is headerless PCM, because that is what
	// Patter's OpenAI adapter requests and resamples.
	localAudioFmt = "pcm"

	// localTTSModel names the protocol rather than the weights: the voice is
	// chosen on the speech server, and this only has to be a model id the
	// OpenAI dialect accepts.
	localTTSModel = "tts-1"

	// bargeInThresholdMs is Patter's default: long enough that a cough does
	// not cut the agent off, short enough that talking over it feels natural.
	bargeInThresholdMs = 300
)

// renderShellConfig projects the channel config onto the voice shell's own.
func renderShellConfig(cfg Config, bridgePort int, bridgeToken string) shellConfig {
	stt := shellSTT{Provider: cfg.STT.Provider, Model: cfg.STT.Model, Language: cfg.Language}
	switch cfg.STT.Provider {
	case providerDeepgram:
		stt.APIKey = cfg.STTAPIKey
		if stt.Model == "" {
			stt.Model = deepgramModel
		}
	case providerWhisper:
		stt.APIKey = cfg.STTAPIKey
		if stt.Model == "" {
			stt.Model = whisperModel
		}
	case providerLocalOpenAI:
		// A local server must never be handed the cloud key kept for fallback.
		// Factor's own gets this boot's secret, the way the bridge does; a
		// server the user pointed us at gets nothing. Model ids are the
		// server's own (faster-whisper sizes, Hugging Face repo names), so
		// pass through whatever was configured and let it default otherwise.
		stt.BaseURL = cfg.STT.BaseURL
		if cfg.managedSpeech() {
			stt.APIKey = bridgeToken
		}
	}

	tts := shellTTS{Provider: cfg.TTS.Provider, Model: cfg.TTS.Model, Voice: cfg.TTS.Voice}
	switch cfg.TTS.Provider {
	case providerElevenLabs:
		tts.APIKey = cfg.ElevenLabsAPIKey
		tts.Format = carrierAudioFormat(cfg.Carrier)
		if tts.Model == "" {
			tts.Model = elevenLabsModel
		}
		if tts.Voice == "" {
			tts.Voice = cfg.VoiceID
		}
	case providerLocalOpenAI:
		tts.BaseURL = cfg.TTS.BaseURL
		// Patter's OpenAI adapter asks for "pcm" and resamples 24 kHz down to
		// the phone band, whatever this says; the field is here so the shell
		// config records what actually crosses the wire.
		tts.Format = localAudioFmt
		if tts.Model == "" {
			tts.Model = localTTSModel
		}
		if cfg.managedSpeech() {
			tts.APIKey = bridgeToken
		}
	}

	return shellConfig{
		ControlHost:        "127.0.0.1",
		ControlPort:        cfg.SidecarPort,
		WebhookPort:        cfg.webhookPort(),
		BridgeURL:          fmt.Sprintf("http://127.0.0.1:%d", bridgePort),
		BridgeToken:        bridgeToken,
		Carrier:            renderCarrier(cfg),
		STT:                stt,
		TTS:                tts,
		Language:           cfg.Language,
		UserNumber:         cfg.UserNumber,
		AllowFrom:          cfg.inboundAllowlist(),
		AllowCallTo:        cfg.outboundAllowlist(),
		MaxCallSeconds:     cfg.MaxCallMinutes * 60,
		BargeInThresholdMs: bargeInThresholdMs,
		FirstMessage:       firstMessage(cfg.Language),
		LongTurnMessage:    longTurnMessage(cfg.Language),
		MachineDetection:   true,
		VoicemailMessage:   voicemailMessage(cfg.Language),
		Tunnel:             cfg.Tunnel,
		WebhookURL:         cfg.WebhookURL,
		// Patter's dashboard fails open without a token, so it stays off.
		Dashboard: false,
		Tier:      cfg.Tier(),
	}
}

// renderCarrier hands the shell the credentials its carrier actually uses.
func renderCarrier(cfg Config) shellCarrier {
	carrier := shellCarrier{Name: cfg.Carrier, PhoneNumber: cfg.PhoneNumber}
	if cfg.Carrier == carrierTelnyx {
		carrier.APIKey = cfg.TelnyxAPIKey
		carrier.ConnectionID = cfg.TelnyxConnectionID
		carrier.PublicKey = cfg.TelnyxPublicKey
		return carrier
	}
	carrier.AccountSID = cfg.TwilioAccountSID
	carrier.AuthToken = cfg.TwilioAuthToken
	return carrier
}

// carrierAudioFormat is the wire format the carrier negotiates, which is what
// the speech synthesiser should emit so nothing has to transcode it on the way.
func carrierAudioFormat(carrier string) string {
	if carrier == carrierTelnyx {
		return telnyxFormat
	}
	return telephonyFormat
}

// outboundAllowlist is who the agent may dial, owner first.
func (c Config) outboundAllowlist() []string {
	out := []string{c.UserNumber}
	for _, n := range c.AllowCallTo {
		if n != c.UserNumber {
			out = append(out, n)
		}
	}
	return out
}

// firstMessage is what the agent says when it picks up. Silence on answer
// reads as a dropped call, so there is always a greeting.
func firstMessage(language string) string {
	if isSpanish(language) {
		return "Hola, soy tu asistente. ¿En qué te ayudo?"
	}
	return "Hi, it's your assistant. What can I do for you?"
}

// longTurnMessage is the filler the shell speaks when a turn outruns the
// silence a caller will tolerate. Factor's turns are not streamed, so this is
// the difference between "thinking" and "the line went dead".
func longTurnMessage(language string) string {
	if isSpanish(language) {
		return "Dame un segundo, lo estoy viendo…"
	}
	return "Give me a second, I'm working on that…"
}

func voicemailMessage(language string) string {
	if isSpanish(language) {
		return "Hola, te llamaba tu asistente. Te vuelvo a llamar más tarde."
	}
	return "Hi, this is your assistant calling. I'll try you again later."
}

func isSpanish(language string) bool {
	return strings.HasPrefix(strings.ToLower(language), "es")
}

// ---- local audio tier resolution -------------------------------------------

// probeAudioServer reports whether a local OpenAI-compatible speech server is
// answering. Any HTTP response counts: an auth challenge still proves the
// server is there, and calls would work.
var probeAudioServer = func(ctx context.Context, baseURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// resolveAudioTier checks the local speech servers a config asks for. An
// unreachable one either falls back to the cloud tier (the default, so a
// forgotten `speaches` process degrades quality instead of killing every call)
// or takes the channel down, when the user asked for local-only.
func resolveAudioTier(ctx context.Context, cfg Config) (Config, error) {
	if cfg.STT.Provider == providerLocalOpenAI {
		if err := probeAudioServer(ctx, cfg.STT.BaseURL); err != nil {
			if !cfg.localAudioFallback() {
				return cfg, fmt.Errorf("local speech-to-text server at %s is unreachable: %w", cfg.STT.BaseURL, err)
			}
			if cfg.STTAPIKey == "" {
				return cfg, fmt.Errorf("local speech-to-text server at %s is unreachable and there is no stt_api_key to fall back to the cloud with: %w",
					cfg.STT.BaseURL, err)
			}
			slog.Warn("local speech-to-text server unreachable; falling back to the cloud tier",
				"base_url", cfg.STT.BaseURL, "error", err)
			cfg.STT = AudioEndpoint{Provider: providerDeepgram}
		}
	}
	if cfg.TTS.Provider == providerLocalOpenAI {
		if err := probeAudioServer(ctx, cfg.TTS.BaseURL); err != nil {
			if !cfg.localAudioFallback() {
				return cfg, fmt.Errorf("local text-to-speech server at %s is unreachable: %w", cfg.TTS.BaseURL, err)
			}
			if cfg.ElevenLabsAPIKey == "" {
				return cfg, fmt.Errorf("local text-to-speech server at %s is unreachable and there is no elevenlabs_api_key to fall back to the cloud with: %w",
					cfg.TTS.BaseURL, err)
			}
			slog.Warn("local text-to-speech server unreachable; falling back to the cloud tier",
				"base_url", cfg.TTS.BaseURL, "error", err)
			cfg.TTS = AudioEndpoint{Provider: providerElevenLabs, Voice: cfg.VoiceID}
		}
	}
	return cfg, nil
}
