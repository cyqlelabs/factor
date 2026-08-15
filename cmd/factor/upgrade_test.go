package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/upgrade"
	"github.com/cyqlelabs/factor/internal/version"
)

// stubUpgrade replaces the release seams and the running version for one test.
func stubUpgrade(t *testing.T, running string, latest func(context.Context) (upgrade.Release, error),
	apply func(context.Context, upgrade.Release, upgrade.Progress) (string, error)) {
	t.Helper()
	prevVersion, prevLatest, prevApply := version.Version, latestRelease, applyRelease
	version.Version, latestRelease, applyRelease = running, latest, apply
	t.Cleanup(func() { version.Version, latestRelease, applyRelease = prevVersion, prevLatest, prevApply })
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

	out, err := captureStdout(t, func() error { return runUpgrade(false) })
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

	out, err := captureStdout(t, func() error { return runUpgrade(true) })
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

	out, err := captureStdout(t, func() error { return runUpgrade(false) })
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

	out, err := captureStdout(t, func() error { return runUpgrade(false) })
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

	out, err := captureStdout(t, func() error { return runUpgrade(false) })
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

func TestUpgradeReportsFailures(t *testing.T) {
	lookupFailed := errors.New("looking up the latest factor release: 403 rate limited")
	stubUpgrade(t, "v0.3.0",
		func(context.Context) (upgrade.Release, error) { return upgrade.Release{}, lookupFailed },
		nil)
	if _, err := captureStdout(t, func() error { return runUpgrade(false) }); !errors.Is(err, lookupFailed) {
		t.Fatalf("err = %v", err)
	}

	installFailed := errors.New("permission denied")
	stubUpgrade(t, "v0.3.0", published(upgrade.Release{Version: "v0.4.0"}),
		func(context.Context, upgrade.Release, upgrade.Progress) (string, error) { return "", installFailed })
	if _, err := captureStdout(t, func() error { return runUpgrade(false) }); !errors.Is(err, installFailed) {
		t.Fatalf("err = %v", err)
	}
}
