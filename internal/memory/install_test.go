package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/cpu"
)

// fakeEnv swaps the exec hooks for a scripted machine: present holds the
// executables that "exist", results maps a joined command line to its output
// and error, and log records every command that ran, in order.
type fakeEnv struct {
	present map[string]bool
	results map[string]struct {
		out string
		err error
	}
	log      []string
	onRun    func(argv []string) // side effects (e.g. creating the installed binary)
	restore  func()
	installs int
}

func newFakeEnv(t *testing.T, present ...string) *fakeEnv {
	t.Helper()
	f := &fakeEnv{present: map[string]bool{}, results: map[string]struct {
		out string
		err error
	}{}}
	for _, p := range present {
		f.present[p] = true
	}
	// Hermetic: no system bin dirs, and a home nothing else writes to.
	oldSystem := systemBinDirs
	systemBinDirs = nil
	t.Setenv("HOME", t.TempDir())

	oldLook, oldRun := lookPath, runCmd
	lookPath = func(name string) (string, error) {
		if f.present[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	runCmd = func(_ context.Context, argv []string) (string, error) {
		line := strings.Join(argv, " ")
		f.log = append(f.log, line)
		f.installs++
		if f.onRun != nil {
			f.onRun(argv)
		}
		if r, ok := f.results[line]; ok {
			return r.out, r.err
		}
		return "ok", nil
	}
	f.restore = func() {
		lookPath, runCmd = oldLook, oldRun
		systemBinDirs = oldSystem
	}
	t.Cleanup(f.restore)
	return f
}

func (f *fakeEnv) fail(line, out string) {
	f.results[line] = struct {
		out string
		err error
	}{out, errors.New("exit status 1")}
}

// installBinary makes the fake installer actually drop a binary where
// FindSmrti will see it, so the post-install resolution path is exercised.
func writeBinary(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, BinaryName())
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFindSmrtiPrefersPath(t *testing.T) {
	f := newFakeEnv(t, "smrti")
	_ = f
	path, ok := FindSmrti("", t.TempDir())
	if !ok || path != "/usr/bin/smrti" {
		t.Fatalf("FindSmrti = %q, %v; want the PATH hit", path, ok)
	}
}

func TestFindSmrtiFallsBackToVenv(t *testing.T) {
	newFakeEnv(t) // nothing on PATH
	home := t.TempDir()
	want := writeBinary(t, venvBinDir(home))
	got, ok := FindSmrti("smrti", home)
	if !ok || got != want {
		t.Fatalf("FindSmrti = %q, %v; want %q", got, ok, want)
	}
}

func TestFindSmrtiExplicitPath(t *testing.T) {
	newFakeEnv(t)
	home := t.TempDir()
	custom := writeBinary(t, filepath.Join(home, "custom"))
	if got, ok := FindSmrti(custom, home); !ok || got != custom {
		t.Fatalf("explicit path = %q, %v", got, ok)
	}
	missing := filepath.Join(home, "custom", "nope")
	if _, ok := FindSmrti(missing, home); ok {
		t.Fatal("missing explicit path reported as found")
	}
}

// A custom command must not silently resolve to a stray default binary.
func TestFindSmrtiCustomCommandDoesNotSearchDirs(t *testing.T) {
	newFakeEnv(t)
	home := t.TempDir()
	writeBinary(t, venvBinDir(home))
	if _, ok := FindSmrti("smrti-custom", home); ok {
		t.Fatal("custom command resolved to the default binary")
	}
}

func TestInstallPrefersUv(t *testing.T) {
	f := newFakeEnv(t, "uv", "pipx", "pip3", "python3")
	home := t.TempDir()
	f.onRun = func([]string) { writeBinary(t, venvBinDir(home)) }

	path, method, err := Install(context.Background(), home, nil)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if method != "uv" {
		t.Errorf("method = %q, want uv", method)
	}
	if len(f.log) != 1 || f.log[0] != "uv tool install smrti" {
		t.Errorf("commands = %v", f.log)
	}
	if path == "" {
		t.Error("no path returned")
	}
}

func TestInstallFallsThroughToVenv(t *testing.T) {
	f := newFakeEnv(t, "pipx", "pip3", "python3")
	home := t.TempDir()
	f.fail("pipx install smrti", "boom")
	f.fail("pip3 install --user --upgrade smrti", "boom")
	f.onRun = func(argv []string) {
		if strings.Contains(strings.Join(argv, " "), "venv") {
			writeBinary(t, venvBinDir(home))
		}
	}

	_, method, err := Install(context.Background(), home, nil)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if method != "venv" {
		t.Fatalf("method = %q, want venv (log: %v)", method, f.log)
	}
	wantVenv := "python3 -m venv " + VenvDir(home)
	if f.log[len(f.log)-2] != wantVenv {
		t.Errorf("venv command = %q, want %q", f.log[len(f.log)-2], wantVenv)
	}
}

// PEP 668 distros reject `pip install --user` outright; the retry with
// --break-system-packages is the difference between working and not on
// Debian, Fedora, and Arch.
func TestInstallRetriesExternallyManagedPython(t *testing.T) {
	f := newFakeEnv(t, "pip3")
	home := t.TempDir()
	f.fail("pip3 install --user --upgrade smrti", "error: externally-managed-environment")
	f.onRun = func(argv []string) {
		if strings.Contains(strings.Join(argv, " "), "break-system-packages") {
			writeBinary(t, filepath.Join(home, "venv", "bin"))
		}
	}

	_, method, err := Install(context.Background(), home, nil)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if method != "pip" {
		t.Errorf("method = %q, want pip", method)
	}
	last := f.log[len(f.log)-1]
	if !strings.Contains(last, "--break-system-packages") {
		t.Errorf("retry command = %q", last)
	}
}

func TestInstallWithoutAnyInstaller(t *testing.T) {
	newFakeEnv(t) // no uv, pipx, pip, or python
	if _, _, err := Install(context.Background(), t.TempDir(), nil); err == nil {
		t.Fatal("expected an error when no installer exists")
	} else if !strings.Contains(err.Error(), "no Python installer") {
		t.Errorf("error = %v", err)
	}
}

func TestInstallReportsFailureDetail(t *testing.T) {
	f := newFakeEnv(t, "uv")
	f.fail("uv tool install smrti", "resolution impossible\nno matching distribution")
	_, _, err := Install(context.Background(), t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "no matching distribution") {
		t.Errorf("error lost the installer output: %v", err)
	}
}

func TestEnsureSmrtiSkipsInstallWhenPresent(t *testing.T) {
	f := newFakeEnv(t, "smrti")
	path, installed, err := EnsureSmrti(context.Background(), "", t.TempDir(), true, nil)
	if err != nil || installed || path == "" {
		t.Fatalf("EnsureSmrti = %q, %v, %v", path, installed, err)
	}
	if f.installs != 0 {
		t.Errorf("ran %d commands; want none", f.installs)
	}
}

func TestEnsureSmrtiRespectsAutoInstallOff(t *testing.T) {
	newFakeEnv(t)
	_, _, err := EnsureSmrti(context.Background(), "smrti", t.TempDir(), false, nil)
	if err == nil || !strings.Contains(err.Error(), "auto_install") {
		t.Fatalf("error = %v", err)
	}
}

func TestEnsureSmrtiInstalls(t *testing.T) {
	f := newFakeEnv(t, "uv")
	home := t.TempDir()
	f.onRun = func([]string) { writeBinary(t, venvBinDir(home)) }
	var steps []string
	path, installed, err := EnsureSmrti(context.Background(), "", home, true,
		func(format string, args ...any) { steps = append(steps, format) })
	if err != nil || !installed || path == "" {
		t.Fatalf("EnsureSmrti = %q, %v, %v", path, installed, err)
	}
	if len(steps) == 0 {
		t.Error("no progress reported")
	}
}

func TestBinaryNameMatchesPlatform(t *testing.T) {
	want := "smrti"
	if runtime.GOOS == "windows" {
		want = "smrti.exe"
	}
	if BinaryName() != want {
		t.Errorf("BinaryName = %q, want %q", BinaryName(), want)
	}
}

// forceNumpyPin drives the CPU probe from a test, so both paths are covered
// whatever the machine running the suite happens to support.
func forceNumpyPin(t *testing.T, on bool) {
	t.Helper()
	prev := needsNumpyPin
	needsNumpyPin = func() bool { return on }
	t.Cleanup(func() { needsNumpyPin = prev })
}

// A CPU without SSE4.2 cannot execute numpy 2's wheels at all — `import numpy`
// is SIGILL — so every installer path has to carry the constraint, whichever
// one this machine ends up using.
func TestInstallPinsNumpyOnAnOldCPU(t *testing.T) {
	forceNumpyPin(t, true)

	cases := []struct {
		name       string
		present    []string
		fail       []string
		failOutput string
		want       string
	}{
		{"uv", []string{"uv"}, nil, "", "uv tool install smrti --with numpy<2"},
		{"pipx", []string{"pipx"}, nil, "", "pipx inject smrti numpy<2"},
		{"pip", []string{"pip3"}, nil, "", "pip3 install --user --upgrade smrti numpy<2"},
		{
			"pip break-system-packages",
			[]string{"pip3"},
			[]string{"pip3 install --user --upgrade smrti numpy<2"},
			"error: externally-managed-environment",
			"pip3 install --user --upgrade --break-system-packages smrti numpy<2",
		},
		// python3 alone still reaches pip via `python3 -m pip`, so venv is only
		// exercised once that has failed for a reason the retry does not cover.
		{
			"venv",
			[]string{"python3"},
			[]string{"python3 -m pip install --user --upgrade smrti numpy<2"},
			"boom",
			filepath.Join("venv", "bin", "pip") + " install --upgrade smrti numpy<2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeEnv(t, c.present...)
			home := t.TempDir()
			for _, line := range c.fail {
				f.fail(line, c.failOutput)
			}
			f.onRun = func([]string) { writeBinary(t, venvBinDir(home)) }

			if _, _, err := Install(context.Background(), home, nil); err != nil {
				t.Fatalf("Install: %v", err)
			}
			joined := strings.Join(f.log, " | ")
			if !strings.Contains(joined, c.want) {
				t.Errorf("commands %q do not carry %q", joined, c.want)
			}
		})
	}
}

// The pin is a workaround for hardware nobody should be held back by: a modern
// CPU installs whatever numpy the resolver picks.
func TestInstallLeavesNumpyAloneOnAModernCPU(t *testing.T) {
	forceNumpyPin(t, false)

	for _, present := range [][]string{{"uv"}, {"pipx"}, {"pip3"}, {"python3"}} {
		f := newFakeEnv(t, present...)
		home := t.TempDir()
		f.onRun = func([]string) { writeBinary(t, venvBinDir(home)) }

		if _, _, err := Install(context.Background(), home, nil); err != nil {
			t.Fatalf("Install with %v: %v", present, err)
		}
		if joined := strings.Join(f.log, " | "); strings.Contains(joined, "numpy") {
			t.Errorf("commands %q pin numpy on a CPU that does not need it", joined)
		}
	}
}

// The probe itself: the machine running this suite is overwhelmingly likely to
// be x86 with SSE4.2, and must not be pinned.
func TestNeedsNumpyPinLeavesCapableMachinesAlone(t *testing.T) {
	switch runtime.GOARCH {
	case "amd64", "386":
		if cpu.X86.HasSSE42 && needsNumpyPin() {
			t.Error("this CPU has SSE4.2; numpy 2 runs on it")
		}
	default:
		if needsNumpyPin() {
			t.Errorf("%s has no x86 baseline problem to work around", runtime.GOARCH)
		}
	}
}
