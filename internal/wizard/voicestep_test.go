package wizard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// The key entered below is not fakeTelephony's, so the live check fails;
	// the wizard must keep going and keep the key.
	_, elevenlabs, _ := fakeTelephony(t)
	h.opts.ElevenLabs = elevenlabs.URL
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
	_, elevenlabs, _ := fakeTelephony(t)
	h.opts.ElevenLabs = elevenlabs.URL
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

// constSamples streams s16le frames of one amplitude forever.
type constSamples struct{ amplitude int16 }

func (r constSamples) Read(p []byte) (int, error) {
	for i := 0; i+1 < len(p); i += 2 {
		p[i], p[i+1] = byte(r.amplitude), byte(r.amplitude>>8)
	}
	return len(p), nil
}

// The setup that would have caught a silent default source: the wizard lists
// the real inputs, checks the chosen one live, and loops until a microphone
// actually carries signal.
func TestWizardVoiceMicCheckCatchesASilentSource(t *testing.T) {
	provider := fakeProvider(t, "big-model")
	h := newHarness(t, voiceAnswers(provider,
		"y",         // set up PC voice
		"2",         // microphone: the mixer input (menu: default, mixer, brio)
		"y",         // it was silent — pick a different source
		"3",         // microphone: the brio
		"",          // language
		"1",         // tier: cloud
		"dg-secret", // deepgram key
		"el-secret", // elevenlabs key
		"",          // voice id
		"3",         // activation: push-to-talk
	)...)
	_, elevenlabs, _ := fakeTelephony(t)
	h.opts.ElevenLabs = elevenlabs.URL

	restore := micCheckDuration
	micCheckDuration = 50 * time.Millisecond
	t.Cleanup(func() { micCheckDuration = restore })

	env := hearingAudio("parec", "paplay", "pactl")
	env.Run = func(_ context.Context, argv ...string) (string, error) {
		return "1\talsa_input.usb-mixer.multichannel-input\tPipeWire\n" +
			"2\talsa_input.usb-brio.mono-fallback\tPipeWire\n" +
			"3\talsa_output.hdmi.monitor\tPipeWire\n", nil
	}
	env.Capture = func(_ context.Context, argv []string) (io.ReadCloser, error) {
		// Only the brio carries signal; the mixer and the default are dead.
		if strings.Contains(strings.Join(argv, " "), "brio") {
			return io.NopCloser(constSamples{amplitude: 700}), nil
		}
		return io.NopCloser(constSamples{}), nil
	}
	h.opts.Audio = env

	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	section := savedVoice(t, h)
	if section.InputDevice != "alsa_input.usb-brio.mono-fallback" {
		t.Errorf("input_device = %q, want the live microphone", section.InputDevice)
	}
	out := h.out.String()
	if !strings.Contains(out, "no signal") {
		t.Errorf("the silent source was never called out:\n%s", out)
	}
	if !strings.Contains(out, "microphone is live") {
		t.Errorf("the live check never reported success:\n%s", out)
	}
	if strings.Contains(out, "hdmi.monitor") {
		t.Errorf("a monitor source reached the menu:\n%s", out)
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

// Saying no to a channel's setup must be able to mean no: an existing section
// left in the config is an enabled channel, which is how a user who declined
// Telegram in the wizard still had a bot polling from the gateway.
func TestWizardDecliningAConfiguredChannelCanDisableIt(t *testing.T) {
	provider := fakeProvider(t, "big-model")
	h := newHarness(t,
		"8", provider.URL+"/v1", "sk-test", "1", "5", // provider, reasoning off
		"3", // memory: off
		"n", // set up telegram? no —
		"n", // — and do not keep it enabled
		"n", // no phone
		"y", // restrict to workspace
		"n", // browser off
	)
	seed := func() {
		cfg, err := config.ReadFile(h.path)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Channels = map[string]json.RawMessage{
			"telegram": json.RawMessage(`{"token":"123:secret","allow_from":["1"],"api_base":"http://x"}`),
		}
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
	}
	seed()
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}

	var section map[string]any
	if err := json.Unmarshal(h.saved().Channels["telegram"], &section); err != nil {
		t.Fatal(err)
	}
	if enabled, ok := section["enabled"].(bool); !ok || enabled {
		t.Errorf("declining did not disable the channel: %v", section)
	}
	// Disabling must not cost the section its settings — token, allowlist,
	// and fields the wizard's own mirror does not even know about.
	if section["token"] != "123:secret" || section["api_base"] != "http://x" {
		t.Errorf("disabling lost settings: %v", section)
	}

	// The other answer: declining setup but keeping the channel on.
	kept := newHarness(t,
		"8", provider.URL+"/v1", "sk-test", "1", "5",
		"3",
		"n", // set up telegram? no —
		"y", // — but keep it running
		"n", // no phone
		"y", "n",
	)
	kept.path = h.path
	seed()
	if err := kept.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, kept.out.String())
	}
	var keptSection map[string]any
	if err := json.Unmarshal(kept.saved().Channels["telegram"], &keptSection); err != nil {
		t.Fatal(err)
	}
	if _, present := keptSection["enabled"]; present {
		t.Errorf("keeping the channel wrote a needless flag: %v", keptSection)
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
