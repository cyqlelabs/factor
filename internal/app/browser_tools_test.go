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
// mounted only where it can decide: an active decision mode with a key.
// Shadow asks questions but acts on none, so it mounts no tool that acts.
func TestBrowserRunMountsOnlyUnderAnActiveDecider(t *testing.T) {
	for mode, want := range map[string]bool{"off": false, "shadow": false, "active": true} {
		cfg := testConfig(t)
		cfg.Browser.Enabled = true
		cfg.Decision.Mode = mode
		cfg.Decision.APIKey = "ts-test-key-1234"
		cfg.Decision.APIBase = "http://127.0.0.1:1"
		a := newTestApp(t, cfg)
		_, ok := a.Registry.Get("browser_run")
		if ok != want {
			t.Errorf("mode %s: browser_run registered = %v, want %v", mode, ok, want)
		}
	}
	// Active, but the browser scenario switched off by name.
	cfg := testConfig(t)
	cfg.Browser.Enabled = true
	cfg.Decision.Mode = "active"
	cfg.Decision.APIKey = "ts-test-key-1234"
	off := false
	cfg.Decision.Browser = &off
	if _, ok := newTestApp(t, cfg).Registry.Get("browser_run"); ok {
		t.Error("browser_run registered with decision.browser off")
	}
	// Active with no key is off.
	cfg = testConfig(t)
	cfg.Browser.Enabled = true
	cfg.Decision.Mode = "active"
	if _, ok := newTestApp(t, cfg).Registry.Get("browser_run"); ok {
		t.Error("browser_run registered with no key")
	}
}
