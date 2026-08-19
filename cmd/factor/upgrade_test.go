package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/upgrade"
	"github.com/cyqlelabs/factor/internal/version"
)

// stubUpgrade replaces the release seams and the running version for one test.
// The engine half is stubbed out with them: what docker is running on the
// machine under test is none of this test's business.
func stubUpgrade(t *testing.T, running string, latest func(context.Context) (upgrade.Release, error),
	apply func(context.Context, upgrade.Release, upgrade.Progress) (string, error)) {
	t.Helper()
	// Neither is a live gateway on the machine running the tests: the pid
	// lookup is pointed at an empty home, and a restart signal that escapes
	// anyway fails the test. TestUpgradeInstalls once left both unguarded,
	// and every test-suite run beside a running gateway SIGHUPed it into a
	// restart. A test that wants the signal path overlays stubGateway.
	testHome(t)
	prevRestart := restartGateway
	restartGateway = func(pid int) error {
		t.Errorf("no gateway holds this home's pid file, yet pid %d was signalled", pid)
		return nil
	}
	prevVersion, prevLatest, prevApply, prevEngine := version.Version, latestRelease, applyRelease, updateEngine
	version.Version, latestRelease, applyRelease = running, latest, apply
	updateEngine = func(context.Context, string, bool) error { return nil }
	t.Cleanup(func() {
		restartGateway = prevRestart
		version.Version, latestRelease, applyRelease, updateEngine = prevVersion, prevLatest, prevApply, prevEngine
	})
}

func published(rel upgrade.Release) func(context.Context) (upgrade.Release, error) {
	return func(context.Context) (upgrade.Release, error) { return rel, nil }
}

func TestUpgradeSaysNothingToDo(t *testing.T) {
	stubUpgrade(t, "v0.4.0", published(upgrade.Release{Version: "v0.4.0"}),
		func(context.Context, upgrade.Release, upgrade.Progress) (string, error) {
			t.Error("nothing to install, but Apply ran")
			return "", nil
		})

	out, err := captureStdout(t, func() error { return runUpgrade("", false) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "newest release") {
		t.Errorf("output = %q", out)
	}
}

func TestUpgradeCheckOnlyInstallsNothing(t *testing.T) {
	stubUpgrade(t, "v0.3.0",
		published(upgrade.Release{Version: "v0.4.0", Notes: "https://example.test/v0.4.0"}),
		func(context.Context, upgrade.Release, upgrade.Progress) (string, error) {
			t.Error("--check installed the release")
			return "", nil
		})

	out, err := captureStdout(t, func() error { return runUpgrade("", true) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "v0.4.0 is available") || !strings.Contains(out, "https://example.test/v0.4.0") {
		t.Errorf("output = %q", out)
	}
}

func TestUpgradeInstalls(t *testing.T) {
	stubUpgrade(t, "v0.3.0", published(upgrade.Release{Version: "v0.4.0"}),
		func(_ context.Context, rel upgrade.Release, progress upgrade.Progress) (string, error) {
			progress("downloading factor %s", rel.Version)
			return "/usr/local/bin/factor", nil
		})

	out, err := captureStdout(t, func() error { return runUpgrade("", false) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "downloading factor v0.4.0") {
		t.Errorf("progress not printed: %q", out)
	}
	if !strings.Contains(out, "installed factor v0.4.0 at /usr/local/bin/factor") {
		t.Errorf("output = %q", out)
	}
}

// stubGateway pretends a daemon holds the pid file, and reports what the
// upgrade asked of it.
func stubGateway(t *testing.T, signalErr error) *[]int {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "factor.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	var signalled []int
	prev := restartGateway
	restartGateway = func(pid int) error {
		signalled = append(signalled, pid)
		return signalErr
	}
	t.Cleanup(func() { restartGateway = prev })
	return &signalled
}

func TestUpgradeRestartsTheRunningGateway(t *testing.T) {
	stubUpgrade(t, "v0.3.0", published(upgrade.Release{Version: "v0.4.0"}),
		func(context.Context, upgrade.Release, upgrade.Progress) (string, error) {
			return "/usr/local/bin/factor", nil
		})
	signalled := stubGateway(t, nil)

	out, err := captureStdout(t, func() error { return runUpgrade("", false) })
	if err != nil {
		t.Fatal(err)
	}
	if len(*signalled) != 1 || (*signalled)[0] != os.Getpid() {
		t.Errorf("signalled %v, want the running gateway's pid %d", *signalled, os.Getpid())
	}
	if !strings.Contains(out, "will restart into v0.4.0") {
		t.Errorf("output = %q", out)
	}
}

func TestUpgradeFallsBackWhenTheGatewayCannotBeSignalled(t *testing.T) {
	stubUpgrade(t, "v0.3.0", published(upgrade.Release{Version: "v0.4.0"}),
		func(context.Context, upgrade.Release, upgrade.Progress) (string, error) {
			return "/usr/local/bin/factor", nil
		})
	stubGateway(t, errors.New("operation not permitted"))

	out, err := captureStdout(t, func() error { return runUpgrade("", false) })
	if err != nil {
		t.Fatal(err)
	}
	// The install still succeeded; only the reload has to be done by hand.
	if !strings.Contains(out, "installed factor v0.4.0") || !strings.Contains(out, "restart it to pick this up") {
		t.Errorf("output = %q", out)
	}
	if !strings.Contains(out, "operation not permitted") {
		t.Errorf("output hides why the restart failed: %q", out)
	}
}

func TestUpgradeReportsAnEngineItCannotUpdate(t *testing.T) {
	stubUpgrade(t, "v0.4.0", published(upgrade.Release{Version: "v0.4.0"}), nil)
	updateEngine = func(context.Context, string, bool) error {
		return errors.New("no running container publishes port 8420")
	}
	// The engine's trouble is reported, and the binary is still looked at.
	out, err := captureStdout(t, func() error { return runUpgrade("", false) })
	if err != nil {
		t.Fatalf("an engine that cannot be updated must not fail the upgrade: %v", err)
	}
	if !strings.Contains(out, "newest release") {
		t.Errorf("output = %q", out)
	}
}

func TestUpdateEngineSkipsMemoryThatIsOff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.json"),
		[]byte(`{"memory":{"mode":"off"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := updateEngine(context.Background(), "", false); err != nil {
		t.Fatalf("memory is off; there is no engine to update: %v", err)
	}
}

func TestGatewayMemoryIdleAsksTheRunningDaemon(t *testing.T) {
	body := `{"memory_idle":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Gateway.Host = host
	cfg.Gateway.Port, _ = strconv.Atoi(port)

	// No daemon: nothing else is using the engine, so there is nothing to
	// wait for and no idle check to make.
	t.Setenv("FACTOR_HOME", t.TempDir())
	if gatewayMemoryIdle(cfg) != nil {
		t.Fatal("with no gateway running there is nobody to ask")
	}

	stubGateway(t, nil)
	ask := gatewayMemoryIdle(cfg)
	if ask == nil {
		t.Fatal("a running gateway is asked")
	}
	if !ask() {
		t.Error("the daemon said its engine is idle")
	}
	body = `{"memory_idle":false}`
	if ask() {
		t.Error("the daemon said its engine is busy")
	}
	// A daemon from before this field existed has no opinion, and waiting on
	// an answer it will never give would stall every upgrade.
	body = `{"version":"v0.12.1"}`
	if !ask() {
		t.Error("a daemon that cannot answer must not block the upgrade")
	}

	// A daemon that cannot be reached has not said it is idle.
	srv.Close()
	if ask() {
		t.Error("unconfirmed is not idle")
	}
}

func TestUpgradeReportsFailures(t *testing.T) {
	lookupFailed := errors.New("looking up the latest factor release: 403 rate limited")
	stubUpgrade(t, "v0.3.0",
		func(context.Context) (upgrade.Release, error) { return upgrade.Release{}, lookupFailed },
		nil)
	if _, err := captureStdout(t, func() error { return runUpgrade("", false) }); !errors.Is(err, lookupFailed) {
		t.Fatalf("err = %v", err)
	}

	installFailed := errors.New("permission denied")
	stubUpgrade(t, "v0.3.0", published(upgrade.Release{Version: "v0.4.0"}),
		func(context.Context, upgrade.Release, upgrade.Progress) (string, error) { return "", installFailed })
	if _, err := captureStdout(t, func() error { return runUpgrade("", false) }); !errors.Is(err, installFailed) {
		t.Fatalf("err = %v", err)
	}
}
