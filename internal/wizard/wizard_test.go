package wizard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/browser"
	"github.com/cyqlelabs/factor/internal/channel/phone"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/desktop"
	"github.com/cyqlelabs/factor/internal/memory"
)

// The wizard is driven here exactly as a piped stdin would drive it: numbered
// menu answers and lines of text. That is the same code path a non-TTY user
// gets, so these tests cover the real fallback UI, not a mock of it.

type harness struct {
	t       *testing.T
	out     bytes.Buffer
	home    string
	path    string
	opts    Options
	answers []string
}

// tempHome isolates a test from the real machine: the config, the workspace
// and the smrti lookup all resolve under a throwaway directory (this box may
// well have a real ~/.factor and a real smrti on PATH).
func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Join(home, "bin"))
	return home
}

// fakeBrowserOnPath satisfies the browser probe so the default flow does not
// stop to offer an install; the tests that exercise provisioning leave it out.
func fakeBrowserOnPath(t *testing.T, home string) string {
	t.Helper()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bin, "chromium")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newHarness(t *testing.T, answers ...string) *harness {
	t.Helper()
	home := tempHome(t)
	fakeBrowserOnPath(t, home)
	h := &harness{t: t, home: home, path: filepath.Join(home, "config.json"), answers: answers}
	h.opts = Options{
		Version:  "test",
		Home:     home,
		HTTP:     http.DefaultClient,
		Telegram: "http://127.0.0.1:0", // unreachable unless a test overrides it
		Desktop: desktop.Env{
			GOOS:   "linux",
			Run:    func(context.Context, string, ...string) (string, error) { return "", nil },
			Has:    func(string) bool { return false },
			Getenv: func(string) string { return "" }, // headless by default
		},
		EnsureSmrti: func(context.Context, config.MemoryConfig, memory.Progress) (string, bool, error) {
			return filepath.Join(home, "venv", "bin", "smrti"), true, nil
		},
		InstallPackages: func(_ context.Context, pkgs []string) (string, error) {
			return "installed " + strings.Join(pkgs, " "), nil
		},
		// Never reach GitHub, and never launch a real browser.
		EnsureBrowser: func(context.Context, browser.Progress) (string, bool, error) {
			return filepath.Join(home, "engine", "helium", "helium"), true, nil
		},
		VerifyBrowser: func(context.Context, config.BrowserConfig) error { return nil },
		EnsureFastBrowser: func(context.Context, browser.Progress) (string, bool, error) {
			return filepath.Join(home, "engine", "lightpanda"), true, nil
		},
		FastBrowserSupported: func() (bool, string) { return true, "" },
		// Never touch the network or the machine's Python: tests that care
		// about the install override this to observe what it was asked for.
		InstallSpeech: func(_ context.Context, language string, _, needTTS bool,
			_ phone.Progress) (phone.SpeechChoices, error) {
			choices := phone.SpeechChoices{
				WhisperModel: "base", WhisperDevice: "cpu", WhisperCompute: "int8",
			}
			if needTTS {
				choices.PiperVoice = language + "-test-medium"
			}
			return choices, nil
		},
	}
	return h
}

func (h *harness) run() error {
	h.t.Helper()
	h.opts.UI = NewPlain(strings.NewReader(strings.Join(h.answers, "\n")+"\n"), &h.out)
	// A plain UI is not "interactive", which would divert Run to the quiet
	// path; call the steps the way an interactive session does.
	opts := h.opts
	opts.defaults()
	cfg, err := config.ReadFile(h.path)
	if err != nil {
		return err
	}
	w := &wiz{cfg: cfg, ui: opts.UI, opts: opts}
	for _, step := range []func(context.Context) error{
		w.stepProvider, w.stepMemory, w.stepChannels, w.stepDesktop, w.stepFinish,
	} {
		if err := step(context.Background()); err != nil {
			return err
		}
	}
	return nil
}

func (h *harness) saved() *config.Config {
	h.t.Helper()
	cfg, err := config.ReadFile(h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	return cfg
}

// fakeProvider serves an OpenAI-compatible /models and /chat/completions.
func fakeProvider(t *testing.T, models ...string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var data []map[string]string
		for _, m := range models {
			data = append(data, map[string]string{"id": m})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestWizardHappyPath(t *testing.T) {
	provider := fakeProvider(t, "big-model", "small-model")
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/getMe") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"username": "factor_bot"}})
	}))
	defer telegram.Close()

	h := newHarness(t,
		"8",                // provider: other OpenAI-compatible
		provider.URL+"/v1", // base URL
		"sk-test",          // api key
		"2",                // model: small-model (menu is sorted)
		"1",                // reasoning effort: xhigh
		"",                 // do not hide the reasoning text
		"1",                // memory: managed sidecar
		"y",                // install smrti
		"3",                // personality: curious
		"y",                // set up telegram
		"123:secret",       // bot token
		"12345, 67890",     // allowed senders
		"n",                // no phone
		"y",                // restrict to workspace
		"n",                // browser off
	)
	h.opts.Telegram = telegram.URL
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}

	cfg := h.saved()
	if cfg.Provider.Type != "custom" || cfg.Provider.APIBase != provider.URL+"/v1" {
		t.Errorf("provider = %+v", cfg.Provider)
	}
	if cfg.Provider.APIKey != "sk-test" {
		t.Errorf("api key = %q", cfg.Provider.APIKey)
	}
	if cfg.Provider.Model != "small-model" {
		t.Errorf("model = %q (the menu is sorted: big-model, small-model)", cfg.Provider.Model)
	}
	if cfg.Memory.Mode != "sidecar" || cfg.Memory.Personality != "curious" {
		t.Errorf("memory = %+v", cfg.Memory)
	}
	tg := telegramConfig(cfg)
	if tg.Token != "123:secret" || len(tg.AllowFrom) != 2 || tg.AllowFrom[1] != "67890" {
		t.Errorf("telegram = %+v", tg)
	}
	if !cfg.Tools.RestrictToWorkspace || cfg.Browser.Enabled {
		t.Errorf("tools = %+v browser = %+v", cfg.Tools, cfg.Browser)
	}
	if _, err := os.Stat(filepath.Join(cfg.Agent.Workspace, "AGENT.md")); err != nil {
		t.Errorf("workspace not created: %v", err)
	}

	out := h.out.String()
	for _, want := range []string{"connected to @factor_bot", "Factor is ready", "factor gateway"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sk-test") || strings.Contains(out, "123:secret") {
		t.Error("secrets were echoed back to the terminal")
	}
}

func TestWizardKeepsDefaultsOnBlankAnswers(t *testing.T) {
	// Every answer blank: the user just holds Enter.
	h := newHarness(t, "", "", "", "", "", "", "", "", "", "", "", "", "", "", "")
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	cfg := h.saved()
	def := config.Default()
	if cfg.Provider.Type != "openrouter" {
		t.Errorf("provider type = %q, want the first preset", cfg.Provider.Type)
	}
	if cfg.Provider.Model != "google/gemini-3.1-pro-preview" {
		t.Errorf("model = %q", cfg.Provider.Model)
	}
	if cfg.Provider.Reasoning.Effort != "xhigh" {
		t.Errorf("reasoning = %+v; xhigh is the default", cfg.Provider.Reasoning)
	}
	if cfg.Memory.Mode != def.Memory.Mode || cfg.Memory.Personality != def.Memory.Personality {
		t.Errorf("memory = %+v", cfg.Memory)
	}
	if len(cfg.Channels) != 0 {
		t.Errorf("channels = %+v; blank answers should not configure telegram", cfg.Channels)
	}
	if !cfg.Tools.RestrictToWorkspace {
		t.Error("workspace restriction should stay on by default")
	}
}

func TestWizardProviderRetryAfterBadKey(t *testing.T) {
	provider := fakeProvider(t, "good-model")
	h := newHarness(t,
		"8", provider.URL+"/v1",
		"sk-wrong", // rejected by /models and /chat/completions
		"good-model",
		"1", "", // reasoning: xhigh, reasoning text visible
		"1",       // "Re-enter the API key" after the check fails
		"sk-test", // correct key
		"3",       // memory: off, to keep the rest of the run short
		"n",       // no telegram
		"n",       // no phone
		"y", "n",  // restrict, no browser
	)
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	cfg := h.saved()
	if cfg.Provider.APIKey != "sk-test" {
		t.Errorf("api key = %q; the retry did not take", cfg.Provider.APIKey)
	}
	out := h.out.String()
	if !strings.Contains(out, "That did not work") {
		t.Errorf("the failed check was not surfaced:\n%s", out)
	}
}

func TestWizardContinueAnywayWithBrokenProvider(t *testing.T) {
	h := newHarness(t,
		"8", "http://127.0.0.1:1/v1", // nothing listens there
		"sk-x",
		"some-model", // no model list: free-text entry
		"1", "",      // reasoning: xhigh, visible
		"3",        // continue anyway
		"3",        // memory off
		"n",        // no telegram
		"n",        // no phone
		"", "", "", // defaults for the tool questions
	)
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if cfg := h.saved(); cfg.Provider.Model != "some-model" {
		t.Errorf("model = %q", cfg.Provider.Model)
	}
}

func TestWizardModelFilterForLongLists(t *testing.T) {
	var models []string
	for i := 0; i < 40; i++ {
		models = append(models, fmt.Sprintf("vendor/model-%02d", i))
	}
	models = append(models, "vendor/sonnet-x")
	provider := fakeProvider(t, models...)

	h := newHarness(t,
		"8", provider.URL+"/v1", "sk-test",
		"sonnet", // filter
		"1",      // the single match
		"1", "",  // reasoning: xhigh, visible
		"3", // memory off
		"n", // no telegram
		"n", // no phone
		"", "", "",
	)
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if got := h.saved().Provider.Model; got != "vendor/sonnet-x" {
		t.Errorf("model = %q", got)
	}
	if !strings.Contains(h.out.String(), "41 models available") {
		t.Errorf("the list size was not reported:\n%s", h.out.String())
	}
}

func TestWizardSmrtiInstallFailureIsNotFatal(t *testing.T) {
	h := newHarness(t,
		"5",      // ollama: no key needed
		"llama3", // model (no live list)
		"3",      // continue anyway after the check fails
		"1", "y", // memory sidecar, install smrti
		"", "n", "n", "", "", "",
	)
	h.opts.EnsureSmrti = func(context.Context, config.MemoryConfig, memory.Progress) (string, bool, error) {
		return "", false, fmt.Errorf("no Python installer found")
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	cfg := h.saved()
	if !cfg.Memory.AutoInstall {
		t.Error("auto_install should stay on so Factor retries on first start")
	}
	if !strings.Contains(h.out.String(), "pip install smrti") {
		t.Errorf("no manual instructions after a failed install:\n%s", h.out.String())
	}
}

func TestWizardReasoningOverrides(t *testing.T) {
	provider := fakeProvider(t, "m1")
	h := newHarness(t,
		"8", provider.URL+"/v1", "sk-test",
		"1",     // model m1
		"6",     // custom token budget
		"12000", // the budget
		"y",     // keep the reasoning text out of replies
		"3",     // memory off
		"n",     // no telegram
		"n",     // no phone
		"", "", "",
	)
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	r := h.saved().Provider.Reasoning
	if r.MaxTokens != 12000 || r.Effort != "" || !r.Exclude {
		t.Fatalf("reasoning = %+v", r)
	}
	if !strings.Contains(h.out.String(), "12000 thinking tokens") {
		t.Errorf("summary did not report the budget:\n%s", h.out.String())
	}
}

func TestWizardReasoningOffAndSkippedForLocalProviders(t *testing.T) {
	// Ollama: the wizard must not ask about reasoning at all.
	h := newHarness(t, "5", "llama3", "3", "3", "n", "n", "", "", "")
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if got := h.saved().Provider.Reasoning; !got.IsZero() {
		t.Errorf("reasoning = %+v; local servers get none", got)
	}
	if !strings.Contains(h.out.String(), "not available for local servers") {
		t.Errorf("output:\n%s", h.out.String())
	}

	// A key-based provider can still turn reasoning off explicitly.
	provider := fakeProvider(t, "m1")
	h2 := newHarness(t, "8", provider.URL+"/v1", "sk-test", "1", "5", "3", "n", "n", "", "", "")
	if err := h2.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h2.out.String())
	}
	if got := h2.saved().Provider.Reasoning; got.Effort != "none" {
		t.Errorf("reasoning = %+v", got)
	}
}

func TestWizardExternalMemory(t *testing.T) {
	h := newHarness(t,
		"5", "llama3", "3", // provider: ollama, unchecked
		"2", "http://memory.lan:8420", // external smrti
		"1", // personality
		"n", // no telegram
		"n", // no phone
		"", "", "",
	)
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	cfg := h.saved()
	if cfg.Memory.Mode != "external" || cfg.Memory.URL != "http://memory.lan:8420" {
		t.Errorf("memory = %+v", cfg.Memory)
	}
}

func TestWizardDesktopHelpersOffered(t *testing.T) {
	h := newHarness(t, "5", "llama3", "3", "3", "n", "n", "y", "y", "y", "n")
	var installed []string
	h.opts.Desktop = desktop.Env{
		GOOS:   "linux",
		Run:    func(context.Context, string, ...string) (string, error) { return "", nil },
		Has:    func(bin string) bool { return bin == "xdotool" || bin == "apt-get" },
		Getenv: func(k string) string { return map[string]string{"DISPLAY": ":0"}[k] },
	}
	h.opts.InstallPackages = func(_ context.Context, pkgs []string) (string, error) {
		installed = pkgs
		return "ok", nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	cfg := h.saved()
	if cfg.Desktop.Enabled == nil || !*cfg.Desktop.Enabled {
		t.Errorf("desktop = %+v", cfg.Desktop)
	}
	out := h.out.String()
	if !strings.Contains(out, "desktop backend: x11") || !strings.Contains(out, "missing desktop helpers") {
		t.Errorf("desktop step output:\n%s", out)
	}
	// Only meaningful when a package manager was detected on the test machine.
	if len(installed) > 0 {
		for _, want := range []string{"wmctrl", "scrot"} {
			if !strings.Contains(strings.Join(installed, " "), want) {
				t.Errorf("installed %v, want it to include %s", installed, want)
			}
		}
	}
}

func TestWizardDesktopSkippedWhenHeadless(t *testing.T) {
	h := newHarness(t, "5", "llama3", "3", "3", "n", "n", "y", "y", "n")
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if cfg := h.saved(); cfg.Desktop.Enabled != nil {
		t.Errorf("headless run pinned desktop.enabled = %v; it should stay auto", *cfg.Desktop.Enabled)
	}
	if !strings.Contains(h.out.String(), "no graphical session") {
		t.Errorf("output:\n%s", h.out.String())
	}
}

func TestWizardAbortsOnEOF(t *testing.T) {
	h := newHarness(t) // no answers at all
	err := h.run()
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(h.path); statErr == nil {
		t.Error("an aborted wizard wrote a config file")
	}
}

func TestQuietRunInstallsAndWrites(t *testing.T) {
	home := tempHome(t)
	var out bytes.Buffer
	called := false
	err := Run(context.Background(), filepath.Join(home, "config.json"), Options{
		UI:             NewPlain(strings.NewReader(""), &out),
		NonInteractive: true,
		Home:           home,
		EnsureSmrti: func(context.Context, config.MemoryConfig, memory.Progress) (string, bool, error) {
			called = true
			return "/usr/bin/smrti", true, nil
		},
		MemoryAnswering: func(context.Context, config.MemoryConfig) bool { return false },
		EnsureBrowser: func(context.Context, browser.Progress) (string, bool, error) {
			return "/usr/bin/chromium", false, nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !called {
		t.Error("the quiet path did not try to install smrti")
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); err != nil {
		t.Errorf("no config written: %v", err)
	}
	if !strings.Contains(out.String(), "installed at /usr/bin/smrti") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestQuietRunRespectsNoInstall(t *testing.T) {
	home := tempHome(t)
	var out bytes.Buffer
	err := Run(context.Background(), filepath.Join(home, "config.json"), Options{
		UI:             NewPlain(strings.NewReader(""), &out),
		NonInteractive: true,
		NoInstall:      true,
		Home:           home,
		EnsureSmrti: func(context.Context, config.MemoryConfig, memory.Progress) (string, bool, error) {
			t.Error("EnsureSmrti called despite NoInstall")
			return "", false, nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// The env-overlaid config must never be what gets written: exporting a key
// for one session should not persist it into the file.
func TestWizardDoesNotPersistEnvironmentSecrets(t *testing.T) {
	home := tempHome(t)
	t.Setenv("FACTOR_PROVIDER_API_KEY", "sk-from-env")
	var out bytes.Buffer
	if err := Run(context.Background(), filepath.Join(home, "config.json"), Options{
		UI:             NewPlain(strings.NewReader(""), &out),
		NonInteractive: true,
		NoInstall:      true,
		Home:           home,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sk-from-env") {
		t.Errorf("the environment key was persisted:\n%s", data)
	}
}

// ---- the phone step --------------------------------------------------------

// fakeTelephony serves the credential probes the phone step runs: the carrier
// account, the voice vendor, and a local speech server.
func fakeTelephony(t *testing.T) (twilio, elevenlabs, speech *httptest.Server) {
	t.Helper()
	twilio = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if user != "AC-test" || pass != "twilio-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Authenticate"}`))
			return
		}
		_, _ = w.Write([]byte(`{"friendly_name":"Factor","status":"active"}`))
	}))
	elevenlabs = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("xi-api-key") != "eleven-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"subscription":{"tier":"starter"}}`))
	}))
	speech = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"Systran/faster-whisper-small"}]}`))
	}))
	for _, srv := range []*httptest.Server{twilio, elevenlabs, speech} {
		t.Cleanup(srv.Close)
	}
	return twilio, elevenlabs, speech
}

// savedPhone reads back the phone section the wizard wrote.
func savedPhone(t *testing.T, h *harness) phoneSection {
	t.Helper()
	raw, ok := h.saved().Channels["phone"]
	if !ok {
		t.Fatalf("no phone section was written:\n%s", h.out.String())
	}
	var section phoneSection
	if err := json.Unmarshal(raw, &section); err != nil {
		t.Fatal(err)
	}
	return section
}

func TestWizardPhoneCloudTier(t *testing.T) {
	twilio, elevenlabs, _ := fakeTelephony(t)
	h := newHarness(t,
		"5", "llama3", "3", // provider: ollama, unchecked
		"3",               // memory off
		"n",               // no telegram
		"y",               // set up the phone
		"1",               // carrier: twilio
		"AC-test",         // twilio account sid
		"twilio-secret",   // twilio auth token
		"+1 555 000 2222", // the number bought at the carrier
		"+1 555 000 1111", // the owner's number
		"",                // language: en
		"1",               // cloud speech
		"deepgram-secret", // transcription key
		"eleven-secret",   // voice key
		"voice-abc",       // voice id
		"1",               // proactive: text me
		"", "", "",        // tools
	)
	h.opts.Twilio, h.opts.ElevenLabs = twilio.URL, elevenlabs.URL
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}

	section := savedPhone(t, h)
	if section.UserNumber != "+1 555 000 1111" || section.PhoneNumber != "+1 555 000 2222" {
		t.Errorf("numbers = %+v (the channel normalizes them on load)", section)
	}
	if section.TwilioAccountSID != "AC-test" || section.TwilioAuthToken != "twilio-secret" {
		t.Errorf("carrier credentials = %+v", section)
	}
	if section.STT.Provider != "deepgram" || section.STTAPIKey != "deepgram-secret" {
		t.Errorf("stt = %+v / %q", section.STT, section.STTAPIKey)
	}
	if section.TTS.Provider != "elevenlabs" || section.ElevenLabsAPIKey != "eleven-secret" {
		t.Errorf("tts = %+v / %q", section.TTS, section.ElevenLabsAPIKey)
	}
	if section.VoiceID != "voice-abc" || section.Proactive != "sms" {
		t.Errorf("voice/proactive = %q / %q", section.VoiceID, section.Proactive)
	}

	out := h.out.String()
	for _, want := range []string{"Factor", "starter", "cloud speech"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "twilio-secret") || strings.Contains(out, "eleven-secret") {
		t.Errorf("a secret was echoed:\n%s", out)
	}
}

func TestWizardPhoneFullyLocalTier(t *testing.T) {
	twilio, elevenlabs, speech := fakeTelephony(t)
	h := newHarness(t,
		"5", "llama3", "3",
		"3", // memory off
		"n", // no telegram
		"y", "1", "AC-test", "twilio-secret", "+15550002222", "+15550001111",
		"es",                           // language
		"4",                            // fully local audio
		"2",                            // a speech server the user runs
		speech.URL,                     // local speech server
		"Systran/faster-whisper-small", // transcription model
		"es_ES-sharvard-medium",        // voice
		"2",                            // proactive: call me
		"", "", "",
	)
	h.opts.Twilio, h.opts.ElevenLabs = twilio.URL, elevenlabs.URL
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}

	section := savedPhone(t, h)
	if section.STT.Provider != "local-openai" || section.STT.BaseURL != speech.URL {
		t.Errorf("stt = %+v, want the local server", section.STT)
	}
	if section.TTS.Provider != "local-openai" || section.TTS.Voice != "es_ES-sharvard-medium" {
		t.Errorf("tts = %+v", section.TTS)
	}
	if section.STTAPIKey != "" || section.ElevenLabsAPIKey != "" {
		t.Error("the fully local tier asked for cloud credentials")
	}
	if section.Language != "es" || section.Proactive != "call" {
		t.Errorf("language/proactive = %q / %q", section.Language, section.Proactive)
	}
	if !strings.Contains(h.out.String(), "fully local audio") {
		t.Errorf("the chosen tier is not in the summary:\n%s", h.out.String())
	}
}

// A local speech server that is not running yet must not block setup: the
// channel falls back to the cloud tier until it answers.
func TestWizardPhoneLocalTierSurvivesAnAbsentServer(t *testing.T) {
	twilio, elevenlabs, _ := fakeTelephony(t)
	h := newHarness(t,
		"5", "llama3", "3", "3", "n",
		"y", "1", "AC-test", "twilio-secret", "+15550002222", "+15550001111",
		"",                      // language
		"2",                     // local speech-to-text only
		"2",                     // a speech server the user runs
		"http://127.0.0.1:1/v1", // nothing listens there
		"",                      // model: the server's default
		"eleven-secret",         // voice key (text-to-speech is still cloud)
		"",                      // default voice
		"1",                     // proactive: text me
		"", "", "",
	)
	h.opts.Twilio, h.opts.ElevenLabs = twilio.URL, elevenlabs.URL
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	section := savedPhone(t, h)
	if section.STT.Provider != "local-openai" {
		t.Errorf("stt = %+v; the choice should stand", section.STT)
	}
	if !strings.Contains(h.out.String(), "falls back to the cloud tier") {
		t.Errorf("the user was not told what happens next:\n%s", h.out.String())
	}
}

// Choosing a local tier has to leave a working setup, not a to-do list: the
// wizard installs the engines and the models itself and asks the user nothing
// about servers, ports, or model names.
func TestWizardPhoneLocalTierInstallsEverything(t *testing.T) {
	twilio, elevenlabs, _ := fakeTelephony(t)
	h := newHarness(t,
		"5", "llama3", "3", "3", "n",
		"y", "1", "AC-test", "twilio-secret", "+15550002222", "+15550001111",
		"es-MX", // language
		"4",     // fully local audio
		"1",     // let Factor install it
		"2",     // proactive: call me
		"", "", "",
	)
	h.opts.Twilio, h.opts.ElevenLabs = twilio.URL, elevenlabs.URL

	var gotLanguage string
	var gotSTT, gotTTS bool
	h.opts.InstallSpeech = func(_ context.Context, language string, needSTT, needTTS bool,
		_ phone.Progress) (phone.SpeechChoices, error) {
		gotLanguage, gotSTT, gotTTS = language, needSTT, needTTS
		return phone.SpeechChoices{
			WhisperModel: "small", WhisperDevice: "cuda", WhisperCompute: "float16",
			PiperVoice: "es_MX-ald-medium",
		}, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}

	if gotLanguage != "es-MX" || !gotSTT || !gotTTS {
		t.Errorf("installer called with language=%q stt=%v tts=%v", gotLanguage, gotSTT, gotTTS)
	}

	section := savedPhone(t, h)
	// A blank base_url is what marks the server as Factor's own.
	if section.STT.Provider != "local-openai" || section.STT.BaseURL != "" {
		t.Errorf("stt = %+v, want Factor's own server", section.STT)
	}
	if section.TTS.Provider != "local-openai" || section.TTS.BaseURL != "" {
		t.Errorf("tts = %+v, want Factor's own server", section.TTS)
	}
	if section.SpeechServer == nil {
		t.Fatal("what the installer chose was not written to the config")
	}
	if section.SpeechServer.PiperVoice != "es_MX-ald-medium" {
		t.Errorf("voice = %q, want the Mexican Spanish one", section.SpeechServer.PiperVoice)
	}
	if section.SpeechServer.WhisperDevice != "cuda" || section.SpeechServer.WhisperModel != "small" {
		t.Errorf("hardware choices lost: %+v", section.SpeechServer)
	}
	if section.STTAPIKey != "" || section.ElevenLabsAPIKey != "" {
		t.Error("the fully local tier asked for cloud credentials")
	}
	if !strings.Contains(h.out.String(), "es_MX-ald-medium") {
		t.Errorf("the user was not told what was installed:\n%s", h.out.String())
	}
}

// An install that fails must not strand the user mid-setup: the config it was
// going to write is already right, and the gateway retries on start.
func TestWizardPhoneLocalTierSurvivesAFailedInstall(t *testing.T) {
	twilio, elevenlabs, _ := fakeTelephony(t)
	h := newHarness(t,
		"5", "llama3", "3", "3", "n",
		"y", "1", "AC-test", "twilio-secret", "+15550002222", "+15550001111",
		"en", "4", "1", "1", "", "", "",
	)
	h.opts.Twilio, h.opts.ElevenLabs = twilio.URL, elevenlabs.URL
	h.opts.InstallSpeech = func(context.Context, string, bool, bool, phone.Progress) (phone.SpeechChoices, error) {
		return phone.SpeechChoices{}, errors.New("no space left on device")
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}

	section := savedPhone(t, h)
	if section.STT.Provider != "local-openai" || section.TTS.Provider != "local-openai" {
		t.Errorf("the tier choice should stand: stt=%+v tts=%+v", section.STT, section.TTS)
	}
	if section.SpeechServer != nil {
		t.Errorf("a failed install should record no choices: %+v", section.SpeechServer)
	}
	if !strings.Contains(h.out.String(), "try again when the gateway starts") {
		t.Errorf("the user was not told what happens next:\n%s", h.out.String())
	}
}

// A tier that keeps one half in the cloud must not download the other half's
// models — a local-voice machine has no use for a transcription model.
func TestWizardLocalVoiceOnlyDoesNotInstallTranscription(t *testing.T) {
	twilio, elevenlabs, _ := fakeTelephony(t)
	h := newHarness(t,
		"5", "llama3", "3", "3", "n",
		"y", "1", "AC-test", "twilio-secret", "+15550002222", "+15550001111",
		"en",
		"3",               // local text-to-speech only
		"1",               // let Factor install it
		"deepgram-secret", // transcription is still cloud
		"1", "", "", "",
	)
	h.opts.Twilio, h.opts.ElevenLabs = twilio.URL, elevenlabs.URL

	var gotSTT, gotTTS bool
	h.opts.InstallSpeech = func(_ context.Context, _ string, needSTT, needTTS bool,
		_ phone.Progress) (phone.SpeechChoices, error) {
		gotSTT, gotTTS = needSTT, needTTS
		return phone.SpeechChoices{WhisperModel: "base", PiperVoice: "en_US-lessac-medium"}, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if gotSTT || !gotTTS {
		t.Errorf("installer asked for stt=%v tts=%v, want the voice only", gotSTT, gotTTS)
	}
	section := savedPhone(t, h)
	if section.STT.Provider != "deepgram" || section.STTAPIKey != "deepgram-secret" {
		t.Errorf("stt = %+v / %q, want the cloud half kept", section.STT, section.STTAPIKey)
	}
}

func TestWizardPhoneOnTelnyx(t *testing.T) {
	_, elevenlabs, _ := fakeTelephony(t)
	var probed string
	telnyx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer telnyx-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"detail":"Authentication failed"}]}`))
			return
		}
		probed = r.URL.Path
		_, _ = w.Write([]byte(`{"data":{"application_name":"factor-voice","active":true}}`))
	}))
	t.Cleanup(telnyx.Close)

	h := newHarness(t,
		"5", "llama3", "3", "3", "n",
		"y",                 // set up the phone
		"2",                 // carrier: telnyx
		"telnyx-secret",     // api key
		"285123",            // connection id
		"telnyx-public-key", // public key
		"+15550002222",      // the number bought at the carrier
		"+15550001111",      // the owner's number
		"",                  // language: en
		"1",                 // cloud speech
		"deepgram-secret",   // transcription key
		"eleven-secret",     // voice key
		"",                  // voice id: the default
		"1",                 // proactive: text me
		"", "", "",
	)
	h.opts.Telnyx, h.opts.ElevenLabs = telnyx.URL, elevenlabs.URL
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}

	section := savedPhone(t, h)
	if section.Carrier != carrierTelnyx {
		t.Errorf("carrier = %q, want %q", section.Carrier, carrierTelnyx)
	}
	if section.TelnyxAPIKey != "telnyx-secret" || section.TelnyxConnectionID != "285123" ||
		section.TelnyxPublicKey != "telnyx-public-key" {
		t.Errorf("telnyx credentials = %+v", section)
	}
	if section.TwilioAccountSID != "" || section.TwilioAuthToken != "" {
		t.Errorf("a Telnyx section carries Twilio credentials: %+v", section)
	}
	// The probe has to read the application, which is what proves the
	// connection id belongs to this account.
	if probed != "/v2/call_control_applications/285123" {
		t.Errorf("the wizard probed %q", probed)
	}
	if !strings.Contains(h.out.String(), "factor-voice") {
		t.Errorf("the verified application is not in the output:\n%s", h.out.String())
	}
	if strings.Contains(h.out.String(), "telnyx-secret") {
		t.Errorf("a secret was echoed:\n%s", h.out.String())
	}
}

// The public key is what lets the shell verify the carrier's webhooks; without
// it every call would fail at the carrier, so the section is not worth writing.
func TestWizardPhoneSkippedWithoutTheTelnyxPublicKey(t *testing.T) {
	telnyx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"application_name":"factor-voice","active":true}}`))
	}))
	t.Cleanup(telnyx.Close)

	h := newHarness(t,
		"5", "llama3", "3", "3", "n",
		"y", "2", "telnyx-secret", "285123",
		"", // no public key
		"", "", "",
	)
	h.opts.Telnyx = telnyx.URL
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if _, configured := h.saved().Channels["phone"]; configured {
		t.Error("a phone section was written that could never take a call")
	}
	if !strings.Contains(h.out.String(), "skipping the phone") {
		t.Errorf("output:\n%s", h.out.String())
	}
}

func TestWizardPhoneSkippedWithoutCarrierCredentials(t *testing.T) {
	h := newHarness(t,
		"5", "llama3", "3", "3", "n",
		"y", // set up the phone
		"1", // carrier: twilio
		"",  // no account sid
		"",  // no auth token
		"", "", "",
	)
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if _, configured := h.saved().Channels["phone"]; configured {
		t.Error("a phone section was written with no carrier credentials")
	}
	if !strings.Contains(h.out.String(), "skipping the phone") {
		t.Errorf("output:\n%s", h.out.String())
	}
}

// A smrti running in Docker or on another box is a working memory. Offering
// to install one on top of it is how `factor init` ended up asking a user to
// install something they were already running.
func TestWizardDoesNotOfferSmrtiOverALiveEngine(t *testing.T) {
	h := newHarness(t, "5", "llama3", "3", "1", "3", "n", "n", "", "n")
	h.opts.MemoryAnswering = func(context.Context, config.MemoryConfig) bool { return true }
	h.opts.EnsureSmrti = func(context.Context, config.MemoryConfig, memory.Progress) (string, bool, error) {
		t.Error("offered to install smrti while one was already answering")
		return "", false, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if !strings.Contains(h.out.String(), "already answering") {
		t.Errorf("the live engine was not reported:\n%s", h.out.String())
	}
}

func TestQuietRunLeavesAnExternalEngineAlone(t *testing.T) {
	home := tempHome(t)
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, []byte(`{"memory":{"mode":"external","url":"http://memory.lan:8420"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Run(context.Background(), path, Options{
		UI:             NewPlain(strings.NewReader(""), &out),
		NonInteractive: true,
		Home:           home,
		EnsureSmrti: func(context.Context, config.MemoryConfig, memory.Progress) (string, bool, error) {
			t.Error("installed smrti for an engine Factor does not run")
			return "", false, nil
		},
		MemoryAnswering: func(context.Context, config.MemoryConfig) bool { return false },
		EnsureBrowser: func(context.Context, browser.Progress) (string, bool, error) {
			return "/usr/bin/chromium", false, nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// Setting Factor up over ssh must still provision the desktop the box runs:
// deciding from this session's environment is how xdotool never got installed
// on a machine with a screen in front of it.
func TestWizardSetsUpTheDesktopWhenOnlyTheSessionIsHeadless(t *testing.T) {
	h := newHarness(t, "5", "llama3", "3", "3", "n", "n", "y", "y", "y", "n")
	h.opts.Desktop = desktop.Env{
		GOOS:   "linux",
		Run:    func(context.Context, string, ...string) (string, error) { return "", nil },
		Has:    func(string) bool { return false },
		Getenv: func(string) string { return "" }, // ssh: no DISPLAY here
		Glob: func(pattern string) ([]string, error) {
			if pattern == "/tmp/.X11-unix/X*" {
				return []string{"/tmp/.X11-unix/X0"}, nil
			}
			return nil, nil
		},
	}
	var installed []string
	h.opts.InstallPackages = func(_ context.Context, pkgs []string) (string, error) {
		installed = pkgs
		return "ok", nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	out := h.out.String()
	if strings.Contains(out, "no graphical session detected") {
		t.Errorf("the running X server was ignored:\n%s", out)
	}
	if !strings.Contains(out, "missing desktop helpers") {
		t.Errorf("helpers were never offered:\n%s", out)
	}
	if cfg := h.saved(); cfg.Desktop.Enabled == nil || !*cfg.Desktop.Enabled {
		t.Error("desktop control was left off on a machine with a display")
	}
	if len(installed) > 0 && !strings.Contains(strings.Join(installed, " "), "xdotool") {
		t.Errorf("installed %v, want the missing helpers", installed)
	}
}

func TestQuietRunInstallsDesktopHelpers(t *testing.T) {
	home := tempHome(t)
	var out bytes.Buffer
	err := Run(context.Background(), filepath.Join(home, "config.json"), Options{
		UI:             NewPlain(strings.NewReader(""), &out),
		NonInteractive: true,
		Home:           home,
		Desktop: desktop.Env{
			GOOS:   "linux",
			Run:    func(context.Context, string, ...string) (string, error) { return "", nil },
			Has:    func(string) bool { return false },
			Getenv: func(string) string { return "" },
			Glob: func(pattern string) ([]string, error) {
				if pattern == "/tmp/.X11-unix/X*" {
					return []string{"/tmp/.X11-unix/X0"}, nil
				}
				return nil, nil
			},
		},
		EnsureSmrti: func(context.Context, config.MemoryConfig, memory.Progress) (string, bool, error) {
			return "/usr/bin/smrti", false, nil
		},
		MemoryAnswering: func(context.Context, config.MemoryConfig) bool { return false },
		EnsureBrowser: func(context.Context, browser.Progress) (string, bool, error) {
			return "/usr/bin/chromium", false, nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Either it installed them or it said why it could not; silently
	// skipping the machine's dependencies is the failure being guarded here.
	if !strings.Contains(out.String(), "desktop:") {
		t.Errorf("the scriptable path ignored the desktop helpers:\n%s", out.String())
	}
}
