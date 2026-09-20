//go:build !nobrowser

package app

import "testing"

// The default build mounts the CDP browser suite. Constructing the tools
// must not launch a browser — that happens lazily on first use.
func TestNewRegistersBrowserToolsWhenEnabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.Browser.Enabled = true
	a := newTestApp(t, cfg)

	for _, name := range []string{
		"browser_navigate", "browser_read", "browser_click", "browser_fill",
		"browser_screenshot", "browser_eval", "browser_back",
	} {
		if _, ok := a.Registry.Get(name); !ok {
			t.Errorf("browser tool %q not registered", name)
		}
	}
}

// The bounded executor rides the same session as the step tools, and it is
// mounted only where a decision can change what happens. Shadow asks every
// question and acts on none, so it mounts no tool that acts.
func TestBrowserRunMountsOnlyWhenDecisionsAreActive(t *testing.T) {
	for mode, want := range map[string]bool{"off": false, "shadow": false, "active": true} {
		cfg := testConfig(t)
		cfg.Browser.Enabled = true
		cfg.Decision.Mode = mode
		// Never build a virtualenv from a test: the supervisor reports
		// itself down instead, which is not the same as being absent.
		no := false
		cfg.Decision.AutoInstall = &no
		a := newTestApp(t, cfg)
		_, ok := a.Registry.Get("browser_run")
		if ok != want {
			t.Errorf("mode %s: browser_run registered = %v, want %v", mode, ok, want)
		}
		if (a.Decisions != nil) != (mode != "off") {
			t.Errorf("mode %s: model built = %v", mode, a.Decisions != nil)
		}
	}
	// Active, but the browser scenario switched off by name.
	cfg := testConfig(t)
	cfg.Browser.Enabled = true
	cfg.Decision.Mode = "active"
	no := false
	cfg.Decision.AutoInstall = &no
	off := false
	cfg.Decision.Browser = &off
	if _, ok := newTestApp(t, cfg).Registry.Get("browser_run"); ok {
		t.Error("browser_run registered with decision.browser off")
	}
}

// Decisions need nothing configured: a default config has them on, with a
// model this machine runs and no credential anywhere.
func TestDecisionsAreOnWithNothingConfigured(t *testing.T) {
	cfg := testConfig(t)
	no := false
	cfg.Decision.AutoInstall = &no
	if !cfg.Decision.On() || !cfg.Decision.Active() {
		t.Fatalf("a default config has decisions off: %+v", cfg.Decision)
	}
	a := newTestApp(t, cfg)
	if a.Decisions == nil {
		t.Fatal("no decision model was built")
	}
	if a.Decisions.Healthy() {
		t.Error("a model that was never installed reads as healthy")
	}
	// And it is stopped with the App rather than left running.
	a.Close()
	if a.Decisions.Healthy() {
		t.Error("the model outlived the App")
	}
}
