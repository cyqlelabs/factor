package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FACTOR_HOME", dir)
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestConfigGetRedactsSecrets(t *testing.T) {
	cfg := testConfig(t)
	cfg.Provider.APIKey = "sk-very-secret"
	get := NewConfigTools(cfg)[0]

	res := get.Execute(context.Background(), map[string]any{"key": "provider"})
	if res.IsError || strings.Contains(res.ForLLM, "sk-very-secret") {
		t.Fatalf("secret leaked or error: %+v", res)
	}
	if !strings.Contains(res.ForLLM, "[redacted]") {
		t.Errorf("expected redaction marker: %s", res.ForLLM)
	}

	res = get.Execute(context.Background(), map[string]any{"key": "provider.model"})
	if res.IsError || !strings.Contains(res.ForLLM, cfg.Provider.Model) {
		t.Errorf("scalar get = %+v", res)
	}
	res = get.Execute(context.Background(), map[string]any{"key": "no.such.key"})
	if !res.IsError {
		t.Error("missing key accepted")
	}
}

func TestConfigSetPersistsAndValidates(t *testing.T) {
	cfg := testConfig(t)
	set := NewConfigTools(cfg)[1]

	res := set.Execute(context.Background(), map[string]any{"key": "heartbeat.interval_minutes", "value": 15.0})
	if res.IsError {
		t.Fatalf("set = %+v", res)
	}
	if cfg.Heartbeat.IntervalMinutes != 15 {
		t.Errorf("in-memory value = %d", cfg.Heartbeat.IntervalMinutes)
	}
	reloaded, err := config.Load(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Heartbeat.IntervalMinutes != 15 {
		t.Errorf("persisted value = %d", reloaded.Heartbeat.IntervalMinutes)
	}

	// schema violations rejected
	res = set.Execute(context.Background(), map[string]any{"key": "heartbeat.interval_minutes", "value": "not-a-number"})
	if !res.IsError {
		t.Error("type-invalid set accepted")
	}
	// list values work
	res = set.Execute(context.Background(), map[string]any{"key": "tools.disabled", "value": []any{"exec"}})
	if res.IsError || cfg.Tools.IsToolEnabled("exec") {
		t.Errorf("list set failed: %+v", res)
	}
}

func TestPkgInstallBuildsCommands(t *testing.T) {
	var captured [][]string
	tool := &PkgInstallTool{
		lookPath: func(bin string) (string, error) {
			if bin == "apt-get" || bin == "pip" || bin == "sudo" {
				return "/usr/bin/" + bin, nil
			}
			return "", fmt.Errorf("not found")
		},
		euid: func() int { return 1000 },
		runner: func(_ context.Context, argv []string) (string, error) {
			captured = append(captured, argv)
			return "ok", nil
		},
	}

	res := tool.Execute(context.Background(), map[string]any{"packages": []any{"htop", "jq"}})
	if res.IsError {
		t.Fatalf("auto install = %+v", res)
	}
	want := []string{"sudo", "-n", "apt-get", "install", "-y", "htop", "jq"}
	if strings.Join(captured[0], " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v", captured[0])
	}

	res = tool.Execute(context.Background(), map[string]any{"packages": []any{"smrti"}, "manager": "pip"})
	if res.IsError {
		t.Fatalf("pip install = %+v", res)
	}
	if strings.Join(captured[1], " ") != "pip install smrti" {
		t.Errorf("pip argv = %v (no sudo for language managers)", captured[1])
	}
}

func TestPkgInstallAsRootSkipsSudo(t *testing.T) {
	var captured [][]string
	tool := &PkgInstallTool{
		lookPath: func(bin string) (string, error) { return "/bin/" + bin, nil },
		euid:     func() int { return 0 },
		runner: func(_ context.Context, argv []string) (string, error) {
			captured = append(captured, argv)
			return "", nil
		},
	}
	res := tool.Execute(context.Background(), map[string]any{"packages": []any{"nano"}, "manager": "pkg"})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	if captured[0][0] != "pkg" {
		t.Errorf("root install argv = %v", captured[0])
	}
}

func TestPkgInstallRejectsHostileNames(t *testing.T) {
	tool := NewPkgInstallTool()
	for _, bad := range []string{"", "-rf", "a b", "x;rm", "y|z", "$(boom)"} {
		res := tool.Execute(context.Background(), map[string]any{"packages": []any{bad}})
		if !res.IsError || !strings.Contains(res.ForLLM, "invalid package name") {
			t.Errorf("hostile name %q accepted: %+v", bad, res)
		}
	}
}
