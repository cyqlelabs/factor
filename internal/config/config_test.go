package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaultsWhenMissing(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.MaxToolIterations != 20 {
		t.Errorf("MaxToolIterations = %d, want 20", cfg.Agent.MaxToolIterations)
	}
	if !cfg.Tools.RestrictToWorkspace {
		t.Error("RestrictToWorkspace should default to true")
	}
	if cfg.Memory.Mode != "sidecar" {
		t.Errorf("Memory.Mode = %q, want sidecar", cfg.Memory.Mode)
	}
}

func TestLoadOverlayAndEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACTOR_HOME", dir)
	path := filepath.Join(dir, "config.json")
	body := `{"provider":{"type":"anthropic","model":"claude-sonnet-5","api_key":"file-key"},"tools":{"restrict_to_workspace":false}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FACTOR_PROVIDER_API_KEY", "env-key")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.Type != "anthropic" {
		t.Errorf("Type = %q", cfg.Provider.Type)
	}
	if cfg.Provider.APIKey != "env-key" {
		t.Errorf("env override lost: %q", cfg.Provider.APIKey)
	}
	if cfg.Tools.RestrictToWorkspace {
		t.Error("file override of restrict_to_workspace lost")
	}
	// untouched defaults survive the overlay
	if cfg.Memory.RecallTopK != 5 {
		t.Errorf("RecallTopK = %d", cfg.Memory.RecallTopK)
	}
}

// The extraction settings are what a small box tunes to keep smrti from
// loading a local model, so each one has to be reachable from the
// environment like the rest of the memory section.
func TestExtractSettingsHaveEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACTOR_HOME", dir)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"memory":{"extract_mode":"hybrid"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FACTOR_MEMORY_EXTRACT_MODE", "llm")
	t.Setenv("FACTOR_MEMORY_EXTRACT_URL", "https://extract.example/v1")
	t.Setenv("FACTOR_MEMORY_EXTRACT_MODEL", "google/gemini-3.1-flash-lite")
	t.Setenv("FACTOR_MEMORY_EXTRACT_API_KEY", "sk-extract")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.ExtractMode != "llm" {
		t.Errorf("ExtractMode = %q, want the env override to win over the file", cfg.Memory.ExtractMode)
	}
	if cfg.Memory.ExtractURL != "https://extract.example/v1" {
		t.Errorf("ExtractURL = %q", cfg.Memory.ExtractURL)
	}
	if cfg.Memory.ExtractModel != "google/gemini-3.1-flash-lite" {
		t.Errorf("ExtractModel = %q", cfg.Memory.ExtractModel)
	}
	if cfg.Memory.ExtractAPIKey != "sk-extract" {
		t.Errorf("ExtractAPIKey = %q", cfg.Memory.ExtractAPIKey)
	}
}

func TestContextWindowDerivedFromMaxTokens(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACTOR_HOME", dir)
	path := filepath.Join(dir, "config.json")
	body := `{"agent":{"context_window_tokens":0},"provider":{"max_tokens":2000}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.ContextWindowTokens != 8000 {
		t.Errorf("ContextWindowTokens = %d, want 8000", cfg.Agent.ContextWindowTokens)
	}
}

func TestSecretsFilteringAndRedaction(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	cfg := Default()
	cfg.Provider.APIKey = "sk-super-secret"
	cfg.Channels = map[string]json.RawMessage{
		"telegram": json.RawMessage(`{"token":"tg-token-value","allow_from":["1"]}`),
	}
	cfg.MCP.Servers = map[string]MCPServer{
		"gh": {Command: "gh-mcp", Env: map[string]string{"GH_TOKEN": "ghp_longtoken"}},
	}

	filtered := cfg.FilterSecrets("key is sk-super-secret and tg-token-value and ghp_longtoken")
	for _, leak := range []string{"sk-super-secret", "tg-token-value", "ghp_longtoken"} {
		if strings.Contains(filtered, leak) {
			t.Errorf("secret %q leaked: %s", leak, filtered)
		}
	}

	m, err := cfg.RedactedMap()
	if err != nil {
		t.Fatal(err)
	}
	prov := m["provider"].(map[string]any)
	if prov["api_key"] != "[redacted]" {
		t.Errorf("api_key not redacted: %v", prov["api_key"])
	}
	if prov["model"] == "[redacted]" {
		t.Error("model should not be redacted")
	}
}

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACTOR_HOME", dir)
	cfg := Default()
	cfg.path = filepath.Join(dir, "config.json")
	cfg.Provider.Model = "test-model"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	re, err := Load(cfg.path)
	if err != nil {
		t.Fatal(err)
	}
	if re.Provider.Model != "test-model" {
		t.Errorf("round trip lost model: %q", re.Provider.Model)
	}
}

func TestEnsureWorkspace(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "workspace")
	if err := EnsureWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"AGENT.md", "SOUL.md", "USER.md", "HEARTBEAT.md", "skills", "sessions", "instructions"} {
		if _, err := os.Stat(filepath.Join(ws, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	// existing files are not overwritten
	custom := []byte("customized")
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), custom, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(ws, "AGENT.md"))
	if string(got) != "customized" {
		t.Error("EnsureWorkspace overwrote an existing file")
	}
}

func TestEnsureWorkspaceCarriesForwardDefaults(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "workspace")
	if err := EnsureWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	// a file left at any default this build superseded is brought forward
	soul := filepath.Join(ws, "SOUL.md")
	for i, old := range supersededTemplates["SOUL.md"] {
		if err := os.WriteFile(soul, []byte(old), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := EnsureWorkspace(ws); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(soul); string(got) != workspaceTemplates["SOUL.md"] {
			t.Errorf("superseded SOUL.md %d not carried forward: %q", i, got)
		}
	}
	// one the user rewrote is not, even when its name has superseded versions
	user := filepath.Join(ws, "USER.md")
	if err := os.WriteFile(user, []byte("# User\n\nNico, Buenos Aires.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// an unreadable path is left as it is rather than failing the boot
	if err := os.Remove(filepath.Join(ws, "HEARTBEAT.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, "HEARTBEAT.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := EnsureWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(user); string(got) != "# User\n\nNico, Buenos Aires.\n" {
		t.Errorf("edited USER.md was overwritten: %q", got)
	}
	if info, err := os.Stat(filepath.Join(ws, "HEARTBEAT.md")); err != nil || !info.IsDir() {
		t.Errorf("unreadable HEARTBEAT.md was not left alone: %v", err)
	}
}

func TestIsToolEnabled(t *testing.T) {
	tc := ToolsConfig{Disabled: []string{"exec"}}
	if tc.IsToolEnabled("exec") {
		t.Error("exec should be disabled")
	}
	if !tc.IsToolEnabled("read_file") {
		t.Error("read_file should be enabled")
	}
}

func TestCostStartsCountingButNeverCapping(t *testing.T) {
	cfg := Default()
	if !cfg.Cost.Track {
		t.Error("tracking is off by default, so the status bar starts blank")
	}
	if !cfg.Cost.Budget.Off() {
		t.Errorf("a cap was inherited rather than asked for: %+v", cfg.Cost.Budget)
	}
	if cfg.Cost.Budget.Period != "month" {
		t.Errorf("default period = %q", cfg.Cost.Budget.Period)
	}
	if got := (BudgetConfig{GlobalUSD: 5}); got.Off() {
		t.Error("a configured global cap reported itself off")
	}
}

func TestCostNormalizesWhatWasHandEdited(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(
		`{"cost":{"refresh_hours":0,"budget":{"period":"fortnight","session_usd":2}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cost.RefreshHours != 24 {
		t.Errorf("refresh hours = %d, want the default back", cfg.Cost.RefreshHours)
	}
	if cfg.Cost.Budget.Period != "month" {
		t.Errorf("period = %q, want an unknown one to fall back to month", cfg.Cost.Budget.Period)
	}
	if cfg.Cost.Budget.SessionUSD != 2 {
		t.Errorf("session cap = %v, want the one that was written", cfg.Cost.Budget.SessionUSD)
	}
}

func TestBudgetCapsCanComeFromTheEnvironment(t *testing.T) {
	t.Setenv("FACTOR_BUDGET_SESSION_USD", "1.25")
	t.Setenv("FACTOR_BUDGET_GLOBAL_USD", "40")
	cfg, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cost.Budget.SessionUSD != 1.25 || cfg.Cost.Budget.GlobalUSD != 40 {
		t.Errorf("budget = %+v", cfg.Cost.Budget)
	}
}
