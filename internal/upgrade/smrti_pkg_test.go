package upgrade

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	prevWait, prevOrder := smrtiRespawnWait, stopBeforeInstall
	smrtiRespawnWait = 60 * time.Millisecond
	// The order where the install goes first, whatever this machine is; the
	// stop-first order has tests of its own.
	stopBeforeInstall = false
	var calls []string
	upgradePackage = func(_ context.Context, exe, _ string, _ memory.Progress) (string, error) {
		calls = append(calls, "upgrade "+exe)
		return "pipx", err
	}
	stopEngine = func(context.Context, int, int) (int, error) {
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
		smrtiRespawnWait, stopBeforeInstall = prevWait, prevOrder
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
	if note != "the engine restarted" {
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
	// Nothing to stop: no engine holds the port.
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
	if note != "no engine is running here, so it loads the next time the engine starts" {
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

// pinnedEngine is an engine whose files cannot be replaced while it runs, the
// way Windows treats a live process. stopFrees says whether stopping it lets
// go of its port, and calls records whether the port was taken while the
// package installed.
func pinnedEngine(t *testing.T, stopFrees bool) (cfg config.MemoryConfig, calls *[]string) {
	t.Helper()
	calls = fakeInstaller(t, 4242, false, nil)
	prev := stopBeforeInstall
	stopBeforeInstall = true
	t.Cleanup(func() { stopBeforeInstall = prev })

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"spaces":{},"space":"main","version":"0.11.3"}`)
	}))
	t.Cleanup(engine.Close)
	addr := engine.Listener.Addr().String()
	stopEngine = func(_ context.Context, _, want int) (int, error) {
		*calls = append(*calls, fmt.Sprintf("stop %d", want))
		if stopFrees {
			engine.Close()
		}
		return 4242, nil
	}
	upgradePackage = func(_ context.Context, exe, _ string, _ memory.Progress) (string, error) {
		port := "free"
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			_ = c.Close()
			port = "taken"
		}
		*calls = append(*calls, "upgrade with the port "+port)
		return "venv", nil
	}
	return config.MemoryConfig{Mode: "sidecar", URL: engine.URL, Host: "127.0.0.1", Port: 8420}, calls
}

// Where a live engine pins its own files, it is stopped before the install,
// whichever engine it is, and its port is held while pip runs so the
// supervisor waits rather than spawning on half-installed files. The hold is
// let go once the install is done, or nothing could bring the engine back.
func TestSmrtiApplyStopsAnEngineThatPinsItsFilesFirst(t *testing.T) {
	quickPacing(t)
	noDocker(t)
	installed(t, `C:\Users\u\.factor\venv\Scripts\smrti.exe`, "0.11.3")
	fakePyPI(t, "0.13.0")
	cfg, calls := pinnedEngine(t, true)

	s := NewSmrti(cfg, nil)
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	note, err := s.Apply(context.Background(), rel, func(f string, a ...any) { steps = append(steps, fmt.Sprintf(f, a...)) })
	if err != nil {
		t.Fatal(err)
	}
	if line := strings.Join(*calls, " | "); line != fmt.Sprintf("stop %d | upgrade with the port taken", memory.AnyEngine) {
		t.Fatalf("calls = %q", line)
	}
	if c, err := net.DialTimeout("tcp", s.address(), 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Error("the memory port was still held after the install")
	}
	if !strings.Contains(note, "starts with the next factor run") {
		t.Errorf("note = %q", note)
	}
	if !strings.Contains(strings.Join(steps, " "), "installed smrti 0.13.0 with venv") {
		t.Errorf("progress = %v", steps)
	}
}

// A port still taken after the stop is an engine still running, and pip run
// under it fails halfway and leaves the package renamed aside with no
// metadata — so nothing is installed at all.
func TestSmrtiApplyInstallsNothingUnderAnEngineStillServing(t *testing.T) {
	quickPacing(t)
	noDocker(t)
	installed(t, `C:\Users\u\.factor\venv\Scripts\smrti.exe`, "0.11.3")
	fakePyPI(t, "0.13.0")
	cfg, calls := pinnedEngine(t, false)

	s := NewSmrti(cfg, nil)
	rel, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(context.Background(), rel, nil); err == nil || !strings.Contains(err.Error(), "nothing was installed") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(strings.Join(*calls, " "), "upgrade") {
		t.Errorf("installed under a live engine: %v", *calls)
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
		"upgraded smrti 0.11.3 to 0.13.0 — the engine restarted",
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
	stopEngine = func(context.Context, int, int) (int, error) { return 0, fmt.Errorf("operation not permitted") }
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

func TestSmrtiRestartRestartsThePackageEngine(t *testing.T) {
	quickPacing(t)
	noDocker(t)
	installed(t, "/home/u/.local/bin/smrti", "0.13.0")
	calls := fakeInstaller(t, 4242, true, nil)

	note, err := NewSmrti(engineConfig(t, true), nil).Restart(context.Background(), nil)
	if err != nil || note != "the engine restarted" {
		t.Fatalf("note = %q, err = %v", note, err)
	}
	if strings.Join(*calls, " ") != "stop" {
		t.Errorf("a restart installs nothing: %v", *calls)
	}
}

func TestSmrtiRestartRestartsTheContainer(t *testing.T) {
	quickPacing(t)
	engineDocker(t)
	var restarted []string
	prev := dockerCmd
	dockerCmd = func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "restart" {
			restarted = append(restarted, strings.Join(args, " "))
			return "", nil
		}
		return prev(ctx, args...)
	}
	t.Cleanup(func() { dockerCmd = prev })

	note, err := NewSmrti(engineConfig(t, true), nil).Restart(context.Background(), nil)
	if err != nil || !strings.Contains(note, "container restarted") {
		t.Fatalf("note = %q, err = %v", note, err)
	}
	if len(restarted) != 1 || !strings.HasPrefix(restarted[0], "restart -t "+smrtiStopTimeout+" ") {
		t.Errorf("docker calls = %v", restarted)
	}
}

func TestSmrtiRestartLeavesAnEngineElsewhereAlone(t *testing.T) {
	cfg := config.MemoryConfig{Mode: "external", URL: "http://memory.example.net:8420"}
	if _, err := NewSmrti(cfg, nil).Restart(context.Background(), nil); !errors.Is(err, ErrNotManaged) {
		t.Fatalf("error = %v", err)
	}
}
