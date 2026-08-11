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
