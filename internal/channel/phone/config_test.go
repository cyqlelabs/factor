package phone

import (
	"strings"
	"testing"
)

// validConfig is the smallest section that should survive validation: two
// numbers and the credentials the default (all-cloud) tier needs.
func validConfig() Config {
	return Config{
		UserNumber:       "+15550001111",
		PhoneNumber:      "+15550002222",
		TwilioAccountSID: "AC0123456789",
		TwilioAuthToken:  "twilio-secret",
		ElevenLabsAPIKey: "eleven-secret",
		STTAPIKey:        "deepgram-secret",
	}
}

func prepared(t *testing.T, mutate func(*Config)) Config {
	t.Helper()
	cfg := validConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	cfg.applyDefaults()
	return cfg
}

func TestApplyDefaultsFillsEveryKnob(t *testing.T) {
	cfg := prepared(t, nil)

	checks := map[string]any{
		"carrier":          cfg.Carrier,
		"language":         cfg.Language,
		"stt provider":     cfg.STT.Provider,
		"tts provider":     cfg.TTS.Provider,
		"proactive":        cfg.Proactive,
		"tunnel":           cfg.Tunnel,
		"max call minutes": cfg.MaxCallMinutes,
		"sidecar port":     cfg.SidecarPort,
		"bridge port":      cfg.BridgePort,
	}
	want := map[string]any{
		"carrier":          "twilio",
		"language":         "en",
		"stt provider":     providerDeepgram,
		"tts provider":     providerElevenLabs,
		"proactive":        proactiveSMS,
		"tunnel":           "quick",
		"max call minutes": defaultMaxCallMinutes,
		"sidecar port":     defaultSidecarPort,
		"bridge port":      defaultBridgePort,
	}
	for key, got := range checks {
		if got != want[key] {
			t.Errorf("%s = %v, want %v", key, got, want[key])
		}
	}
	if err := cfg.validate(); err != nil {
		t.Errorf("a minimal section did not validate: %v", err)
	}
}

func TestApplyDefaultsNormalizesNumbers(t *testing.T) {
	cfg := prepared(t, func(c *Config) {
		c.UserNumber = "+1 (555) 000-1111"
		c.PhoneNumber = " +1.555.000.2222 "
		c.AllowFrom = []string{"+1 555 000 3333", anyCaller}
		c.AllowCallTo = []string{"+1-555-000-4444"}
	})
	if cfg.UserNumber != "+15550001111" || cfg.PhoneNumber != "+15550002222" {
		t.Errorf("numbers not normalized: %q / %q", cfg.UserNumber, cfg.PhoneNumber)
	}
	if cfg.AllowFrom[0] != "+15550003333" {
		t.Errorf("allow_from not normalized: %q", cfg.AllowFrom[0])
	}
	if cfg.AllowFrom[1] != anyCaller {
		t.Errorf("the wildcard was mangled into %q", cfg.AllowFrom[1])
	}
	if cfg.AllowCallTo[0] != "+15550004444" {
		t.Errorf("allow_call_to not normalized: %q", cfg.AllowCallTo[0])
	}
}

func TestValidateRejectsBrokenSections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"missing user number", func(c *Config) { c.UserNumber = "" }, "user_number"},
		{"user number without a plus", func(c *Config) { c.UserNumber = "15550001111" }, "user_number"},
		{"user number too short", func(c *Config) { c.UserNumber = "+1234" }, "user_number"},
		{"leading zero country code", func(c *Config) { c.UserNumber = "+05550001111" }, "user_number"},
		{"missing carrier number", func(c *Config) { c.PhoneNumber = "" }, "phone_number"},
		{"bad inbound allowlist entry", func(c *Config) { c.AllowFrom = []string{"nope"} }, "allow_from"},
		{"no wildcard for dialling out", func(c *Config) { c.AllowCallTo = []string{anyCaller} }, "allow_call_to"},
		{"unwired carrier", func(c *Config) { c.Carrier = "telnyx" }, "not wired yet"},
		{"missing twilio sid", func(c *Config) { c.TwilioAccountSID = "" }, "twilio_account_sid"},
		{"missing twilio token", func(c *Config) { c.TwilioAuthToken = "" }, "twilio_auth_token"},
		{"deepgram without a key", func(c *Config) { c.STTAPIKey = "" }, "stt_api_key"},
		{"whisper without a key", func(c *Config) {
			c.STT.Provider = providerWhisper
			c.STTAPIKey = ""
		}, "stt_api_key"},
		{"local stt without a base url", func(c *Config) { c.STT.Provider = providerLocalOpenAI }, "stt.base_url"},
		{"unknown stt provider", func(c *Config) { c.STT.Provider = "vosk" }, "unknown stt.provider"},
		{"elevenlabs without a key", func(c *Config) { c.ElevenLabsAPIKey = "" }, "elevenlabs_api_key"},
		{"local tts without a base url", func(c *Config) { c.TTS.Provider = providerLocalOpenAI }, "tts.base_url"},
		{"unknown tts provider", func(c *Config) { c.TTS.Provider = "festival" }, "unknown tts.provider"},
		{"unknown proactive mode", func(c *Config) { c.Proactive = "carrier-pigeon" }, "unknown proactive"},
		{"unknown tunnel mode", func(c *Config) { c.Tunnel = "ssh" }, "unknown tunnel"},
		{"no tunnel and no webhook url", func(c *Config) { c.Tunnel = "none" }, "webhook_url is required"},
		{"bridge port on the control port", func(c *Config) {
			c.SidecarPort = 9000
			c.BridgePort = 9000
		}, "collides"},
		{"bridge port on the webhook port", func(c *Config) {
			c.SidecarPort = 9000
			c.BridgePort = 9001
		}, "collides"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := prepared(t, c.mutate).validate()
			if err == nil {
				t.Fatalf("validate accepted a broken section (wanted %q)", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("validate error = %q, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

func TestValidateAcceptsTheLocalTiers(t *testing.T) {
	cfg := prepared(t, func(c *Config) {
		c.STT = AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: "http://127.0.0.1:8000/v1/"}
		c.TTS = AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: "http://127.0.0.1:8000/v1"}
		c.STTAPIKey = ""
		c.ElevenLabsAPIKey = ""
	})
	if err := cfg.validate(); err != nil {
		t.Fatalf("fully local tier rejected: %v", err)
	}
	if cfg.STT.BaseURL != "http://127.0.0.1:8000/v1" {
		t.Errorf("trailing slash not trimmed: %q", cfg.STT.BaseURL)
	}
	if cfg.Tier() != 4 {
		t.Errorf("Tier() = %d, want 4", cfg.Tier())
	}
}

func TestTierAndLabel(t *testing.T) {
	cases := []struct {
		name      string
		stt, tts  string
		wantTier  int
		wantLabel string
	}{
		{"all cloud", providerDeepgram, providerElevenLabs, 1, "tier 1 · cloud audio"},
		{"local stt", providerLocalOpenAI, providerElevenLabs, 2, "tier 2 · local speech-to-text"},
		{"local tts", providerDeepgram, providerLocalOpenAI, 3, "tier 3 · local text-to-speech"},
		{"fully local", providerLocalOpenAI, providerLocalOpenAI, 4, "tier 4 · fully local audio"},
		{"hosted whisper is still cloud", providerWhisper, providerElevenLabs, 1, "tier 1 · cloud audio"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{STT: AudioEndpoint{Provider: c.stt}, TTS: AudioEndpoint{Provider: c.tts}}
			if got := cfg.Tier(); got != c.wantTier {
				t.Errorf("Tier() = %d, want %d", got, c.wantTier)
			}
			if got := cfg.TierLabel(); got != c.wantLabel {
				t.Errorf("TierLabel() = %q, want %q", got, c.wantLabel)
			}
		})
	}
}

func TestAllowlistsAreClosedByDefault(t *testing.T) {
	cfg := prepared(t, func(c *Config) {
		c.AllowFrom = []string{"+15550003333"}
		c.AllowCallTo = []string{"+15550004444"}
	})

	cases := []struct {
		number            string
		inbound, outbound bool
	}{
		{"+15550001111", true, true},   // the owner is always allowed, both ways
		{"+15550003333", true, false},  // may call in, must not be called
		{"+15550004444", false, true},  // may be called, must not call in
		{"+15559999999", false, false}, // a stranger
		{"", false, false},
		{"not a number", false, false},
	}
	for _, c := range cases {
		if got := cfg.inboundAllowed(c.number); got != c.inbound {
			t.Errorf("inboundAllowed(%q) = %v, want %v", c.number, got, c.inbound)
		}
		if got := cfg.outboundAllowed(c.number); got != c.outbound {
			t.Errorf("outboundAllowed(%q) = %v, want %v", c.number, got, c.outbound)
		}
	}
}

func TestWildcardOpensInboundOnly(t *testing.T) {
	cfg := prepared(t, func(c *Config) { c.AllowFrom = []string{anyCaller} })
	if err := cfg.validate(); err != nil {
		t.Fatalf("the wildcard was rejected: %v", err)
	}
	if !cfg.allowAnyCaller() {
		t.Error("allowAnyCaller() = false with the wildcard configured")
	}
	if !cfg.inboundAllowed("+15559999999") {
		t.Error("a stranger was refused despite the wildcard")
	}
	if cfg.outboundAllowed("+15559999999") {
		t.Error("the inbound wildcard leaked into the outbound allowlist")
	}
	if got := cfg.inboundAllowlist(); len(got) != 1 || got[0] != anyCaller {
		t.Errorf("inboundAllowlist() = %v, want just the wildcard", got)
	}
}

func TestAllowlistsPutTheOwnerFirstWithoutDuplicating(t *testing.T) {
	cfg := prepared(t, func(c *Config) {
		c.AllowFrom = []string{"+15550001111", "+15550003333"}
		c.AllowCallTo = []string{"+15550001111", "+15550004444"}
	})
	inbound := cfg.inboundAllowlist()
	if len(inbound) != 2 || inbound[0] != "+15550001111" || inbound[1] != "+15550003333" {
		t.Errorf("inboundAllowlist() = %v", inbound)
	}
	outbound := cfg.outboundAllowlist()
	if len(outbound) != 2 || outbound[0] != "+15550001111" || outbound[1] != "+15550004444" {
		t.Errorf("outboundAllowlist() = %v", outbound)
	}
}

func TestTriStateSwitchesDefaultOn(t *testing.T) {
	cfg := validConfig()
	if !cfg.autoInstall() || !cfg.localAudioFallback() {
		t.Error("unset tri-state switches should default to on")
	}
	off := false
	cfg.AutoInstall, cfg.LocalAudioFallback = &off, &off
	if cfg.autoInstall() || cfg.localAudioFallback() {
		t.Error("an explicit false was ignored")
	}
}

func TestNormalizeNumberKeepsOnlyALeadingPlusAndDigits(t *testing.T) {
	for input, want := range map[string]string{
		"+1 (555) 000-1111": "+15550001111",
		"+15550001111":      "+15550001111",
		"555-0000":          "5550000",
		"++1555":            "+1555",
		"1+555":             "1555",
		"":                  "",
		"abc":               "",
	} {
		if got := normalizeNumber(input); got != want {
			t.Errorf("normalizeNumber(%q) = %q, want %q", input, got, want)
		}
	}
}
