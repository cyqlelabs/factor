// Package config loads, defaults, persists, and redacts Factor's configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/caarlos0/env/v11"
)

const Version = 1

// Config is the root configuration. Channel sections stay raw JSON so
// connectors can define and decode their own config without touching core.
type Config struct {
	ConfigVersion int                        `json:"version"`
	Agent         AgentConfig                `json:"agent"`
	Provider      ProviderConfig             `json:"provider"`
	Memory        MemoryConfig               `json:"memory"`
	Channels      map[string]json.RawMessage `json:"channels,omitempty"`
	MCP           MCPConfig                  `json:"mcp"`
	Tools         ToolsConfig                `json:"tools"`
	Desktop       DesktopConfig              `json:"desktop"`
	Browser       BrowserConfig              `json:"browser"`
	Heartbeat     HeartbeatConfig            `json:"heartbeat"`
	Gateway       GatewayConfig              `json:"gateway"`

	path string
}

type AgentConfig struct {
	Workspace           string `json:"workspace" env:"FACTOR_WORKSPACE"`
	MaxToolIterations   int    `json:"max_tool_iterations" env:"FACTOR_MAX_TOOL_ITERATIONS"`
	MaxConcurrentTurns  int    `json:"max_concurrent_turns"`
	ContextWindowTokens int    `json:"context_window_tokens"`
	SummarizeAtPercent  int    `json:"summarize_at_percent"`
	SummarizeAtMessages int    `json:"summarize_at_messages"`
	KeepRecentMessages  int    `json:"keep_recent_messages"`
	ExtraInstructions   string `json:"extra_instructions,omitempty"`
}

// Candidate identifies one provider+model combination in the failover chain.
// A nil Reasoning inherits the provider-level setting.
type Candidate struct {
	Type      string           `json:"type"`
	APIKey    string           `json:"api_key,omitempty"`
	APIBase   string           `json:"api_base,omitempty"`
	Model     string           `json:"model"`
	Reasoning *ReasoningConfig `json:"reasoning,omitempty"`
}

// ReasoningConfig asks the model to think before answering. Each backend
// spells this differently (OpenRouter takes a reasoning object, OpenAI and
// Groq take reasoning_effort, Anthropic takes a thinking budget); Factor
// translates one setting into whichever the active provider understands.
// The fields are written even when empty: the section is overlaid on top of
// the defaults at load time, so an omitted "effort" would silently restore
// the default xhigh instead of the "off" the user asked for.
type ReasoningConfig struct {
	Effort    string `json:"effort"`     // xhigh | high | medium | low | minimal | none
	MaxTokens int    `json:"max_tokens"` // explicit thinking budget; wins over effort
	Exclude   bool   `json:"exclude"`    // think, but keep the reasoning out of the reply
}

// IsZero reports that nothing was configured (no reasoning parameters sent).
func (r ReasoningConfig) IsZero() bool {
	return r.Effort == "" && r.MaxTokens == 0 && !r.Exclude
}

// Off reports an explicit opt-out.
func (r ReasoningConfig) Off() bool { return r.Effort == "none" && r.MaxTokens == 0 }

type ProviderConfig struct {
	Type             string          `json:"type" env:"FACTOR_PROVIDER_TYPE"`
	APIKey           string          `json:"api_key,omitempty" env:"FACTOR_PROVIDER_API_KEY"`
	APIBase          string          `json:"api_base,omitempty" env:"FACTOR_PROVIDER_API_BASE"`
	Model            string          `json:"model" env:"FACTOR_PROVIDER_MODEL"`
	Reasoning        ReasoningConfig `json:"reasoning"`
	MaxTokens        int             `json:"max_tokens"`
	Temperature      float64         `json:"temperature,omitempty"`
	Fallbacks        []Candidate     `json:"fallbacks,omitempty"`
	MaxRetries       int             `json:"max_retries"`
	RetryBackoffSecs int             `json:"retry_backoff_secs"`
}

// Candidates returns the primary candidate followed by the fallbacks, each
// carrying the reasoning settings it should use.
func (p ProviderConfig) Candidates() []Candidate {
	primary := Candidate{Type: p.Type, APIKey: p.APIKey, APIBase: p.APIBase, Model: p.Model}
	reasoning := p.Reasoning
	primary.Reasoning = &reasoning
	out := []Candidate{primary}
	for _, f := range p.Fallbacks {
		if f.Reasoning == nil {
			inherited := p.Reasoning
			f.Reasoning = &inherited
		}
		out = append(out, f)
	}
	return out
}

type MemoryConfig struct {
	Mode                string   `json:"mode" env:"FACTOR_MEMORY_MODE"` // sidecar | external | off
	URL                 string   `json:"url,omitempty" env:"FACTOR_MEMORY_URL"`
	Command             string   `json:"command"`
	AutoInstall         bool     `json:"auto_install"` // install smrti (uv/pipx/pip/venv) when missing
	KeepAlive           bool     `json:"keep_alive"`   // sidecar outlives Factor; later runs adopt it warm
	Host                string   `json:"host"`
	Port                int      `json:"port" env:"FACTOR_MEMORY_PORT"`
	DBPath              string   `json:"db_path"`
	Tenant              string   `json:"tenant"`
	Space               string   `json:"space"`
	Personality         string   `json:"personality" env:"FACTOR_MEMORY_PERSONALITY"`
	APIKey              string   `json:"api_key,omitempty" env:"FACTOR_MEMORY_API_KEY"`
	RecallTopK          int      `json:"recall_top_k"`
	RecallMinConfidence float64  `json:"recall_min_confidence"`
	QueryContextMsgs    int      `json:"query_context_msgs"`
	QueryMaxChars       int      `json:"query_max_chars"`
	InjectMaxChars      int      `json:"inject_max_chars"`
	ReflectIntervalSecs int      `json:"reflect_interval_secs"`
	ExtractMode         string   `json:"extract_mode,omitempty"` // hybrid | llm | local | "" = auto
	ExtractURL          string   `json:"extract_url,omitempty"`
	ExtractModel        string   `json:"extract_model,omitempty"`
	IgnorePatterns      []string `json:"ignore_patterns,omitempty"`
	StartupTimeoutSecs  int      `json:"startup_timeout_secs"`
}

// BaseURL returns the effective smrti endpoint.
func (m MemoryConfig) BaseURL() string {
	if m.URL != "" {
		return m.URL
	}
	return fmt.Sprintf("http://%s:%d", m.Host, m.Port)
}

type MCPServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Enabled *bool             `json:"enabled,omitempty"` // nil means enabled
}

func (s MCPServer) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

type MCPConfig struct {
	Servers map[string]MCPServer `json:"servers,omitempty"`
}

type ToolsConfig struct {
	Disabled                  []string `json:"disabled,omitempty"`
	RestrictToWorkspace       bool     `json:"restrict_to_workspace"`
	AllowReadOutsideWorkspace bool     `json:"allow_read_outside_workspace"`
	AllowPaths                []string `json:"allow_paths,omitempty"`
	ExecTimeoutSecs           int      `json:"exec_timeout_secs"`
	EnableDenyPatterns        bool     `json:"enable_deny_patterns"`
	CustomDenyPatterns        []string `json:"custom_deny_patterns,omitempty"`
	AllowInstall              bool     `json:"allow_install"`
}

// IsToolEnabled reports whether a tool name survives the user's disabled list.
func (t ToolsConfig) IsToolEnabled(name string) bool {
	for _, d := range t.Disabled {
		if d == name {
			return false
		}
	}
	return true
}

// DesktopConfig controls the desktop-control tools (windows, screenshots,
// mouse, keyboard, clipboard, notifications). Enabled is a tri-state: unset
// means "register them when a graphical session is detected", which keeps a
// headless server's prompt free of tools that could never work there.
type DesktopConfig struct {
	Enabled       *bool  `json:"enabled,omitempty"`
	ScreenshotDir string `json:"screenshot_dir,omitempty"`
}

// Register reports whether the desktop tools should be registered.
func (d DesktopConfig) Register(hasDisplay bool) bool {
	if d.Enabled != nil {
		return *d.Enabled
	}
	return hasDisplay
}

// BrowserConfig controls the CDP browser integration. With AttachURL empty,
// Factor probes the standard DevTools port and falls back to launching a
// managed instance (visible unless Headless).
type BrowserConfig struct {
	Enabled     bool   `json:"enabled"`
	AttachURL   string `json:"attach_url,omitempty" env:"FACTOR_BROWSER_ATTACH_URL"`
	Command     string `json:"command,omitempty"`
	Headless    bool   `json:"headless"`
	UserDataDir string `json:"user_data_dir,omitempty"`
}

type HeartbeatConfig struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
}

type GatewayConfig struct {
	Host string `json:"host" env:"FACTOR_GATEWAY_HOST"`
	Port int    `json:"port" env:"FACTOR_GATEWAY_PORT"`
}

// Home returns $FACTOR_HOME or ~/.factor.
func Home() string {
	if h := os.Getenv("FACTOR_HOME"); h != "" {
		return expandHome(h)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".factor"
	}
	return filepath.Join(home, ".factor")
}

// DefaultPath returns the default config file location.
func DefaultPath() string { return filepath.Join(Home(), "config.json") }

// Default returns a fully populated configuration.
func Default() *Config {
	home := Home()
	return &Config{
		ConfigVersion: Version,
		Agent: AgentConfig{
			Workspace:           filepath.Join(home, "workspace"),
			MaxToolIterations:   20,
			MaxConcurrentTurns:  4,
			ContextWindowTokens: 65536,
			SummarizeAtPercent:  75,
			SummarizeAtMessages: 40,
			KeepRecentMessages:  8,
		},
		Provider: ProviderConfig{
			Type:             "openrouter",
			Model:            "google/gemini-pro-latest",
			Reasoning:        ReasoningConfig{Effort: "xhigh"},
			MaxTokens:        4096,
			MaxRetries:       2,
			RetryBackoffSecs: 2,
		},
		Memory: MemoryConfig{
			Mode:                "sidecar",
			Command:             "smrti",
			AutoInstall:         true,
			KeepAlive:           true,
			Host:                "127.0.0.1",
			Port:                8420,
			DBPath:              filepath.Join(home, "memory.db"),
			Tenant:              "default",
			Space:               "main",
			Personality:         "balanced",
			RecallTopK:          5,
			RecallMinConfidence: 0.3,
			QueryContextMsgs:    5,
			QueryMaxChars:       500,
			InjectMaxChars:      500,
			ReflectIntervalSecs: 60,
			IgnorePatterns:      []string{"^HEARTBEAT_OK$", "^# Heartbeat"},
			StartupTimeoutSecs:  90,
		},
		MCP: MCPConfig{Servers: map[string]MCPServer{}},
		Tools: ToolsConfig{
			RestrictToWorkspace: true,
			ExecTimeoutSecs:     120,
			EnableDenyPatterns:  true,
			AllowInstall:        true,
		},
		Browser: BrowserConfig{
			Enabled:     true,
			UserDataDir: filepath.Join(home, "browser"),
		},
		Heartbeat: HeartbeatConfig{Enabled: true, IntervalMinutes: 30},
		Gateway:   GatewayConfig{Host: "127.0.0.1", Port: 8720},
	}
}

// Load reads the config file (DefaultPath when path is empty), overlays it on
// defaults, then applies FACTOR_* environment overrides.
func Load(path string) (*Config, error) {
	cfg, err := loadFile(path)
	if err != nil {
		return nil, err
	}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("env overrides: %w", err)
	}
	cfg.normalize()
	return cfg, nil
}

// loadFile reads defaults + file WITHOUT env overrides — the form safe to
// write back to disk (env secrets must never be persisted).
func loadFile(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	cfg := Default()
	cfg.path = path
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		// defaults only
	case err != nil:
		return nil, fmt.Errorf("read config: %w", err)
	default:
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	return cfg, nil
}

var fileMu sync.Mutex

// Update atomically load-modifies-saves the config FILE. The live in-memory
// Config is deliberately immutable after startup (concurrent turns read it
// lock-free); durable changes go through here and apply on restart.
func Update(path string, fn func(*Config) error) error {
	fileMu.Lock()
	defer fileMu.Unlock()
	cfg, err := loadFile(path)
	if err != nil {
		return err
	}
	if err := fn(cfg); err != nil {
		return err
	}
	return cfg.Save()
}

// ReadFile returns a fresh file-backed copy (no env overlay), for
// introspection consistent with what Update sees.
func ReadFile(path string) (*Config, error) {
	fileMu.Lock()
	defer fileMu.Unlock()
	return loadFile(path)
}

// Path returns where this config was loaded from (or will be saved to).
func (c *Config) Path() string {
	if c.path == "" {
		return DefaultPath()
	}
	return c.path
}

// Save writes the config as indented JSON, creating parent directories.
func (c *Config) Save() error {
	path := c.Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Config) normalize() {
	c.Agent.Workspace = expandHome(c.Agent.Workspace)
	c.Memory.DBPath = expandHome(c.Memory.DBPath)
	if c.Agent.MaxToolIterations <= 0 {
		c.Agent.MaxToolIterations = 20
	}
	if c.Agent.MaxConcurrentTurns <= 0 {
		c.Agent.MaxConcurrentTurns = 4
	}
	if c.Provider.MaxTokens <= 0 {
		c.Provider.MaxTokens = 4096
	}
	if c.Agent.ContextWindowTokens <= 0 {
		c.Agent.ContextWindowTokens = 4 * c.Provider.MaxTokens
	}
}

// SecretValues returns every non-empty secret for output filtering.
func (c *Config) SecretValues() []string {
	secrets := []string{c.Provider.APIKey, c.Memory.APIKey}
	for _, f := range c.Provider.Fallbacks {
		secrets = append(secrets, f.APIKey)
	}
	for _, raw := range c.Channels {
		secrets = append(secrets, rawSecretValues(raw)...)
	}
	for _, srv := range c.MCP.Servers {
		for _, v := range srv.Env {
			if len(v) >= 8 { // short values create false positives
				secrets = append(secrets, v)
			}
		}
	}
	out := secrets[:0]
	for _, s := range secrets {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// FilterSecrets replaces occurrences of secret values in s with a placeholder.
func (c *Config) FilterSecrets(s string) string {
	for _, secret := range c.SecretValues() {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	return s
}

var secretKeyRe = []string{"api_key", "apikey", "token", "secret", "password"}

func isSecretKey(key string) bool {
	k := strings.ToLower(key)
	for _, s := range secretKeyRe {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// RedactedMap returns the config as a generic map with secret-looking values masked.
func (c *Config) RedactedMap() (map[string]any, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	redactMap(m)
	return m, nil
}

func redactMap(m map[string]any) {
	for k, v := range m {
		switch val := v.(type) {
		case map[string]any:
			redactMap(val)
		case []any:
			for _, item := range val {
				if sub, ok := item.(map[string]any); ok {
					redactMap(sub)
				}
			}
		case string:
			if isSecretKey(k) && val != "" {
				m[k] = "[redacted]"
			}
		}
	}
}

func rawSecretValues(raw json.RawMessage) []string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	var out []string
	for k, v := range m {
		if s, ok := v.(string); ok && isSecretKey(k) && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}
