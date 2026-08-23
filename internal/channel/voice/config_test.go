package voice

import (
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/channel/phone"
)

// validConfig is the smallest cloud-tier section that passes validation.
func validConfig() Config {
	return Config{STTAPIKey: "dg-key", ElevenLabsAPIKey: "el-key"}
}

func TestApplyDefaultsFillsEveryKnob(t *testing.T) {
	cfg := validConfig()
	cfg.applyDefaults()

	if cfg.Language != "en" || cfg.Activation != activationAlways {
		t.Errorf("language=%q activation=%q", cfg.Language, cfg.Activation)
	}
	if cfg.STT.Provider != providerDeepgram || cfg.TTS.Provider != providerElevenLabs {
		t.Errorf("providers = %q/%q, want the cloud tier", cfg.STT.Provider, cfg.TTS.Provider)
	}
	if cfg.ControlPort != defaultControlPort {
		t.Errorf("control port = %d", cfg.ControlPort)
	}
	if cfg.SpeechServer.Port != defaultSpeechPort {
		t.Errorf("speech port = %d, want %d — clear of the phone channel's server", cfg.SpeechServer.Port, defaultSpeechPort)
	}
	if cfg.FollowUpSeconds != defaultFollowUpSeconds || cfg.SilenceMs != defaultSilenceMs {
		t.Errorf("follow_up=%d silence=%d", cfg.FollowUpSeconds, cfg.SilenceMs)
	}
	if cfg.VADRatio != defaultVADRatio || cfg.BargeRatio != defaultBargeRatio {
		t.Errorf("ratios = %v/%v", cfg.VADRatio, cfg.BargeRatio)
	}
	if cfg.OutputVolume != 100 {
		t.Errorf("output volume = %d, want full volume by default", cfg.OutputVolume)
	}
	if cfg.SpeakerThreshold != defaultSpeakerThreshold || cfg.UnknownSpeaker != unknownAnonymous {
		t.Errorf("speaker knobs = %v/%q", cfg.SpeakerThreshold, cfg.UnknownSpeaker)
	}
	if err := cfg.validate(); err != nil {
		t.Errorf("a defaulted cloud config does not validate: %v", err)
	}
}

func TestApplyDefaultsNamesTheWakeWordOnlyInWakeWordMode(t *testing.T) {
	cfg := Config{Activation: "wake-word"}
	cfg.applyDefaults()
	if cfg.WakeWord != defaultWakeWord {
		t.Errorf("wake word = %q", cfg.WakeWord)
	}
	other := Config{}
	other.applyDefaults()
	if other.WakeWord != "" {
		t.Errorf("always-on mode grew a wake word %q", other.WakeWord)
	}
}

// A local tier with no server named is Factor's own managed server.
func TestApplyDefaultsResolvesTheManagedSpeechServer(t *testing.T) {
	cfg := Config{
		STT: phone.AudioEndpoint{Provider: providerLocalOpenAI},
		TTS: phone.AudioEndpoint{Provider: providerLocalOpenAI},
	}
	cfg.applyDefaults()
	want := phone.SpeechBaseURL(cfg.SpeechServer)
	if cfg.STT.BaseURL != want || cfg.TTS.BaseURL != want {
		t.Errorf("base urls = %q/%q, want %q", cfg.STT.BaseURL, cfg.TTS.BaseURL, want)
	}
	if !cfg.managedSpeech() {
		t.Error("a blank base_url on a local tier is Factor's own server")
	}
	if cfg.Tier() != 4 {
		t.Errorf("tier = %d", cfg.Tier())
	}

	// A user-run server is not managed and never gets the boot secret.
	byo := Config{STT: phone.AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: "http://127.0.0.1:8000/v1"},
		ElevenLabsAPIKey: "el"}
	byo.applyDefaults()
	if byo.managedSpeech() {
		t.Error("a user-run server was treated as managed")
	}
}

func TestValidateRejectsWhatWouldFailMidConversation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"unknown activation", func(c *Config) { c.Activation = "sometimes" }, "unknown activation"},
		{"deepgram without a key", func(c *Config) { c.STTAPIKey = "" }, "stt_api_key"},
		{"whisper without a key", func(c *Config) { c.STT.Provider = providerWhisper; c.STTAPIKey = "" }, "stt_api_key"},
		{"unknown stt provider", func(c *Config) { c.STT.Provider = "azure" }, "unknown stt.provider"},
		{"elevenlabs without a key", func(c *Config) { c.ElevenLabsAPIKey = "" }, "elevenlabs_api_key"},
		{"unknown tts provider", func(c *Config) { c.TTS.Provider = "azure" }, "unknown tts.provider"},
		{"volume beyond full", func(c *Config) { c.OutputVolume = 130 }, "output_volume"},
		{"speaker id without the managed server", func(c *Config) { c.SpeakerID = true }, "speaker_id"},
		{"unknown unknown_speaker", func(c *Config) { c.UnknownSpeaker = "sometimes" }, "unknown_speaker"},
		{"speaker threshold beyond cosine", func(c *Config) { c.SpeakerThreshold = 1.5 }, "speaker_threshold"},
		{"a pace no voice can carry", func(c *Config) { c.SpeechServer.SpeechSpeed = 12 }, "speech_speed"},
		{"speech port colliding with control", func(c *Config) {
			c.STT = phone.AudioEndpoint{Provider: providerLocalOpenAI}
			c.SpeechServer.Port = 8730
			c.ControlPort = 8730
		}, "collides"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			cfg.applyDefaults()
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validate() = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestTierLabels(t *testing.T) {
	local := phone.AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: "http://127.0.0.1:1/v1"}
	cases := []struct {
		cfg  Config
		want string
	}{
		{validConfig(), "tier 1"},
		{Config{STT: local, ElevenLabsAPIKey: "el"}, "tier 2"},
		{Config{TTS: local, STTAPIKey: "dg"}, "tier 3"},
		{Config{STT: local, TTS: local}, "tier 4"},
	}
	for _, tc := range cases {
		tc.cfg.applyDefaults()
		if got := tc.cfg.TierLabel(); !strings.HasPrefix(got, tc.want) {
			t.Errorf("TierLabel() = %q, want prefix %q", got, tc.want)
		}
	}
}

func TestSpeakerIDValidatesOnAManagedTier(t *testing.T) {
	cfg := Config{
		STT:              phone.AudioEndpoint{Provider: providerLocalOpenAI},
		ElevenLabsAPIKey: "el",
		SpeakerID:        true,
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		t.Errorf("speaker_id on a managed tier was rejected: %v", err)
	}
}
