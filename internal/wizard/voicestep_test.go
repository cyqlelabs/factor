package wizard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/channel/phone"
	"github.com/cyqlelabs/factor/internal/channel/voice"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/desktop"
	"github.com/cyqlelabs/factor/internal/memory"
)

// hearingAudio is a machine with a sound card and the named helper binaries.
func hearingAudio(bins ...string) voice.Env {
	installed := map[string]bool{}
	for _, b := range bins {
		installed[b] = true
	}
	return voice.Env{
		GOOS:   "linux",
		Has:    func(bin string) bool { return installed[bin] },
		Getenv: func(string) string { return "" },
		Glob: func(pattern string) ([]string, error) {
			if pattern == "/dev/snd/pcm*" {
				return []string{"/dev/snd/pcmC0D0p"}, nil
			}
			return nil, nil
		},
	}
}

// plantManager puts a fake apt-get on the truncated PATH, so the helper
// install path can be exercised without a real package manager.
func plantManager(t *testing.T, home string) {
	t.Helper()
	path := filepath.Join(home, "bin", "apt-get")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func savedVoice(t *testing.T, h *harness) voiceSection {
	t.Helper()
	cfg := h.saved()
	raw, ok := cfg.Channels["voice"]
	if !ok {
		t.Fatalf("no channels.voice was saved; output:\n%s", h.out.String())
	}
	var section voiceSection
	if err := json.Unmarshal(raw, &section); err != nil {
		t.Fatal(err)
	}
	return section
}

// voiceAnswers reaches the voice step with everything before it declined,
// then appends the step's own answers.
func voiceAnswers(provider *httptest.Server, rest ...string) []string {
	return append([]string{
		"8", // provider: other OpenAI-compatible
		provider.URL + "/v1",
		"sk-test", // api key
		"1",       // model
		"5",       // reasoning: none
		"3",       // memory: off
		"n",       // no telegram
		"n",       // no phone
	}, append(rest,
		"y", // restrict to workspace
		"n", // browser off
	)...)
}

func TestWizardVoiceCloudTier(t *testing.T) {
	provider := fakeProvider(t, "big-model")
	h := newHarness(t, voiceAnswers(provider,
		"y",         // set up PC voice
		"",          // language: en
		"1",         // tier: cloud
		"dg-secret", // deepgram key
		"el-secret", // elevenlabs key
		"my-voice",  // voice id
		"2",         // activation: wake word
		"helios",    // the wake word
	)...)
	h.opts.Audio = hearingAudio("parec", "paplay")
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}

	section := savedVoice(t, h)
	if section.STT.Provider != "deepgram" || section.STTAPIKey != "dg-secret" {
		t.Errorf("stt = %+v key = %q", section.STT, section.STTAPIKey)
	}
	if section.TTS.Provider != "elevenlabs" || section.ElevenLabsAPIKey != "el-secret" || section.VoiceID != "my-voice" {
		t.Errorf("tts = %+v key = %q voice = %q", section.TTS, section.ElevenLabsAPIKey, section.VoiceID)
	}
	if section.Activation != "wake-word" || section.WakeWord != "helios" || section.Language != "en" {
		t.Errorf("activation = %q wake = %q language = %q", section.Activation, section.WakeWord, section.Language)
	}

	out := h.out.String()
	if strings.Contains(out, "dg-secret") || strings.Contains(out, "el-secret") {
		t.Error("secrets were echoed back to the terminal")
	}
	if !strings.Contains(out, "voice · wake-word") {
		t.Errorf("the summary does not name the voice channel:\n%s", out)
	}
	if !strings.Contains(out, "factor talk") {
		t.Errorf("the wake-word rescue was never mentioned:\n%s", out)
	}
}

func TestWizardVoiceLocalTierInstallsSpeech(t *testing.T) {
	provider := fakeProvider(t, "big-model")
	h := newHarness(t, voiceAnswers(provider,
		"y",  // set up PC voice
		"es", // language
		"4",  // tier: fully local
		"1",  // Factor installs the server
		"1",  // activation: always listening
	)...)
	h.opts.Audio = hearingAudio("parec", "paplay")

	var gotLanguage string
	var gotSTT, gotTTS bool
	base := h.opts.InstallSpeech
	h.opts.InstallSpeech = func(ctx context.Context, language string, needSTT, needTTS bool,
		progress phone.Progress) (phone.SpeechChoices, error) {
		gotLanguage, gotSTT, gotTTS = language, needSTT, needTTS
		return base(ctx, language, needSTT, needTTS, progress)
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}

	if gotLanguage != "es" || !gotSTT || !gotTTS {
		t.Errorf("InstallSpeech got language=%q stt=%v tts=%v", gotLanguage, gotSTT, gotTTS)
	}
	section := savedVoice(t, h)
	if section.STT.Provider != "local-openai" || section.STT.BaseURL != "" {
		t.Errorf("stt = %+v, want Factor's own server (blank base_url)", section.STT)
	}
	if section.TTS.Provider != "local-openai" || section.TTS.BaseURL != "" {
		t.Errorf("tts = %+v", section.TTS)
	}
	if section.SpeechServer == nil || section.SpeechServer.PiperVoice != "es-test-medium" {
		t.Errorf("speech_server = %+v, want the installer's choices recorded", section.SpeechServer)
	}
	if section.Activation != "always" {
		t.Errorf("activation = %q", section.Activation)
	}
}

func TestWizardVoiceInstallsMissingAudioHelpers(t *testing.T) {
	provider := fakeProvider(t, "big-model")
	h := newHarness(t, voiceAnswers(provider,
		"y",         // set up PC voice
		"y",         // install the audio helpers
		"",          // language
		"1",         // tier: cloud
		"dg-secret", // deepgram key
		"el-secret", // elevenlabs key
		"",          // voice id
		"3",         // activation: push-to-talk
	)...)
	h.opts.Audio = hearingAudio() // a sound card, but nothing to drive it
	plantManager(t, h.home)

	var installed []string
	h.opts.InstallPackages = func(_ context.Context, pkgs []string) (string, error) {
		installed = append(installed, pkgs...)
		return "ok", nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}

	if len(installed) != 1 || installed[0] != "pulseaudio-utils" {
		t.Errorf("installed = %v, want the one package carrying both helpers", installed)
	}
	if section := savedVoice(t, h); section.Activation != "push-to-talk" {
		t.Errorf("activation = %q", section.Activation)
	}
	if !strings.Contains(h.out.String(), "factor talk") {
		t.Errorf("push-to-talk was configured without naming its trigger:\n%s", h.out.String())
	}
}

// A machine with no sound system is never asked: the step notes why and moves
// on, the same way the desktop step handles a headless box.
func TestWizardVoiceSkippedOnADeafMachine(t *testing.T) {
	provider := fakeProvider(t, "big-model")
	h := newHarness(t, voiceAnswers(provider)...) // no voice answers at all
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if !strings.Contains(h.out.String(), "skipping PC voice") {
		t.Errorf("the skip was silent:\n%s", h.out.String())
	}
	if _, ok := h.saved().Channels["voice"]; ok {
		t.Error("a deaf machine grew a voice section")
	}
}

// The scriptable path meets a configured voice channel's dependencies without
// asking, and leaves an unconfigured machine alone.
func TestQuietRunInstallsAudioHelpers(t *testing.T) {
	home := tempHome(t)
	plantManager(t, home)
	path := filepath.Join(home, "config.json")
	cfg, err := config.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Memory.Mode = "off"
	cfg.Channels = map[string]json.RawMessage{
		"voice": json.RawMessage(`{"stt_api_key":"dg","elevenlabs_api_key":"el"}`),
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	var installed []string
	var out bytes.Buffer
	err = Run(context.Background(), path, Options{
		UI:             NewPlain(strings.NewReader(""), &out),
		NonInteractive: true,
		Home:           home,
		Audio:          hearingAudio(),
		// Headless, so the desktop helpers stay out of the assertion.
		Desktop: desktop.Env{
			GOOS:   "linux",
			Run:    func(context.Context, string, ...string) (string, error) { return "", nil },
			Has:    func(string) bool { return false },
			Getenv: func(string) string { return "" },
		},
		MemoryAnswering: func(context.Context, config.MemoryConfig) bool { return false },
		EnsureSmrti: func(context.Context, config.MemoryConfig, memory.Progress) (string, bool, error) {
			return "/usr/bin/smrti", false, nil
		},
		InstallPackages: func(_ context.Context, pkgs []string) (string, error) {
			installed = append(installed, pkgs...)
			return "ok", nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(installed) != 1 || installed[0] != "pulseaudio-utils" {
		t.Errorf("installed = %v", installed)
	}
	if !strings.Contains(out.String(), "voice:") {
		t.Errorf("the scriptable path said nothing about the voice helpers:\n%s", out.String())
	}
}
