package phone

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The voice shell only ever sees what this renders, so every tier is checked
// here rather than on a live call.
func TestRenderShellConfigCoversEveryTier(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Config)
		wantTier int
		want     func(*testing.T, shellConfig)
	}{
		{
			name:     "tier 1 · cloud speech both ways",
			wantTier: 1,
			want: func(t *testing.T, s shellConfig) {
				if s.STT.Provider != providerDeepgram || s.STT.Model != deepgramModel {
					t.Errorf("stt = %+v, want deepgram/%s", s.STT, deepgramModel)
				}
				if s.STT.APIKey != "deepgram-secret" {
					t.Errorf("stt api key = %q", s.STT.APIKey)
				}
				if s.TTS.Provider != providerElevenLabs || s.TTS.Model != elevenLabsModel {
					t.Errorf("tts = %+v, want elevenlabs/%s", s.TTS, elevenLabsModel)
				}
				if s.TTS.Format != telephonyFormat {
					t.Errorf("tts format = %q, want the telephony codec %q", s.TTS.Format, telephonyFormat)
				}
				if s.STT.BaseURL != "" || s.TTS.BaseURL != "" {
					t.Error("a cloud tier must not carry local base URLs")
				}
			},
		},
		{
			name: "tier 2 · local speech-to-text",
			mutate: func(c *Config) {
				c.STT = AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: "http://127.0.0.1:8000/v1", Model: "Systran/faster-whisper-small"}
			},
			wantTier: 2,
			want: func(t *testing.T, s shellConfig) {
				if s.STT.BaseURL != "http://127.0.0.1:8000/v1" || s.STT.Model != "Systran/faster-whisper-small" {
					t.Errorf("local stt = %+v", s.STT)
				}
				if s.TTS.Provider != providerElevenLabs {
					t.Errorf("tts should still be cloud, got %q", s.TTS.Provider)
				}
				// The Deepgram key is kept for the cloud fallback; the local
				// server must not be handed it.
				if s.STT.APIKey != "" {
					t.Errorf("the cloud transcription key was sent to a local server: %q", s.STT.APIKey)
				}
			},
		},
		{
			name: "tier 3 · local text-to-speech",
			mutate: func(c *Config) {
				c.TTS = AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: "http://127.0.0.1:8000/v1", Voice: "es_ES-sharvard-medium"}
			},
			wantTier: 3,
			want: func(t *testing.T, s shellConfig) {
				if s.TTS.BaseURL != "http://127.0.0.1:8000/v1" || s.TTS.Voice != "es_ES-sharvard-medium" {
					t.Errorf("local tts = %+v", s.TTS)
				}
				if s.TTS.Format != localAudioFmt {
					t.Errorf("local tts format = %q, want %q", s.TTS.Format, localAudioFmt)
				}
				if s.TTS.APIKey != "" {
					t.Error("a local speech server must not be handed the ElevenLabs key")
				}
				if s.STT.Provider != providerDeepgram {
					t.Errorf("stt should still be cloud, got %q", s.STT.Provider)
				}
			},
		},
		{
			name: "tier 4 · fully local audio",
			mutate: func(c *Config) {
				c.STT = AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: "http://127.0.0.1:8000/v1"}
				c.TTS = AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: "http://127.0.0.1:8000/v1"}
				c.STTAPIKey = ""
				c.ElevenLabsAPIKey = ""
			},
			wantTier: 4,
			want: func(t *testing.T, s shellConfig) {
				if s.STT.BaseURL == "" || s.TTS.BaseURL == "" {
					t.Errorf("both stages should be local: %+v %+v", s.STT, s.TTS)
				}
				if s.STT.APIKey != "" || s.TTS.APIKey != "" {
					t.Error("the fully local tier needs no cloud credentials")
				}
			},
		},
		{
			name:     "hosted whisper keeps its key and default model",
			mutate:   func(c *Config) { c.STT = AudioEndpoint{Provider: providerWhisper} },
			wantTier: 1,
			want: func(t *testing.T, s shellConfig) {
				if s.STT.Model != whisperModel || s.STT.APIKey != "deepgram-secret" {
					t.Errorf("whisper stt = %+v", s.STT)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := prepared(t, c.mutate)
			if err := cfg.validate(); err != nil {
				t.Fatalf("fixture does not validate: %v", err)
			}
			shell := renderShellConfig(cfg, 9999, "bridge-token")
			if shell.Tier != c.wantTier {
				t.Errorf("tier = %d, want %d", shell.Tier, c.wantTier)
			}
			c.want(t, shell)

			// Whatever the tier, these have to be right or nothing works.
			if shell.BridgeURL != "http://127.0.0.1:9999" {
				t.Errorf("bridge url = %q", shell.BridgeURL)
			}
			if shell.BridgeToken != "bridge-token" {
				t.Errorf("bridge token = %q", shell.BridgeToken)
			}
			if shell.ControlPort != cfg.SidecarPort || shell.WebhookPort != cfg.SidecarPort+1 {
				t.Errorf("ports = control %d / webhook %d", shell.ControlPort, shell.WebhookPort)
			}
			if shell.Dashboard {
				t.Error("Patter's dashboard fails open without a token; it must stay off")
			}
			if _, err := json.Marshal(shell); err != nil {
				t.Errorf("shell config is not serializable: %v", err)
			}
		})
	}
}

// Each carrier gets its own credentials and its own wire format; handing the
// shell the other one's would fail on the first call, not at startup.
func TestRenderShellConfigPerCarrier(t *testing.T) {
	twilio := renderShellConfig(prepared(t, nil), 1, "tok")
	if twilio.Carrier.Name != carrierTwilio || twilio.Carrier.AccountSID != "AC0123456789" ||
		twilio.Carrier.AuthToken != "twilio-secret" {
		t.Errorf("twilio carrier = %+v", twilio.Carrier)
	}
	if twilio.Carrier.APIKey != "" || twilio.Carrier.ConnectionID != "" || twilio.Carrier.PublicKey != "" {
		t.Errorf("twilio carried Telnyx fields: %+v", twilio.Carrier)
	}
	if twilio.TTS.Format != telephonyFormat {
		t.Errorf("twilio tts format = %q, want %q", twilio.TTS.Format, telephonyFormat)
	}

	telnyx := renderShellConfig(prepared(t, telnyxConfig), 1, "tok")
	if telnyx.Carrier.Name != carrierTelnyx || telnyx.Carrier.APIKey != "telnyx-secret" ||
		telnyx.Carrier.ConnectionID != "2851234567890" || telnyx.Carrier.PublicKey != "telnyx-public-key" {
		t.Errorf("telnyx carrier = %+v", telnyx.Carrier)
	}
	if telnyx.Carrier.AccountSID != "" || telnyx.Carrier.AuthToken != "" {
		t.Errorf("telnyx carried Twilio fields: %+v", telnyx.Carrier)
	}
	if telnyx.Carrier.PhoneNumber != "+15550002222" {
		t.Errorf("telnyx phone number = %q", telnyx.Carrier.PhoneNumber)
	}
	// Telnyx negotiates linear PCM; μ-law bytes would arrive as noise.
	if telnyx.TTS.Format != telnyxFormat {
		t.Errorf("telnyx tts format = %q, want %q", telnyx.TTS.Format, telnyxFormat)
	}
}

func TestRenderShellConfigCarriesGuardrails(t *testing.T) {
	cfg := prepared(t, func(c *Config) {
		c.MaxCallMinutes = 7
		c.AllowFrom = []string{"+15550003333"}
		c.VoiceID = "voice-abc"
	})
	shell := renderShellConfig(cfg, 1, "tok")

	if shell.MaxCallSeconds != 7*60 {
		t.Errorf("max call seconds = %d, want %d", shell.MaxCallSeconds, 7*60)
	}
	if !shell.MachineDetection {
		t.Error("machine detection should be on for outbound calls")
	}
	if shell.BargeInThresholdMs != bargeInThresholdMs {
		t.Errorf("barge-in threshold = %d", shell.BargeInThresholdMs)
	}
	if len(shell.AllowFrom) != 2 || shell.AllowFrom[0] != cfg.UserNumber {
		t.Errorf("allow_from = %v, want the owner first", shell.AllowFrom)
	}
	if len(shell.AllowCallTo) != 1 || shell.AllowCallTo[0] != cfg.UserNumber {
		t.Errorf("allow_call_to = %v, want just the owner", shell.AllowCallTo)
	}
	if shell.TTS.Voice != "voice-abc" {
		t.Errorf("voice_id did not reach the shell: %q", shell.TTS.Voice)
	}
	if shell.FirstMessage == "" || shell.LongTurnMessage == "" || shell.VoicemailMessage == "" {
		t.Error("a call with no greeting, no filler, or no voicemail message is a dropped call")
	}
}

func TestSpokenLinesFollowTheLanguage(t *testing.T) {
	english := renderShellConfig(prepared(t, nil), 1, "tok")
	spanish := renderShellConfig(prepared(t, func(c *Config) { c.Language = "es-AR" }), 1, "tok")

	if english.FirstMessage == spanish.FirstMessage {
		t.Error("the greeting did not follow the configured language")
	}
	if !strings.Contains(spanish.LongTurnMessage, "segundo") {
		t.Errorf("spanish filler = %q", spanish.LongTurnMessage)
	}
	if !strings.Contains(spanish.VoicemailMessage, "asistente") {
		t.Errorf("spanish voicemail = %q", spanish.VoicemailMessage)
	}
	if spanish.STT.Language != "es-AR" {
		t.Errorf("stt language = %q", spanish.STT.Language)
	}
}

// ---- local audio tier resolution -------------------------------------------

// stubProbe replaces the reachability check for the duration of a test.
func stubProbe(t *testing.T, unreachable ...string) {
	t.Helper()
	original := probeAudioServer
	t.Cleanup(func() { probeAudioServer = original })
	probeAudioServer = func(_ context.Context, baseURL string) error {
		for _, down := range unreachable {
			if baseURL == down {
				return errors.New("connection refused")
			}
		}
		return nil
	}
}

func TestResolveAudioTierFallsBackToTheCloud(t *testing.T) {
	const local = "http://127.0.0.1:8000/v1"
	stubProbe(t, local)

	cfg := prepared(t, func(c *Config) {
		c.STT = AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: local}
		c.TTS = AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: local}
		c.VoiceID = "voice-abc"
	})
	effective, err := resolveAudioTier(context.Background(), cfg)
	if err != nil {
		t.Fatalf("fallback should not be an error: %v", err)
	}
	if effective.Tier() != 1 {
		t.Errorf("tier after fallback = %d, want the cloud tier", effective.Tier())
	}
	if effective.STT.Provider != providerDeepgram || effective.TTS.Provider != providerElevenLabs {
		t.Errorf("fell back to %q / %q", effective.STT.Provider, effective.TTS.Provider)
	}
	if effective.TTS.Voice != "voice-abc" {
		t.Errorf("the configured voice was lost in the fallback: %q", effective.TTS.Voice)
	}
}

func TestResolveAudioTierKeepsAReachableLocalServer(t *testing.T) {
	const local = "http://127.0.0.1:8000/v1"
	stubProbe(t) // everything answers

	cfg := prepared(t, func(c *Config) {
		c.STT = AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: local}
		c.TTS = AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: local}
	})
	effective, err := resolveAudioTier(context.Background(), cfg)
	if err != nil {
		t.Fatalf("resolveAudioTier: %v", err)
	}
	if effective.Tier() != 4 {
		t.Errorf("tier = %d, want the fully local tier untouched", effective.Tier())
	}
}

func TestResolveAudioTierReportsDownWhenFallbackIsRefused(t *testing.T) {
	const local = "http://127.0.0.1:8000/v1"
	stubProbe(t, local)
	no := false

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name: "local-only was asked for",
			mutate: func(c *Config) {
				c.LocalAudioFallback = &no
				c.STT = AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: local}
			},
			wantErr: "speech-to-text server at " + local + " is unreachable",
		},
		{
			name: "nothing to fall back to for speech-to-text",
			mutate: func(c *Config) {
				c.STT = AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: local}
				c.STTAPIKey = ""
			},
			wantErr: "no stt_api_key",
		},
		{
			name: "nothing to fall back to for text-to-speech",
			mutate: func(c *Config) {
				c.TTS = AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: local}
				c.ElevenLabsAPIKey = ""
			},
			wantErr: "no elevenlabs_api_key",
		},
		{
			name: "local-only text-to-speech was asked for",
			mutate: func(c *Config) {
				c.LocalAudioFallback = &no
				c.TTS = AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: local}
			},
			wantErr: "text-to-speech server at " + local + " is unreachable",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := resolveAudioTier(context.Background(), prepared(t, c.mutate))
			if err == nil {
				t.Fatalf("expected the channel to report itself down (wanted %q)", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

// The probe has to treat an answering-but-unhappy server as present: an auth
// challenge still means calls would work.
func TestProbeAudioServerAcceptsAnyResponse(t *testing.T) {
	srv := unauthorizedServer(t)
	if err := probeAudioServer(context.Background(), srv.URL); err != nil {
		t.Errorf("probe rejected a live server that answered 401: %v", err)
	}
	if err := probeAudioServer(context.Background(), "http://127.0.0.1:1"); err == nil {
		t.Error("probe accepted a port with nothing on it")
	}
	if err := probeAudioServer(context.Background(), "://nonsense"); err == nil {
		t.Error("probe accepted an unparseable base URL")
	}
}
