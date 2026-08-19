//go:build linux || windows

package tray

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCanTrayNeedsASessionBus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows always runs a shell")
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if canTray() {
		t.Error("no bus anywhere, yet canTray said yes")
	}

	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if canTray() {
		t.Error("an empty runtime dir has no bus to speak to")
	}

	runtimeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(runtimeDir, "bus"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	if !canTray() {
		t.Error("a runtime-dir bus socket was not accepted")
	}

	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/0/bus")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if !canTray() {
		t.Error("an advertised session bus was not accepted")
	}
}

func TestRunWithoutASessionReturnsAtOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows always shows a tray")
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("XDG_RUNTIME_DIR", "")

	done := make(chan struct{})
	go func() {
		Run("test", func() {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run blocked with no session to show a tray in")
	}
}

func TestQuitEndsRun(t *testing.T) {
	if runtime.GOOS != "windows" {
		// A bus address nothing answers on: systray's loop comes up, fails to
		// register, and idles — exactly the state Quit must be able to end.
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/factor-test-bus")
	}

	done := make(chan struct{})
	go func() {
		Run("test", func() {})
		close(done)
	}()
	time.Sleep(200 * time.Millisecond) // let the loop come up before ending it
	Quit()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Quit")
	}
}
