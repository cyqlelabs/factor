package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/jobs"
	"github.com/cyqlelabs/factor/internal/tools"
)

// testConfig builds a self-contained app config: memory off (no sidecar to
// spawn), browser off, no MCP servers, everything under a temp home, and a
// provider pointed at a dead local port so no test touches the network.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	cfg := config.Default()
	cfg.Agent.Workspace = filepath.Join(home, "workspace")
	cfg.Memory.Mode = "off"
	cfg.Memory.DBPath = filepath.Join(home, "memory.db")
	cfg.Provider = config.ProviderConfig{
		Type: "ollama", Model: "test", MaxTokens: 512, APIKey: "test-key",
		// A dead local port: turns fail fast and no test ever touches the network.
		APIBase: "http://127.0.0.1:1/v1", MaxRetries: 0, RetryBackoffSecs: 1,
	}
	cfg.Browser.Enabled = false
	return cfg
}

func names(t *testing.T, cfg *config.Config) map[string]bool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	defer a.Close()
	set := map[string]bool{}
	for _, n := range a.Registry.Names() {
		set[n] = true
	}
	return set
}

func newTestApp(t *testing.T, cfg *config.Config) *App {
	t.Helper()
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

// The default arsenal is a product promise, not an implementation detail: a
// fresh install must come with a shell, files, memory, jobs, and the desktop.
func TestDefaultToolArsenal(t *testing.T) {
	cfg := testConfig(t)
	on := true
	cfg.Desktop.Enabled = &on // this machine may be headless
	got := names(t, cfg)

	want := []string{
		// shell and files
		"exec", "read_file", "write_file", "edit_file", "list_dir",
		// web
		"web_fetch", "web_search",
		// memory
		"remember", "recall", "forget", "reflect", "memory_status",
		// background work and scheduling
		"job_start", "job_status", "job_list", "job_cancel", "cron",
		// self-management
		"config_get", "config_set", "pkg_install", "upgrade",
		"skill_install", "skill_write", "skill_remove", "skill_find",
		"mcp_add", "mcp_remove", "mcp_list",
		// desktop control
		"window_list", "window_control", "screenshot", "mouse", "type_text",
		"press_key", "clipboard", "notify", "open", "desktop_info",
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("tool %q is not registered by default", name)
		}
	}
}

// Every schema in this test is prompt text the model reads before it can call
// anything. A property without a description, or an enum it cannot see, is a
// tool call the model has to guess at — so the whole arsenal is held to the
// contract, not just the built-ins that internal/tools can reach.
func TestEveryToolSchemaIsModelReady(t *testing.T) {
	cfg := testConfig(t)
	on := true
	cfg.Desktop.Enabled = &on
	a := newTestApp(t, cfg)

	for _, def := range a.Registry.Definitions() {
		t.Run(def.Name, func(t *testing.T) {
			if len(strings.TrimSpace(def.Description)) < 20 {
				t.Errorf("description %q is too thin to route on", def.Description)
			}
			props, _ := def.Parameters["properties"].(map[string]any)
			for key, raw := range props {
				spec, ok := raw.(map[string]any)
				if !ok {
					t.Errorf("property %q spec is %T, want map[string]any", key, raw)
					continue
				}
				if desc, _ := spec["description"].(string); strings.TrimSpace(desc) == "" {
					t.Errorf("property %q has no description; the model must guess what to pass", key)
				}
				if _, declared := spec["enum"]; declared && len(tools.SchemaStrings(spec["enum"])) == 0 {
					t.Errorf("property %q declares an enum with no usable string values", key)
				}
			}
		})
	}
}

func TestNewWiresEveryComponent(t *testing.T) {
	cfg := testConfig(t)
	a := newTestApp(t, cfg)

	if a.Loop == nil || a.Bus == nil || a.Chain == nil || a.Memory == nil ||
		a.Ambient == nil || a.Registry == nil || a.Sessions == nil ||
		a.Skills == nil || a.Jobs == nil || a.Cron == nil || a.MCP == nil {
		t.Fatalf("New left a component nil: %+v", a)
	}

	// the workspace layout exists
	for _, name := range []string{"AGENT.md", "SOUL.md", "USER.md", "HEARTBEAT.md", "sessions", "skills", "cron", "instructions"} {
		if _, err := os.Stat(filepath.Join(cfg.Agent.Workspace, name)); err != nil {
			t.Errorf("workspace missing %s: %v", name, err)
		}
	}

	// every registered tool exposes a usable schema to the model
	for _, def := range a.Registry.Definitions() {
		if def.Description == "" {
			t.Errorf("tool %q has no description", def.Name)
		}
		if def.Parameters["type"] != "object" {
			t.Errorf("tool %q schema type = %v", def.Name, def.Parameters["type"])
		}
	}
}

func TestDesktopToolsFollowConfig(t *testing.T) {
	off := false
	cfg := testConfig(t)
	cfg.Desktop.Enabled = &off
	if names(t, cfg)["window_list"] {
		t.Error("desktop.enabled=false still registered the desktop tools")
	}

	// A headless machine (no DISPLAY) leaves them out on the auto setting.
	cfg = testConfig(t)
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	if names(t, cfg)["screenshot"] {
		t.Error("desktop tools registered without a graphical session")
	}
}

func TestDisabledToolsAreHonoured(t *testing.T) {
	cfg := testConfig(t)
	on := true
	cfg.Desktop.Enabled = &on
	cfg.Tools.Disabled = []string{"exec", "screenshot"}
	got := names(t, cfg)
	if got["exec"] || got["screenshot"] {
		t.Errorf("tools.disabled was ignored: %v", got)
	}
	if !got["window_list"] {
		t.Error("disabling one desktop tool removed the rest")
	}
	if _, ok := newTestApp(t, cfg).Registry.Get("read_file"); !ok {
		t.Error("disabling some tools removed unrelated ones")
	}
}

func TestNewFailsOnBadConfiguration(t *testing.T) {
	cases := map[string]func(*config.Config){
		"unknown memory mode":    func(c *config.Config) { c.Memory.Mode = "telepathy" },
		"provider with no model": func(c *config.Config) { c.Provider.Model = "" },
		"invalid deny pattern":   func(c *config.Config) { c.Tools.CustomDenyPatterns = []string{"([unclosed"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			mutate(cfg)
			a, err := New(context.Background(), cfg)
			if err == nil {
				a.Close()
				t.Fatalf("New accepted a config with %s", name)
			}
		})
	}
}

func TestNewFailsWhenWorkspaceCannotBeCreated(t *testing.T) {
	cfg := testConfig(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("file, not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Agent.Workspace = filepath.Join(blocker, "workspace")
	if a, err := New(context.Background(), cfg); err == nil {
		a.Close()
		t.Error("New succeeded with an unusable workspace path")
	}
}

func TestFinishedJobNotifiesItsOriginatingSession(t *testing.T) {
	cfg := testConfig(t)
	a := newTestApp(t, cfg)

	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{
		Channel: "telegram", ChatID: "4242", SessionKey: "telegram:4242",
	})
	res := a.Registry.Execute(ctx, "job_start", map[string]any{
		"kind":        "exec",
		"description": "quick check",
		"payload":     "echo background-work-done",
	})
	if res.IsError {
		t.Fatalf("job_start = %+v", res)
	}

	select {
	case msg := <-a.Bus.Inbound():
		if msg.Channel != "telegram" || msg.ChatID != "4242" {
			t.Errorf("notification routed to %s:%s", msg.Channel, msg.ChatID)
		}
		for _, want := range []string{"[system]", "quick check", "done", "background-work-done"} {
			if !strings.Contains(msg.Content, want) {
				t.Errorf("notification missing %q:\n%s", want, msg.Content)
			}
		}
	case <-time.After(15 * time.Second):
		t.Fatal("finished job never notified its session")
	}
}

func TestTaskJobsDelegateThroughTheAgentLoop(t *testing.T) {
	cfg := testConfig(t)
	a := newTestApp(t, cfg)

	// kind=task requires a TaskRunner; New must have supplied one.
	job, err := a.Jobs.Start(jobs.KindTask, "delegated", "do something", jobs.Origin{
		Channel: "cli", ChatID: "main", SessionKey: "cli:main",
	})
	if err != nil {
		t.Fatalf("task job rejected — New did not wire a TaskRunner: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if job.Snapshot().State != jobs.StateRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The provider is unreachable in tests, so the delegated turn fails —
	// what matters is that it ran and reported instead of hanging.
	if state := job.Snapshot().State; state == jobs.StateRunning {
		t.Errorf("delegated task never finished (state %s)", state)
	}
}

func TestCronJobsCaptureTheirOrigin(t *testing.T) {
	cfg := testConfig(t)
	a := newTestApp(t, cfg)

	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{
		Channel: "telegram", ChatID: "77", SessionKey: "telegram:77",
	})
	res := a.Registry.Execute(ctx, "cron", map[string]any{
		"action": "add", "schedule": "0 9 * * *", "message": "morning brief",
	})
	if res.IsError {
		t.Fatalf("cron add = %+v", res)
	}
	list := a.Cron.List()
	if len(list) != 1 || list[0].Channel != "telegram" || list[0].ChatID != "77" {
		t.Fatalf("cron job did not capture its origin: %+v", list)
	}
}

func TestCronTargetFallback(t *testing.T) {
	noExternal := func() (string, string, bool) { return "", "", false }
	telegram := func() (string, string, bool) { return "telegram", "77", true }

	// external origins are always honored as-is
	if ch, chat := cronTarget("telegram", "42", noExternal); ch != "telegram" || chat != "42" {
		t.Errorf("external origin rewritten: %s:%s", ch, chat)
	}
	// a cli-origin job with no external channel seen keeps its target
	if ch, chat := cronTarget("cli", "main", noExternal); ch != "cli" || chat != "main" {
		t.Errorf("cli origin without a fallback = %s:%s", ch, chat)
	}
	// once the user has chatted elsewhere, cli-origin results follow them
	if ch, chat := cronTarget("cli", "main", telegram); ch != "telegram" || chat != "77" {
		t.Errorf("cli origin not rerouted: %s:%s", ch, chat)
	}
	// a job scheduled during a heartbeat, or by another cron job, belongs to
	// no chat either: it follows the user the same way
	for _, origin := range []string{"system", "cron"} {
		if ch, chat := cronTarget(origin, "heartbeat", telegram); ch != "telegram" || chat != "77" {
			t.Errorf("%s origin not rerouted: %s:%s", origin, ch, chat)
		}
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	cfg := testConfig(t)
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	a.Close() // must not panic or hang
}
