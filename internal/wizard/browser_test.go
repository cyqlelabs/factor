//go:build !nobrowser

package wizard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/browser"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/memory"
)

// The browser steps only exist in a build that carries the suite; a
// -tags nobrowser wizard skips them entirely, so these live behind the tag
// rather than passing vacuously there.

// The browser step is the one that used to lie: it asked "enable the browser
// tools?", took yes for an answer, and left the machine with no browser.
func TestWizardInstallsBrowserWhenMissing(t *testing.T) {
	h := newHarness(t, "5", "llama3", "3", "3", "n", "n", "", "y", "y", "n")
	if err := os.Remove(fakeBrowserPath(h.home)); err != nil {
		t.Fatal(err)
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	cfg := h.saved()
	want := filepath.Join(h.home, "engine", "helium", "helium")
	if !cfg.Browser.Enabled || cfg.Browser.Command != want {
		t.Errorf("browser = %+v, want it enabled and pointed at %s", cfg.Browser, want)
	}
	out := h.out.String()
	if !strings.Contains(out, "no browser found") || !strings.Contains(out, "installing Helium") {
		t.Errorf("browser step output:\n%s", out)
	}
}

// A failed install must say why and leave the tools on: setup carrying on
// silently is how a box ends up with the browser tools registered, no browser
// behind them, and nothing left that would ever install one.
func TestWizardReportsAFailedBrowserInstall(t *testing.T) {
	h := newHarness(t, "5", "llama3", "3", "3", "n", "n", "", "y", "y", "n")
	if err := os.Remove(fakeBrowserPath(h.home)); err != nil {
		t.Fatal(err)
	}
	h.opts.EnsureBrowser = func(context.Context, browser.Progress) (string, bool, error) {
		return "", false, errors.New("no Helium build for this architecture")
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	cfg := h.saved()
	if !cfg.Browser.Enabled || cfg.Browser.Command != "" {
		t.Errorf("browser = %+v, want it left enabled with no command", cfg.Browser)
	}
	out := h.out.String()
	if !strings.Contains(out, "no Helium build for this architecture") {
		t.Errorf("the failure reason is missing from setup's output:\n%s", out)
	}
	if !strings.Contains(out, "try this again on their first call") {
		t.Errorf("setup does not say the tools retry:\n%s", out)
	}
}

func TestWizardKeepsBrowserToolsWhenInstallDeclined(t *testing.T) {
	h := newHarness(t, "5", "llama3", "3", "3", "n", "n", "", "y", "n", "n")
	if err := os.Remove(fakeBrowserPath(h.home)); err != nil {
		t.Fatal(err)
	}
	h.opts.EnsureBrowser = func(context.Context, browser.Progress) (string, bool, error) {
		t.Error("EnsureBrowser called after the user declined")
		return "", false, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	cfg := h.saved()
	if !cfg.Browser.Enabled || cfg.Browser.Command != "" {
		t.Errorf("browser = %+v, want it left enabled with no command", cfg.Browser)
	}
}

func TestWizardUsesTheBrowserAlreadyInstalled(t *testing.T) {
	h := newHarness(t, "5", "llama3", "3", "3", "n", "n", "", "y", "n")
	h.opts.EnsureBrowser = func(context.Context, browser.Progress) (string, bool, error) {
		t.Error("EnsureBrowser called with a browser already on PATH")
		return "", false, nil
	}
	var verified config.BrowserConfig
	h.opts.VerifyBrowser = func(_ context.Context, cfg config.BrowserConfig) error {
		verified = cfg
		return nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	want := fakeBrowserPath(h.home)
	if h.saved().Browser.Command != want {
		t.Errorf("command = %q, want %q", h.saved().Browser.Command, want)
	}
	if verified.Command != want {
		t.Errorf("verified %+v, want the browser that was just configured", verified)
	}
}

// Running as root without --no-sandbox is one of the two ways a correctly
// installed browser still refuses to start; the other, no display, is decided
// at launch instead of here.
func TestWizardConfiguresBrowserForRoot(t *testing.T) {
	h := newHarness(t, "5", "llama3", "3", "3", "n", "n", "", "y", "n")
	old := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = old })

	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if !h.saved().Browser.NoSandbox {
		t.Error("no_sandbox stayed off for root — the browser would refuse to start")
	}
	if !strings.Contains(h.out.String(), "runs headless unless") {
		t.Errorf("the display caveat was not mentioned:\n%s", h.out.String())
	}
}

func TestWizardReportsABrowserThatWillNotDrive(t *testing.T) {
	h := newHarness(t, "5", "llama3", "3", "3", "n", "n", "", "y", "n")
	h.opts.VerifyBrowser = func(context.Context, config.BrowserConfig) error {
		return errors.New("chrome exited before the socket appeared")
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if !strings.Contains(h.out.String(), "did not finish a page") {
		t.Errorf("a browser that fails its test page was reported as fine:\n%s", h.out.String())
	}
}

func TestQuietRunProvisionsBrowser(t *testing.T) {
	home := tempHome(t)
	var out bytes.Buffer
	err := Run(context.Background(), filepath.Join(home, "config.json"), Options{
		UI:             NewPlain(strings.NewReader(""), &out),
		NonInteractive: true,
		Home:           home,
		EnsureSmrti: func(context.Context, config.MemoryConfig, memory.Progress) (string, bool, error) {
			return "/usr/bin/smrti", true, nil
		},
		MemoryAnswering: func(context.Context, config.MemoryConfig) bool { return false },
		EnsureBrowser: func(_ context.Context, progress browser.Progress) (string, bool, error) {
			progress("downloading Helium 9.9.9 (125 MB)")
			return "/opt/helium/helium", true, nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	cfg, err := config.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Browser.Command != "/opt/helium/helium" {
		t.Errorf("command = %q, want the provisioned browser", cfg.Browser.Command)
	}
	if !strings.Contains(out.String(), "downloading Helium") {
		t.Errorf("install progress was swallowed:\n%s", out.String())
	}
}

func TestWizardAddsTheLightweightEngineWhenAskedTo(t *testing.T) {
	h := newHarness(t, "5", "llama3", "3", "3", "n", "n", "", "y", "y")
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	cfg := h.saved()
	want := filepath.Join(h.home, "engine", "lightpanda")
	if !cfg.Browser.FastPath || cfg.Browser.FastCommand != want {
		t.Errorf("browser = %+v, want the fast path on and pointed at %s", cfg.Browser, want)
	}
}

// The second engine is a convenience; failing to install it must not cost the
// user the browser they already have.
func TestWizardKeepsTheBrowserWhenTheLightweightEngineWillNotInstall(t *testing.T) {
	h := newHarness(t, "5", "llama3", "3", "3", "n", "n", "", "y", "y")
	h.opts.EnsureFastBrowser = func(context.Context, browser.Progress) (string, bool, error) {
		return "", false, errors.New("Lightpanda will not run here: GLIBC_2.34 not found")
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	cfg := h.saved()
	if cfg.Browser.FastPath || cfg.Browser.FastCommand != "" {
		t.Errorf("browser = %+v, want the fast path left off", cfg.Browser)
	}
	if !cfg.Browser.Enabled || cfg.Browser.Command == "" {
		t.Errorf("browser = %+v, want the real browser untouched", cfg.Browser)
	}
	if !strings.Contains(h.out.String(), "GLIBC_2.34") {
		t.Errorf("the reason was not shown:\n%s", h.out.String())
	}
}

// Offering an engine the machine cannot load costs a 150MB download to say
// no, on exactly the machines least able to spare it.
func TestWizardDoesNotOfferAnEngineThisMachineCannotRun(t *testing.T) {
	h := newHarness(t, "5", "llama3", "3", "3", "n", "n", "", "y")
	h.opts.FastBrowserSupported = func() (bool, string) {
		return false, "the lightweight engine needs glibc 2.34 and this system has 2.31"
	}
	h.opts.EnsureFastBrowser = func(context.Context, browser.Progress) (string, bool, error) {
		t.Error("downloaded an engine this machine cannot run")
		return "", false, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if h.saved().Browser.FastPath {
		t.Error("fast path left on for an engine that cannot load")
	}
	if !strings.Contains(h.out.String(), "glibc 2.34") {
		t.Errorf("the reason was not given:\n%s", h.out.String())
	}
}

// A configured browser is a browser: the scriptable path must not pull down
// a second one over it.
func TestQuietRunKeepsTheConfiguredBrowser(t *testing.T) {
	home := tempHome(t)
	existing := fakeBrowserOnPath(t, home)
	path := filepath.Join(home, "config.json")
	cfg := fmt.Sprintf(`{"memory":{"mode":"off"},"browser":{"enabled":true,"command":%q}}`, existing)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Run(context.Background(), path, Options{
		UI:             NewPlain(strings.NewReader(""), &out),
		NonInteractive: true,
		Home:           home,
		EnsureBrowser: func(context.Context, browser.Progress) (string, bool, error) {
			t.Error("downloaded a browser over the configured one")
			return "", false, nil
		},
		MemoryAnswering: func(context.Context, config.MemoryConfig) bool { return false },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	saved, err := config.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Browser.Command != existing {
		t.Errorf("command = %q, want the configured %q", saved.Browser.Command, existing)
	}
}
