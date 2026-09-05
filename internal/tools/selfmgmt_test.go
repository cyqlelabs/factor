package tools

import (
	"context"
	"fmt"
	"net"
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
	if err := cfg.Save(); err != nil { // config_get reads the file
		t.Fatal(err)
	}
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

// The proxy carries every provider call, so config_set will not point it at
// an address nothing answers at, and will not move it at all for a heartbeat.
func TestConfigSetRefusesAProxyThatWillNotCarryTraffic(t *testing.T) {
	cfg := testConfig(t)
	set := NewConfigTools(cfg)[1]
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()

	res := set.Execute(context.Background(), map[string]any{"key": "proxy.address", "value": closed})
	if !res.IsError || !strings.Contains(res.ForLLM, "nothing answered") {
		t.Errorf("a port nothing listens on was accepted: %+v", res)
	}
	res = set.Execute(context.Background(), map[string]any{"key": "proxy.address", "value": `"none"`})
	if !res.IsError {
		t.Error("a quoted word was accepted as a proxy address")
	}
	res = set.Execute(context.Background(), map[string]any{"key": "proxy", "value": map[string]any{"address": closed}})
	if !res.IsError {
		t.Error("the whole section was a way around the check")
	}
	reloaded, err := config.Load(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Proxy.Address != "" {
		t.Errorf("a refused address was saved: %q", reloaded.Proxy.Address)
	}

	heartbeat := WithToolContext(context.Background(), ToolContext{Channel: "system", ChatID: "heartbeat", SessionKey: "system:heartbeat"})
	res = set.Execute(heartbeat, map[string]any{"key": "proxy.address", "value": "127.0.0.1:8080"})
	if !res.IsError || !strings.Contains(res.ForLLM, "heartbeat") {
		t.Errorf("a heartbeat moved the proxy: %+v", res)
	}
	// Turning the proxy off is never refused: there is nothing to probe.
	res = set.Execute(context.Background(), map[string]any{"key": "proxy.address", "value": ""})
	if res.IsError {
		t.Errorf("clearing the proxy was refused: %+v", res)
	}
}

func TestConfigSetPersistsAndValidates(t *testing.T) {
	cfg := testConfig(t)
	set := NewConfigTools(cfg)[1]

	res := set.Execute(context.Background(), map[string]any{"key": "heartbeat.interval_minutes", "value": 15.0})
	if res.IsError {
		t.Fatalf("set = %+v", res)
	}
	// the live config is immutable; the FILE carries the change
	if cfg.Heartbeat.IntervalMinutes == 15 {
		t.Error("config_set must not mutate the live config")
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
	// list values work and persist
	res = set.Execute(context.Background(), map[string]any{"key": "tools.disabled", "value": []any{"exec"}})
	if res.IsError {
		t.Fatalf("list set failed: %+v", res)
	}
	reloaded, err = config.Load(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Tools.IsToolEnabled("exec") {
		t.Error("list value not persisted")
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
	// Puppy's `pkg install` only unpacks an already-downloaded file, and it
	// reads a trailing flag as a package name — so both the verb and the
	// order are load-bearing.
	if strings.Join(captured[0], " ") != "pkg -f get nano" {
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
