package gateway

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fastConfirm shortens the startup-confirmation window so a failing child is
// reported in test time.
func fastConfirm(t *testing.T) {
	t.Helper()
	oldTimeout, oldPoll := confirmTimeout, confirmPoll
	confirmTimeout, confirmPoll = 2*time.Second, 10*time.Millisecond
	t.Cleanup(func() { confirmTimeout, confirmPoll = oldTimeout, oldPoll })
}

// fakeGateway points Daemonize at this test binary, which TestMain makes
// behave like the named flavour of `factor gateway`.
func fakeGateway(t *testing.T, mode string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FACTOR_TEST_GATEWAY_MODE", mode)
	old := daemonExecutable
	daemonExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { daemonExecutable = old })
}

func TestDaemonizeSpawnsDetachedGateway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	fastConfirm(t)
	fakeGateway(t, "serve")

	pid, err := Daemonize(filepath.Join(home, "config.json"), []string{"-p", "127.0.0.1:9"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killProcess(pid) })
	if !pidAlive(pid) {
		t.Errorf("reported pid %d is not alive", pid)
	}

	// The child got the real gateway invocation, config path included, and
	// its output landed in the log rather than on this terminal.
	log, err := os.ReadFile(LogPath())
	if err != nil {
		t.Fatal(err)
	}
	// The child got the real gateway invocation with the config path and the
	// flags that configured this process: a detached gateway that quietly
	// drops the proxy is a capture that records nothing.
	want := "args: gateway -c " + filepath.Join(home, "config.json") + " -p 127.0.0.1:9"
	if !strings.Contains(string(log), want) {
		t.Errorf("log %q is missing %q", log, want)
	}
}

func TestDaemonizeRefusesWhileGatewayRuns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	if err := writePidFile(); err != nil { // pretend a gateway is already up
		t.Fatal(err)
	}
	old := daemonExecutable
	daemonExecutable = func() (string, error) {
		t.Error("spawned a child despite the running gateway")
		return "", errors.New("no")
	}
	t.Cleanup(func() { daemonExecutable = old })

	_, err := Daemonize("", nil)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("Daemonize = %v, want an already-running refusal", err)
	}
}

func TestDaemonizeReportsChildDeathFromTheLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	fastConfirm(t)
	fakeGateway(t, "die")

	_, err := Daemonize("", nil)
	if err == nil || !strings.Contains(err.Error(), "factor: boom") {
		t.Errorf("Daemonize = %v, want the child's last words", err)
	}
}

func TestDaemonizeTimesOutOnSilentChild(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	fastConfirm(t)
	fakeGateway(t, "silent")

	_, err := Daemonize("", nil)
	if err == nil || !strings.Contains(err.Error(), LogPath()) {
		t.Errorf("Daemonize = %v, want a timeout naming the log", err)
	}
	// The child never became a gateway; do not leave it sleeping around.
	if data, err := os.ReadFile(filepath.Join(home, "child.pid")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			killProcess(pid)
		}
	}
}

func TestLastLogLineFallsBackToThePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)

	if got := lastLogLine(); !strings.Contains(got, LogPath()) {
		t.Errorf("no log file: lastLogLine() = %q, want the path", got)
	}
	if err := os.WriteFile(LogPath(), []byte("\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastLogLine(); !strings.Contains(got, LogPath()) {
		t.Errorf("blank log: lastLogLine() = %q, want the path", got)
	}
	if err := os.WriteFile(LogPath(), []byte("first\nlast words\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastLogLine(); got != "last words" {
		t.Errorf("lastLogLine() = %q, want %q", got, "last words")
	}
}
