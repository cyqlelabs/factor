package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
)

// testConfig builds a self-contained app config: memory off (no sidecar to
// spawn), no MCP servers, everything under a temp home.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	cfg := config.Default()
	cfg.Agent.Workspace = filepath.Join(home, "workspace")
	cfg.Memory.Mode = "off"
	cfg.Memory.DBPath = filepath.Join(home, "memory.db")
	cfg.Provider = config.ProviderConfig{Type: "ollama", Model: "test", MaxTokens: 512}
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
		"config_get", "config_set", "pkg_install", "skill_install",
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
}
