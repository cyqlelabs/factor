package upgrade

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/memory"
)

// noDocker takes the container half off the table, which is what a machine
// running smrti as a package looks like.
func noDocker(t *testing.T) {
	t.Helper()
	prev := dockerLook
	dockerLook = func() error { return fmt.Errorf("not found") }
	t.Cleanup(func() { dockerLook = prev })
}

// noInstall reports no smrti on disk.
func noInstall(t *testing.T) {
	t.Helper()
	prev := findSmrti
	findSmrti = func(string, string) (string, bool) { return "", false }
	t.Cleanup(func() { findSmrti = prev })
}

// installed reports an smrti on disk at the given version.
func installed(t *testing.T, path, version string) {
	t.Helper()
	prevFind, prevVer := findSmrti, installedVersion
	findSmrti = func(string, string) (string, bool) { return path, true }
	installedVersion = func(context.Context, string) string { return version }
	t.Cleanup(func() { findSmrti, installedVersion = prevFind, prevVer })
}

// fakePyPI publishes one version of the smrti package.
func fakePyPI(t *testing.T, version string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pypi/"+memory.PackageName+"/json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"info":{"version":%q}}`, version)
	}))
	prev := pypiBase
	pypiBase = srv.URL
	t.Cleanup(func() { pypiBase = prev; srv.Close() })
}

// fakeInstaller stands in for uv/pipx/pip and records what it was asked to
// upgrade. stopped is the pid the engine was running as, or 0 when there was
// nothing to stop; a supervisor then spawns the replacement under a new pid.
func fakeInstaller(t *testing.T, stopped int, supervised bool, err error) *[]string {
	t.Helper()
	prevUpgrade, prevStop, prevPid := upgradePackage, stopEngine, enginePid
	prevWait := smrtiRespawnWait
	smrtiRespawnWait = 60 * time.Millisecond
	var calls []string
	upgradePackage = func(_ context.Context, exe, _ string, _ memory.Progress) (string, error) {
		calls = append(calls, "upgrade "+exe)
		return "pipx", err
	}
	stopEngine = func(context.Context) (int, error) {
		calls = append(calls, "stop")
		return stopped, nil
	}
	enginePid = func() (int, bool) {
		if !supervised {
			return 0, false
		}
		return stopped + 1, true
	}
	t.Cleanup(func() {
		upgradePackage, stopEngine, enginePid = prevUpgrade, prevStop, prevPid
		smrtiRespawnWait = prevWait
	})
	return &calls
}

// versionedEngine answers /status with a version, the way a live smrti does.
func versionedEngine(t *testing.T, version string) config.MemoryConfig {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"spaces":{},"space":"main","version":%q}`, version)
	}))
	t.Cleanup(srv.Close)
	return config.MemoryConfig{Mode: "sidecar", URL: srv.URL, Host: "127.0.0.1", Port: 8420}
}

func TestSmrtiCheckFallsBackToThePackageInstall(t *testing.T) {
	noDocker(t)
	installed(t, "/home/u/.local/bin/smrti", "0.11.3")
	fakePyPI(t, "0.13.0")

	rel, err := NewSmrti(engineConfig(t, true), nil).Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Mode != ModePackage || rel.Running != "0.11.3" || rel.Version != "0.13.0" {
		t.Fatalf("release = %+v", rel)
	}
	if rel.Path != "/home/u/.local/bin/smrti" || !rel.Newer() {
		t.Fatalf("release = %+v", rel)
	}
	if rel.Source() != "published release" {
		t.Errorf("source = %q", rel.Source())
	}
}

func TestSmrtiCheckAsksTheRunningEngineForItsVersion(t *testing.T) {
	noDocker(t)
	// The install on disk is newer than the engine that is actually serving:
	// an upgrade that was installed but never restarted into. What is running
	// is what the check must measure.
	installed(t, "/home/u/.local/bin/smrti", "0.13.0")
	fakePyPI(t, "0.13.0")

	rel, err := NewSmrti(versionedEngine(t, "0.11.3"), nil).Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Running != "0.11.3" || !rel.Newer() {
		t.Fatalf("release = %+v", rel)
	}
}

func TestSmrtiCheckReadsAnUnversionedInstallAsOlder(t *testing.T) {
	noDocker(t)
	installed(t, "/home/u/.local/bin/smrti", "")
	fakePyPI(t, "0.13.0")

	rel, err := NewSmrti(engineConfig(t, true), nil).Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rel.Newer() {
		t.Fatal("an install nothing can read a version from must be offered the published one")
	}
	if rel.RunningVersion() != "an unreadable version" {
		t.Errorf("running = %q", rel.RunningVersion())
	}
}

func TestSmrtiCheckLeavesAnEngineOnAnotherMachineAlone(t *testing.T) {
	noDocker(t)
	installed(t, "/home/u/.local/bin/smrti", "0.11.3")
	fakePyPI(t, "0.13.0")

	cfg := config.MemoryConfig{Mode: "external", URL: "http://memory.box:8420", Port: 8420}
	_, err := NewSmrti(cfg, nil).Check(context.Background())
	if !errors.Is(err, ErrNotManaged) || !strings.Contains(err.Error(), "memory.box") {
		t.Fatalf("error = %v", err)
	}
}

func TestSmrtiApplyUpgradesThePackageAndRestartsTheEngine(t *testing.T) {
	quickPacing(t)
	noDocker(t)
	installed(t, "/home/u/.local/bin/smrti", "0.11.3")
	fakePyPI(t, "0.13.0")
	calls := fakeInstaller(t, 4242, true, nil)

	s := NewSmrti(engineConfig(t, true), nil)
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	note, err := s.Apply(context.Background(), rel, func(f string, a ...any) {
		steps = append(steps, fmt.Sprintf(f, a...))
	})
	if err != nil {
		t.Fatal(err)
	}
	if note != "the engine restarted on it" {
		t.Errorf("note = %q", note)
	}
	line := strings.Join(*calls, " | ")
	if line != "upgrade /home/u/.local/bin/smrti | stop" {
		t.Fatalf("calls = %q", line)
	}
	if !strings.Contains(strings.Join(steps, " "), "installed smrti 0.13.0 with pipx") {
		t.Errorf("progress = %v", steps)
	}
}

func TestSmrtiApplyLeavesNewCodeForTheNextStart(t *testing.T) {
	quickPacing(t)
	noDocker(t)
	installed(t, "/home/u/.local/bin/smrti", "0.11.3")
	fakePyPI(t, "0.13.0")
	// Nothing to stop: the engine is one Factor never spawned, or is down.
	fakeInstaller(t, 0, false, nil)

	s := NewSmrti(engineConfig(t, false), nil)
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	note, err := s.Apply(context.Background(), rel, nil)
	if err != nil {
		t.Fatal(err)
	}
	if note != "it loads the next time the engine starts" {
		t.Errorf("note = %q", note)
	}
}

func TestSmrtiApplyPackageWaitsForAQuietGraph(t *testing.T) {
	quickPacing(t)
	noDocker(t)
	installed(t, "/home/u/.local/bin/smrti", "0.11.3")
	fakePyPI(t, "0.13.0")
	calls := fakeInstaller(t, 4242, true, nil)

	s := NewSmrti(engineConfig(t, true), func() bool { return false })
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(context.Background(), rel, nil); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(strings.Join(*calls, " "), "stop") {
		t.Errorf("a busy engine was stopped anyway: %v", *calls)
	}
}

func TestSmrtiApplyPackageReportsAnEngineThatDoesNotComeBack(t *testing.T) {
	quickPacing(t)
	noDocker(t)
	installed(t, "/home/u/.local/bin/smrti", "0.11.3")
	fakePyPI(t, "0.13.0")
	fakeInstaller(t, 4242, true, nil)

	// Restarted by its supervisor, but the new engine never answers.
	s := NewSmrti(engineConfig(t, false), nil)
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(context.Background(), rel, nil)
	if err == nil || !strings.Contains(err.Error(), "is installed") || !strings.Contains(err.Error(), "not answering") {
		t.Fatalf("the install has to be reported even when the restart fails: %v", err)
	}
}

func TestSmrtiApplyPackageWithNothingToRestartTheEngine(t *testing.T) {
	quickPacing(t)
	noDocker(t)
	installed(t, "/home/u/.local/bin/smrti", "0.11.3")
	fakePyPI(t, "0.13.0")
	// A warm engine from an earlier session, and no daemon to bring it back:
	// stopping it is the upgrade, not a failure of one.
	fakeInstaller(t, 4242, false, nil)

	s := NewSmrti(engineConfig(t, false), nil)
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	note, err := s.Apply(context.Background(), rel, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "starts with the next factor run") {
		t.Errorf("note = %q", note)
	}
}

func TestSmrtiApplyPackageReportsAFailedInstall(t *testing.T) {
	quickPacing(t)
	noDocker(t)
	installed(t, "/home/u/.local/bin/smrti", "0.11.3")
	fakePyPI(t, "0.13.0")
	calls := fakeInstaller(t, 4242, true, fmt.Errorf("pipx upgrade smrti: exit status 1"))

	s := NewSmrti(engineConfig(t, true), nil)
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(context.Background(), rel, nil); err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(strings.Join(*calls, " "), "stop") {
		t.Errorf("the engine was restarted after an install that failed: %v", *calls)
	}
}

func TestSmrtiUpdateReportsThePackageHalf(t *testing.T) {
	quickPacing(t)
	noDocker(t)
	installed(t, "/home/u/.local/bin/smrti", "0.11.3")
	fakePyPI(t, "0.13.0")
	fakeInstaller(t, 4242, true, nil)

	s := NewSmrti(engineConfig(t, true), nil)
	var said []string
	out := func(f string, a ...any) { said = append(said, fmt.Sprintf(f, a...)) }

	if err := s.Update(context.Background(), true, out); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(context.Background(), false, out); err != nil {
		t.Fatal(err)
	}
	line := strings.Join(said, " | ")
	for _, want := range []string{
		"smrti 0.13.0 is available — the engine here runs 0.11.3.",
		"upgraded smrti 0.11.3 to 0.13.0 — the engine restarted on it",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("said %q, missing %q", line, want)
		}
	}
}

func TestSmrtiUpdateSaysWhenAPackageIsCurrent(t *testing.T) {
	noDocker(t)
	installed(t, "/home/u/.local/bin/smrti", "0.13.0")
	fakePyPI(t, "0.13.0")

	var said []string
	err := NewSmrti(engineConfig(t, true), nil).Update(context.Background(), false,
		func(f string, a ...any) { said = append(said, fmt.Sprintf(f, a...)) })
	if err != nil {
		t.Fatal(err)
	}
	if len(said) != 1 || said[0] != "smrti 0.13.0 is the newest published release." {
		t.Fatalf("said = %v", said)
	}
}

func TestLatestSmrtiPackageRejectsAnUnversionedRelease(t *testing.T) {
	fakePyPI(t, "nightly")
	if _, err := latestSmrtiPackage(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "no versioned smrti release") {
		t.Fatalf("error = %v", err)
	}

	prev := pypiBase
	pypiBase = "http://127.0.0.1:1" // nothing listens there
	defer func() { pypiBase = prev }()
	if _, err := latestSmrtiPackage(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "looking up the published smrti releases") {
		t.Fatalf("error = %v", err)
	}
}

func TestLocalEngineReadsTheConfiguredEndpoint(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:8420", true},
		{"http://localhost:8420", true},
		{"http://0.0.0.0:8420", true},
		{"http://[::1]:8420", true},
		{"http://192.168.0.106:8420", false},
		{"http://memory.box:8420", false},
	} {
		s := NewSmrti(config.MemoryConfig{URL: tc.url}, nil)
		if got := s.localEngine(); got != tc.want {
			t.Errorf("localEngine(%q) = %v", tc.url, got)
		}
	}
}

func TestSmrtiApplyPackageReportsAnEngineItCannotStop(t *testing.T) {
	quickPacing(t)
	noDocker(t)
	installed(t, "/home/u/.local/bin/smrti", "0.11.3")
	fakePyPI(t, "0.13.0")
	fakeInstaller(t, 4242, true, nil)
	prev := stopEngine
	stopEngine = func(context.Context) (int, error) { return 0, fmt.Errorf("operation not permitted") }
	defer func() { stopEngine = prev }()

	s := NewSmrti(engineConfig(t, true), nil)
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(context.Background(), rel, nil)
	if err == nil || !strings.Contains(err.Error(), "0.13.0 is installed") ||
		!strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("the install has to be reported alongside the failure: %v", err)
	}
}

func TestSmrtiCheckReportsAPyPIItCannotReach(t *testing.T) {
	noDocker(t)
	installed(t, "/home/u/.local/bin/smrti", "0.11.3")
	prev := pypiBase
	pypiBase = "http://127.0.0.1:1"
	defer func() { pypiBase = prev }()

	_, err := NewSmrti(engineConfig(t, true), nil).Check(context.Background())
	if err == nil || errors.Is(err, ErrNotManaged) {
		t.Fatalf("a lookup that failed is not an engine that cannot be upgraded: %v", err)
	}
}
