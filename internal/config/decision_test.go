package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Decisions are off until asked for, and asking for them takes a mode and a
// key: a mode with no key asks nothing, and a key with no mode changes nothing.
func TestDecisionDefaultsOffAndNeedsAModeAndAKey(t *testing.T) {
	cfg := Default()
	d := cfg.Decision
	if d.Mode != "off" || d.Model != "jev-latest" || d.TimeoutMS != 4000 || d.MinConfidence != 0.6 ||
		d.InputPricePerMillion != DefaultDecisionInputPrice {
		t.Errorf("defaults = %+v", d)
	}
	if d.On() || d.Active() || d.BrowserOn() || d.VerifyOn() || d.RecoverOn() || d.InduceOn() {
		t.Error("everything should read off by default")
	}
	d.Mode = "active"
	if d.On() {
		t.Error("a mode with no key should still be off")
	}
	d.APIKey = "ts-key"
	if !d.On() || !d.Active() || !d.BrowserOn() || !d.VerifyOn() || !d.RecoverOn() || !d.InduceOn() {
		t.Errorf("active with a key: %+v", d)
	}
	d.Mode = "shadow"
	if !d.On() || d.Active() {
		t.Error("shadow is on but not active")
	}
	off := false
	d.Browser, d.Verify = &off, &off
	if d.BrowserOn() || d.VerifyOn() || !d.RecoverOn() || !d.InduceOn() {
		t.Error("per-scenario switches did not apply")
	}
}

func TestDecisionNormalizesWhatWasHandEdited(t *testing.T) {
	cfg := Default()
	cfg.Decision.Mode = "sometimes"
	cfg.Decision.Model = ""
	cfg.Decision.TimeoutMS = -5
	cfg.Decision.MinConfidence = 7
	cfg.Decision.InputPricePerMillion = -1
	cfg.normalize()
	d := cfg.Decision
	if d.Mode != "off" || d.Model != "jev-latest" || d.TimeoutMS != 4000 || d.MinConfidence != 0.6 ||
		d.InputPricePerMillion != DefaultDecisionInputPrice {
		t.Errorf("normalized = %+v", d)
	}
	// A price of zero is a decision (a private endpoint), not a mistake.
	cfg.Decision.InputPricePerMillion = 0
	cfg.normalize()
	if cfg.Decision.InputPricePerMillion != 0 {
		t.Error("an explicit zero price was overwritten")
	}
}

// The key is a secret like every other: filtered out of tool output, redacted
// on read, and reachable from the environment.
func TestDecisionKeyIsASecretAndComesFromTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"decision": {"mode": "shadow", "api_key": "ts-secret-key-1234"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Decision.Mode != "shadow" || !cfg.Decision.On() {
		t.Errorf("loaded = %+v", cfg.Decision)
	}
	if got := cfg.FilterSecrets("key is ts-secret-key-1234 ok"); strings.Contains(got, "ts-secret") {
		t.Errorf("secret not filtered: %q", got)
	}
	m, err := cfg.RedactedMap()
	if err != nil {
		t.Fatal(err)
	}
	if sec := m["decision"].(map[string]any)["api_key"]; sec == "ts-secret-key-1234" {
		t.Error("the key was not redacted on read")
	}

	t.Setenv("FACTOR_DECISION_API_KEY", "env-key-abcdefgh")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Decision.APIKey != "env-key-abcdefgh" {
		t.Errorf("env override not applied: %q", cfg.Decision.APIKey)
	}
	// And it is a key config_set can reach.
	fresh := Default()
	if err := fresh.Set("decision.mode", "active"); err != nil || fresh.Decision.Mode != "active" {
		t.Errorf("Set decision.mode: %v (%q)", err, fresh.Decision.Mode)
	}
	if err := fresh.Set("decision.thresholds", map[string]any{"completion": 0.8}); err != nil || fresh.Decision.Thresholds["completion"] != 0.8 {
		t.Errorf("Set decision.thresholds: %v (%v)", err, fresh.Decision.Thresholds)
	}
}
