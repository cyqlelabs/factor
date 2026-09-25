package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The point of the decision model is that there is nothing to set up: no key,
// no endpoint, no provider, no model name. A default config has it on.
func TestDecisionsAreOnWithNothingConfigured(t *testing.T) {
	d := Default().Decision
	if !d.On() || !d.Active() {
		t.Fatalf("decisions are off by default: %+v", d)
	}
	if d.TimeoutMS != 4000 || d.MinConfidence != 0.6 || d.CacheEntries <= 0 {
		t.Errorf("defaults = %+v", d)
	}
	if !d.BrowserOn() || !d.RecoverOn() || !d.InduceOn() {
		t.Error("the scenarios should follow the mode")
	}
	// The completion check is the one scenario that has to be asked for:
	// measured on real turns, its verdicts were noise held back by the bar.
	if d.VerifyOn() {
		t.Error("the completion check ran unasked")
	}
	// Nothing about it is a credential, so nothing about it is in the
	// config's secret list.
	cfg := Default()
	if len(cfg.SecretValues()) != 0 {
		t.Errorf("a default config carries secrets: %v", cfg.SecretValues())
	}
}

func TestDecisionModes(t *testing.T) {
	cases := map[string]struct{ on, active bool }{
		"active": {true, true},
		"shadow": {true, false},
		"off":    {false, false},
	}
	for mode, want := range cases {
		d := DecisionConfig{Mode: mode}
		if d.On() != want.on || d.Active() != want.active {
			t.Errorf("%s: on=%v active=%v", mode, d.On(), d.Active())
		}
	}
	// A scenario switched off by name stays off while the rest follow.
	off, on := false, true
	d := DecisionConfig{Mode: "active", Browser: &off, Verify: &on}
	if d.BrowserOn() || !d.VerifyOn() || !d.RecoverOn() || !d.InduceOn() {
		t.Error("per-scenario switches did not apply")
	}
	if (DecisionConfig{Mode: "off", Browser: &off}).RecoverOn() {
		t.Error("a scenario ran with the mode off")
	}
}

func TestDecisionNormalizesWhatWasHandEdited(t *testing.T) {
	cfg := Default()
	cfg.Decision.Mode = "sometimes"
	cfg.Decision.TimeoutMS = -5
	cfg.Decision.MinConfidence = 7
	cfg.Decision.CacheEntries = -1
	cfg.normalize()
	d := cfg.Decision
	// An unreadable mode falls back to the default rather than to off: this
	// is a core feature, and a typo should not quietly disable it.
	if d.Mode != "active" || d.TimeoutMS != 4000 || d.MinConfidence != 0.6 || d.CacheEntries != 0 {
		t.Errorf("normalized = %+v", d)
	}
	for _, mode := range []string{"off", "shadow", "active"} {
		cfg.Decision.Mode = mode
		cfg.normalize()
		if cfg.Decision.Mode != mode {
			t.Errorf("normalize changed %q to %q", mode, cfg.Decision.Mode)
		}
	}
}

// The escape hatches exist for the machine that needs them, and are reachable
// the way every other setting is.
func TestDecisionEscapeHatchesAreReachable(t *testing.T) {
	cfg := Default()
	for key, value := range map[string]any{
		"decision.mode":          "shadow",
		"decision.port":          9999,
		"decision.device":        "cuda",
		"decision.auto_install":  false,
		"decision.cache_entries": 128,
		"decision.thresholds":    map[string]any{"completion": 0.8},
	} {
		if err := cfg.Set(key, value); err != nil {
			t.Errorf("Set %s: %v", key, err)
		}
	}
	d := cfg.Decision
	if d.Mode != "shadow" || d.Port != 9999 || d.Device != "cuda" || d.CacheEntries != 128 ||
		d.AutoInstall == nil || *d.AutoInstall || d.Thresholds["completion"] != 0.8 {
		t.Errorf("decision = %+v", d)
	}
}

// A config written before decisions existed reads as a config with them on:
// the section is absent, and absent means the defaults.
func TestAnOlderConfigGetsDecisionsOnUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"agent": {"max_tool_iterations": 12}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Decision.On() || cfg.Decision.Mode != "active" {
		t.Errorf("an upgraded config has decisions off: %+v", cfg.Decision)
	}
	if cfg.Agent.MaxToolIterations != 12 {
		t.Errorf("the rest of the config was not preserved: %d", cfg.Agent.MaxToolIterations)
	}
}
