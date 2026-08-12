package wizard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func newHarness(t *testing.T, answers ...string) *harness {
	t.Helper()
	home := tempHome(t)
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
	h := newHarness(t, "", "", "", "", "", "", "", "", "", "", "", "", "", "")
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
		"3",    // continue anyway
		"3",    // memory off
		"n",    // no telegram
		"", "", // defaults for the tool questions
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
		"", "",
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
		"", "n", "", "",
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
		"", "",
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
	h := newHarness(t, "5", "llama3", "3", "3", "n", "", "")
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
	h2 := newHarness(t, "8", provider.URL+"/v1", "sk-test", "1", "5", "3", "n", "", "")
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
		"", "",
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
	h := newHarness(t, "5", "llama3", "3", "3", "n", "y", "y", "y", "n")
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
	h := newHarness(t, "5", "llama3", "3", "3", "n", "y", "y")
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
