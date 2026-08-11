//go:build nobrowser

package app

import (
	"strings"
	"testing"
)

// The nobrowser build strips chromedp entirely: no browser tool may appear,
// even with browser.enabled set.
func TestNoBrowserBuildMountsNoBrowserTools(t *testing.T) {
	cfg := testConfig(t)
	cfg.Browser.Enabled = true
	a := newTestApp(t, cfg)

	for _, name := range a.Registry.Names() {
		if strings.HasPrefix(name, "browser_") {
			t.Errorf("browser tool %q present in a nobrowser build", name)
		}
	}
	if _, ok := a.Registry.Get("read_file"); !ok {
		t.Error("non-browser tools missing from the nobrowser build")
	}
}
