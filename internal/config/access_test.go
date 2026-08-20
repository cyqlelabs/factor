package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func tempConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FACTOR_HOME", dir)
	cfg := Default()
	cfg.path = filepath.Join(dir, "config.json")
	return cfg
}

func TestGet(t *testing.T) {
	cfg := tempConfig(t)
	cfg.Provider.APIKey = "sk-secret"

	whole, err := cfg.Get("")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := whole.(map[string]any); !ok {
		t.Fatalf("empty key returned %T, want the whole config map", whole)
	}

	model, err := cfg.Get("provider.model")
	if err != nil {
		t.Fatal(err)
	}
	if model != cfg.Provider.Model {
		t.Errorf("provider.model = %v", model)
	}

	key, err := cfg.Get("provider.api_key")
	if err != nil {
		t.Fatal(err)
	}
	if key != "[redacted]" {
		t.Errorf("Get must redact secrets, got %v", key)
	}

	if _, err := cfg.Get("provider.nope"); err == nil {
		t.Error("missing key accepted")
	}
	if _, err := cfg.Get("provider.model.deeper"); err == nil {
		t.Error("descending into a scalar accepted")
	}
	if _, err := cfg.Get("nosuchsection"); err == nil {
		t.Error("missing section accepted")
	}
}

func TestSet(t *testing.T) {
	cfg := tempConfig(t)

	if err := cfg.Set("provider.model", "new-model"); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.Model != "new-model" {
		t.Errorf("model = %q", cfg.Provider.Model)
	}
	// unrelated fields survive the round trip
	if cfg.Memory.RecallTopK != 5 {
		t.Errorf("Set clobbered unrelated fields: RecallTopK = %d", cfg.Memory.RecallTopK)
	}

	if err := cfg.Set("", "x"); err == nil {
		t.Error("empty key accepted")
	}
	if err := cfg.Set("provider.max_tokens", "not-a-number"); err == nil {
		t.Error("type-invalid value accepted")
	}
	if err := cfg.Set("provider.model.sub", "x"); err == nil {
		t.Error("descending into a scalar accepted")
	}

	// a new nested section is created on demand
	if err := cfg.Set("mcp.servers", map[string]any{
		"gh": map[string]any{"command": "gh-mcp"},
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.MCP.Servers["gh"].Command != "gh-mcp" {
		t.Errorf("servers = %+v", cfg.MCP.Servers)
	}

	// a zero context window survives Set: it means auto, not a gap to refill
	if err := cfg.Set("agent.context_window_tokens", 0); err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.ContextWindowTokens != 0 {
		t.Errorf("zero did not survive Set: %d", cfg.Agent.ContextWindowTokens)
	}
	// the path survives replacement of the struct
	if cfg.Path() != filepath.Join(Home(), "config.json") {
		t.Errorf("Set lost the config path: %q", cfg.Path())
	}
}

func TestUpdateAndReadFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACTOR_HOME", dir)
	path := filepath.Join(dir, "config.json")

	// Update creates the file from defaults when it does not exist yet
	if err := Update(path, func(c *Config) error {
		return c.Set("heartbeat.interval_minutes", 45)
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Heartbeat.IntervalMinutes != 45 {
		t.Errorf("interval = %d", got.Heartbeat.IntervalMinutes)
	}

	// a callback error aborts without writing
	wantErr := errSentinel{}
	if err := Update(path, func(c *Config) error { return wantErr }); err != wantErr {
		t.Fatalf("Update error = %v", err)
	}
	got, _ = ReadFile(path)
	if got.Heartbeat.IntervalMinutes != 45 {
		t.Error("failed Update still wrote the file")
	}

	if err := Update(filepath.Join(dir, "bad.json"), func(*Config) error { return nil }); err != nil {
		t.Errorf("Update on a missing file should start from defaults: %v", err)
	}
}

type errSentinel struct{}

func (errSentinel) Error() string { return "sentinel" }

func TestUpdateExcludesEnvOverrides(t *testing.T) {
	// Env-supplied secrets must never be persisted into the file.
	dir := t.TempDir()
	t.Setenv("FACTOR_HOME", dir)
	path := filepath.Join(dir, "config.json")
	t.Setenv("FACTOR_PROVIDER_API_KEY", "env-only-secret")

	if err := Update(path, func(c *Config) error { return c.Set("provider.model", "m") }); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "env-only-secret") {
		t.Errorf("env secret was persisted to disk:\n%s", data)
	}
}

func TestUpdateIsSerialized(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACTOR_HOME", dir)
	path := filepath.Join(dir, "config.json")

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = Update(path, func(c *Config) error {
				c.MCP.Servers[string(rune('a'+n))] = MCPServer{Command: "x"}
				return nil
			})
		}(i)
	}
	wg.Wait()
	got, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.MCP.Servers) != 8 {
		t.Errorf("concurrent updates lost writes: %d servers", len(got.MCP.Servers))
	}
}

func TestLoadFileRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACTOR_HOME", dir)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("malformed config accepted")
	}
	if _, err := ReadFile(path); err == nil {
		t.Error("malformed config accepted by ReadFile")
	}
}

func TestLoadUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{}"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("unreadable config accepted")
	}
}

func TestSaveFailsOnUnwritableParent(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.path = filepath.Join(blocker, "config.json") // parent is a regular file
	if err := cfg.Save(); err == nil {
		t.Error("Save into an invalid parent succeeded")
	}
}

func TestPathDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACTOR_HOME", dir)
	cfg := Default()
	if cfg.Path() != DefaultPath() {
		t.Errorf("Path() = %q, want %q", cfg.Path(), DefaultPath())
	}
	if DefaultPath() != filepath.Join(dir, "config.json") {
		t.Errorf("DefaultPath() = %q", DefaultPath())
	}
}

func TestHomeExpandsTilde(t *testing.T) {
	t.Setenv("FACTOR_HOME", "~/factor-home-test")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got := Home(); got != filepath.Join(home, "factor-home-test") {
		t.Errorf("Home() = %q", got)
	}
	t.Setenv("FACTOR_HOME", "")
	if got := Home(); got != filepath.Join(home, ".factor") {
		t.Errorf("default Home() = %q", got)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	cases := map[string]string{
		"~":          home,
		"~/x/y":      filepath.Join(home, "x", "y"),
		"/abs/path":  "/abs/path",
		"relative":   "relative",
		"~notatilde": "~notatilde",
	}
	for in, want := range cases {
		if got := expandHome(in); got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCandidatesAndBaseURL(t *testing.T) {
	p := ProviderConfig{
		Type: "openai", APIKey: "k", APIBase: "b", Model: "m",
		Fallbacks: []Candidate{{Type: "ollama", Model: "llama"}},
	}
	cands := p.Candidates()
	if len(cands) != 2 || cands[0].Model != "m" || cands[1].Type != "ollama" {
		t.Fatalf("candidates = %+v", cands)
	}

	m := MemoryConfig{Host: "127.0.0.1", Port: 8420}
	if got := m.BaseURL(); got != "http://127.0.0.1:8420" {
		t.Errorf("BaseURL() = %q", got)
	}
	m.URL = "https://memory.example"
	if got := m.BaseURL(); got != "https://memory.example" {
		t.Errorf("explicit URL ignored: %q", got)
	}
}

func TestMCPServerIsEnabled(t *testing.T) {
	if !(MCPServer{}).IsEnabled() {
		t.Error("nil Enabled should mean enabled")
	}
	yes, no := true, false
	if !(MCPServer{Enabled: &yes}).IsEnabled() {
		t.Error("explicit true should be enabled")
	}
	if (MCPServer{Enabled: &no}).IsEnabled() {
		t.Error("explicit false should be disabled")
	}
}

func TestNormalizeFloors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FACTOR_HOME", dir)
	path := filepath.Join(dir, "config.json")
	body := `{"agent":{"max_tool_iterations":0,"max_concurrent_turns":-3},"provider":{"max_tokens":0}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.MaxToolIterations != 20 || cfg.Agent.MaxConcurrentTurns != 4 || cfg.Provider.MaxTokens != 4096 {
		t.Errorf("floors not applied: %+v", cfg.Agent)
	}
}

func TestRedactedMapNestedAndLists(t *testing.T) {
	cfg := tempConfig(t)
	cfg.Provider.Fallbacks = []Candidate{{Type: "openai", APIKey: "fallback-secret", Model: "m"}}
	cfg.Channels = map[string]json.RawMessage{
		"telegram": json.RawMessage(`{"token":"tg-secret"}`),
	}
	m, err := cfg.RedactedMap()
	if err != nil {
		t.Fatal(err)
	}
	prov := m["provider"].(map[string]any)
	fallbacks := prov["fallbacks"].([]any)
	if fallbacks[0].(map[string]any)["api_key"] != "[redacted]" {
		t.Errorf("list element secret not redacted: %v", fallbacks[0])
	}
	blob, _ := json.Marshal(m)
	if strings.Contains(string(blob), "fallback-secret") {
		t.Errorf("secret leaked: %s", blob)
	}
}

func TestSecretValuesSkipsShortMCPEnv(t *testing.T) {
	cfg := tempConfig(t)
	cfg.MCP.Servers = map[string]MCPServer{
		"a": {Env: map[string]string{"SHORT": "abc", "LONG_TOKEN": "abcdefghij"}},
	}
	secrets := cfg.SecretValues()
	var hasShort, hasLong bool
	for _, s := range secrets {
		if s == "abc" {
			hasShort = true
		}
		if s == "abcdefghij" {
			hasLong = true
		}
	}
	if hasShort {
		t.Error("short env value treated as a secret (would corrupt output)")
	}
	if !hasLong {
		t.Error("long env value not treated as a secret")
	}
}

func TestFilterSecretsNoOpWithoutSecrets(t *testing.T) {
	cfg := tempConfig(t)
	const text = "nothing sensitive here"
	if got := cfg.FilterSecrets(text); got != text {
		t.Errorf("FilterSecrets altered clean text: %q", got)
	}
}

func TestEnsureWorkspaceFailsOnFileCollision(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "workspace")
	if err := os.WriteFile(ws, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureWorkspace(ws); err == nil {
		t.Error("EnsureWorkspace succeeded where the workspace path is a file")
	}
}

func TestSecretValuesSkipsAnyShortCredential(t *testing.T) {
	cfg := tempConfig(t)
	cfg.Provider.APIKey = "k"
	cfg.Memory.APIKey = "sk-long-enough"
	// A one-character "key" would rewrite ordinary text wherever it appeared.
	if got := cfg.FilterSecrets("1.0k tokens in"); got != "1.0k tokens in" {
		t.Errorf("a short key corrupted output: %q", got)
	}
	if got := cfg.FilterSecrets("using sk-long-enough here"); strings.Contains(got, "sk-long-enough") {
		t.Errorf("a real key survived filtering: %q", got)
	}
}
