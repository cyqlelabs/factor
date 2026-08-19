package wizard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/browser"
	"github.com/cyqlelabs/factor/internal/config"
)

// The minimal complete flow (ollama provider, defaults everywhere), with the
// autostart answer supplied explicitly by each test. The browser questions
// only exist in builds that carry the suite, so the script length follows
// the build tag — a fixed list would land the autostart answer two prompts
// early under -tags nobrowser.
func autostartAnswers(answer string) []string {
	base := []string{"5", "llama3", "3", "3", "n", "n", ""}
	if browser.Available() {
		base = append(base, "", "")
	}
	return append(base, answer)
}

func TestWizardInstallsAutostartOnYes(t *testing.T) {
	h := newHarness(t, autostartAnswers("y")...)
	var gotConfig string
	installed := false
	h.opts.InstallAutostart = func(_ context.Context, configPath string) (string, error) {
		installed = true
		gotConfig = configPath
		return "systemd user service", nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if !installed {
		t.Error("saying yes did not install the login entry")
	}
	// The harness runs under FACTOR_HOME, which a login entry's environment
	// will not carry — the config path must be pinned in.
	if gotConfig != h.path {
		t.Errorf("entry config path = %q, want %q pinned in", gotConfig, h.path)
	}
	if !strings.Contains(h.out.String(), "start at login (systemd user service)") {
		t.Errorf("output:\n%s", h.out.String())
	}
	if !strings.Contains(h.out.String(), "at login (systemd user service)") {
		t.Errorf("summary does not report autostart:\n%s", h.out.String())
	}
}

func TestWizardAutostartDefaultLeavesTheMachineAlone(t *testing.T) {
	h := newHarness(t, autostartAnswers("")...)
	h.opts.InstallAutostart = func(context.Context, string) (string, error) {
		t.Error("the default answer installed a login entry")
		return "", nil
	}
	h.opts.RemoveAutostart = func(context.Context) error {
		t.Error("the default answer removed a login entry")
		return nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if !strings.Contains(h.out.String(), "autostart") {
		t.Errorf("summary is missing the autostart row:\n%s", h.out.String())
	}
}

func TestWizardRemovesAutostartOnDecline(t *testing.T) {
	h := newHarness(t, autostartAnswers("n")...)
	h.opts.AutostartInstalled = func() (string, bool) { return "XDG autostart entry", true }
	removed := false
	h.opts.RemoveAutostart = func(context.Context) error { removed = true; return nil }
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if !removed {
		t.Error("declining did not remove the existing login entry")
	}
	if !strings.Contains(h.out.String(), "no longer start at login") {
		t.Errorf("output:\n%s", h.out.String())
	}
}

func TestWizardAutostartKeepsExistingEntryOnDefault(t *testing.T) {
	// An installed entry makes yes the default, so Enter changes nothing —
	// and nothing is reinstalled over it.
	h := newHarness(t, autostartAnswers("")...)
	h.opts.AutostartInstalled = func() (string, bool) { return "launchd agent", true }
	h.opts.InstallAutostart = func(context.Context, string) (string, error) {
		t.Error("keeping the entry reinstalled it")
		return "", nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	out := h.out.String()
	if !strings.Contains(out, "already starts at login (launchd agent)") ||
		!strings.Contains(out, "keeps starting at login") {
		t.Errorf("output:\n%s", out)
	}
}

func TestWizardAutostartInstallFailureIsNotFatal(t *testing.T) {
	h := newHarness(t, autostartAnswers("y")...)
	h.opts.InstallAutostart = func(context.Context, string) (string, error) {
		return "", errors.New("no user bus")
	}
	if err := h.run(); err != nil {
		t.Fatalf("a failed autostart install aborted the wizard: %v", err)
	}
	out := h.out.String()
	if !strings.Contains(out, "could not set up autostart") ||
		!strings.Contains(out, "factor gateway -d") {
		t.Errorf("output:\n%s", out)
	}
}

func TestWizardAutostartRemoveFailureKeepsTheSummaryHonest(t *testing.T) {
	h := newHarness(t, autostartAnswers("n")...)
	h.opts.AutostartInstalled = func() (string, bool) { return "systemd user service", true }
	h.opts.RemoveAutostart = func(context.Context) error { return errors.New("bus is down") }
	if err := h.run(); err != nil {
		t.Fatalf("a failed removal aborted the wizard: %v", err)
	}
	out := h.out.String()
	if !strings.Contains(out, "could not remove the autostart entry") {
		t.Errorf("output:\n%s", out)
	}
	// The entry is still in place, and the summary must say so rather than
	// echo what the user asked for.
	if !strings.Contains(out, "at login (systemd user service)") {
		t.Errorf("summary claims the entry is gone:\n%s", out)
	}
}

func TestWizardAutostartSkippedByNoInstall(t *testing.T) {
	// --no-install and nothing installed: no prompt, no machine mutation.
	// The script carries no autostart answer at all.
	h := newHarness(t, "5", "llama3", "3", "3", "n", "n", "", "", "")
	h.opts.NoInstall = true
	h.opts.InstallAutostart = func(context.Context, string) (string, error) {
		t.Error("--no-install installed a login entry")
		return "", nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	if !strings.Contains(h.out.String(), "skipped (--no-install)") {
		t.Errorf("output:\n%s", h.out.String())
	}
}

func TestWizardAutostartPinsNonDefaultConfigPath(t *testing.T) {
	tempHome(t)
	cfg, err := config.ReadFile("/tmp/elsewhere/factor.json")
	if err != nil {
		t.Fatal(err)
	}
	w := &wiz{cfg: cfg}
	if got := w.autostartConfigPath(); got != "/tmp/elsewhere/factor.json" {
		t.Errorf("autostartConfigPath() = %q, want the explicit path", got)
	}
}

func TestWizardAutostartOmitsTheTrulyDefaultConfigPath(t *testing.T) {
	// No FACTOR_HOME and the default location: a login gateway finds this
	// config on its own, so the entry stays free of a pinned path that would
	// only go stale.
	tempHome(t)
	t.Setenv("FACTOR_HOME", "")
	cfg, err := config.ReadFile("")
	if err != nil {
		t.Fatal(err)
	}
	w := &wiz{cfg: cfg}
	if got := w.autostartConfigPath(); got != "" {
		t.Errorf("autostartConfigPath() = %q, want it omitted", got)
	}
}
