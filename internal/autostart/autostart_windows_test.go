//go:build windows

package autostart

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// scratchRunKey points the Run-key writes at a throwaway subkey, so a test run
// never disturbs the login entry the user actually has installed.
func scratchRunKey(t *testing.T) {
	t.Helper()
	const scratch = `Software\Factor\test\Run`
	key, _, err := registry.CreateKey(registry.CURRENT_USER, scratch, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	key.Close()
	prev := runKey
	runKey = scratch
	t.Cleanup(func() {
		runKey = prev
		_ = registry.DeleteKey(registry.CURRENT_USER, scratch)
	})
}

func TestWindowsInstallRoundTripsThroughTheRunKey(t *testing.T) {
	scratchRunKey(t)
	env, _, _ := fakeEnv(t, "windows", "")
	const exe = `C:\Program Files\Factor\factor.exe`
	const cfg = `C:\Users\nico\.factor\config.json`

	entry, err := Install(context.Background(), env, exe, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Mechanism != "registry Run key" {
		t.Errorf("mechanism = %q", entry.Mechanism)
	}

	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Close()
	value, _, err := key.GetStringValue("Factor")
	if err != nil {
		t.Fatal(err)
	}
	// The entry has to name a path Windows can actually open. Rendering it
	// with %q doubles every separator, and the login entry then points at a
	// binary that does not exist — an autostart that silently never starts.
	if strings.Contains(value, `\\`) {
		t.Errorf("Run value has escaped separators: %s", value)
	}
	for _, want := range []string{`"` + exe + `"`, "gateway", "-d", `-c "` + cfg + `"`} {
		if !strings.Contains(value, want) {
			t.Errorf("Run value %q is missing %q", value, want)
		}
	}

	if got, ok := Installed(env); !ok || got.Mechanism != "registry Run key" {
		t.Errorf("Installed = (%+v, %v) after install", got, ok)
	}
	if err := Uninstall(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if _, ok := Installed(env); ok {
		t.Error("the Run value survived the uninstall")
	}
}
