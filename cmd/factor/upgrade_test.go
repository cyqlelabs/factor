package main

import (
	"context"
	"errors"
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
