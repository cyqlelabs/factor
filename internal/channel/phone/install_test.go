package phone

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The installer decides whether to build a virtualenv, which Python to build
// it with, and whether the result is usable. All of that is exercised here
// without touching the machine's Python.

type fakeShell struct {
	t *testing.T
	// onPath is what exec.LookPath pretends to find.
	onPath map[string]bool
	// respond decides the outcome of one command; nil means success.
	respond func(argv []string) (string, error)
	ran     [][]string
}

func (f *fakeShell) install() {
	f.t.Helper()
	originalLook, originalRun := lookPath, runCmd
	f.t.Cleanup(func() { lookPath, runCmd = originalLook, originalRun })

	lookPath = func(file string) (string, error) {
		if f.onPath[file] {
			return "/usr/bin/" + file, nil
		}
		return "", exec.ErrNotFound
	}
	runCmd = func(_ context.Context, argv []string) (string, error) {
		f.ran = append(f.ran, argv)
		if f.respond == nil {
			return "", nil
		}
		return f.respond(argv)
	}
}

func (f *fakeShell) commands() string {
	var b strings.Builder
	for _, argv := range f.ran {
		b.WriteString(strings.Join(argv, " "))
		b.WriteString("\n")
	}
	return b.String()
}

// venvWith fakes a virtualenv on disk. installed says whether the pinned
// package is importable in it.
func venvWith(t *testing.T, home string) string {
	t.Helper()
	python := venvPython(home)
	if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(python, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return python
}

func TestFindVoiceShellPythonNeedsBothTheVenvAndThePackage(t *testing.T) {
	home := t.TempDir()

	shell := &fakeShell{t: t}
	shell.install()
	if _, ok := FindVoiceShellPython(home); ok {
		t.Error("found an interpreter with no virtualenv on disk")
	}

	python := venvWith(t, home)
	// The venv exists but Patter is not installed in it: reporting it ready
	// would make the supervisor restart-loop against an ImportError.
	shell.respond = func([]string) (string, error) { return "", errors.New("no such distribution") }
	if _, ok := FindVoiceShellPython(home); ok {
		t.Error("a virtualenv without Patter was reported as ready")
	}

	shell.respond = nil
	got, ok := FindVoiceShellPython(home)
	if !ok || got != python {
		t.Errorf("FindVoiceShellPython = %q, %v; want %q, true", got, ok, python)
	}
	if !strings.Contains(shell.commands(), "importlib.metadata") {
		t.Errorf("the package probe did not query the distribution:\n%s", shell.commands())
	}
	if !strings.Contains(shell.commands(), PackageName) {
		t.Error("the probe did not name the package it needs")
	}
}

func TestResolveInterpreter(t *testing.T) {
	shell := &fakeShell{t: t, onPath: map[string]bool{"python3": true}}
	shell.install()

	dir := t.TempDir()
	existing := filepath.Join(dir, "python")
	if err := os.WriteFile(existing, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got, err := resolveInterpreter(existing); err != nil || got != existing {
		t.Errorf("an explicit path = %q, %v", got, err)
	}
	if _, err := resolveInterpreter(filepath.Join(dir, "nope")); err == nil {
		t.Error("a missing explicit path was accepted")
	} else if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v", err)
	}
	if got, err := resolveInterpreter("python3"); err != nil || got != "/usr/bin/python3" {
		t.Errorf("a bare name = %q, %v", got, err)
	}
	if _, err := resolveInterpreter("python-that-is-not-here"); err == nil {
		t.Error("a bare name that is not on PATH was accepted")
	}
	if _, err := resolveInterpreter(dir); err == nil {
		t.Error("a directory was accepted as an interpreter")
	}
}

func TestSystemPythonPicksANewEnoughInterpreter(t *testing.T) {
	shell := &fakeShell{t: t, onPath: map[string]bool{"python3.11": true, "python3": true}}
	shell.install()
	// Pretend only 3.11 satisfies the version check.
	shell.respond = func(argv []string) (string, error) {
		if strings.Contains(argv[0], "3.11") {
			return "", nil
		}
		return "", errors.New("exit status 1")
	}
	got, err := systemPython()
	if err != nil {
		t.Fatalf("systemPython: %v", err)
	}
	if got != "/usr/bin/python3.11" {
		t.Errorf("systemPython = %q, want the new-enough interpreter", got)
	}
}

func TestSystemPythonExplainsWhatItRejected(t *testing.T) {
	shell := &fakeShell{t: t, onPath: map[string]bool{"python3": true}}
	shell.install()
	shell.respond = func([]string) (string, error) { return "", errors.New("exit status 1") }

	_, err := systemPython()
	if err == nil {
		t.Fatal("an old Python was accepted")
	}
	for _, want := range []string{"python3", "3.11"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	shell.onPath = map[string]bool{}
	if _, err := systemPython(); err == nil || !strings.Contains(err.Error(), "no Python interpreter found") {
		t.Errorf("with no Python at all, error = %v", err)
	}
}

func TestInstallBuildsAVirtualenvAndPinsThePackage(t *testing.T) {
	home := t.TempDir()
	shell := &fakeShell{t: t, onPath: map[string]bool{"python3": true}}
	shell.install()
	shell.respond = func(argv []string) (string, error) {
		// The venv appears the moment it is created.
		if len(argv) > 2 && argv[1] == "-m" && argv[2] == "venv" {
			venvWith(t, home)
		}
		return "", nil
	}

	var steps []string
	path, err := Install(context.Background(), home, func(format string, args ...any) {
		steps = append(steps, format)
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if path != venvPython(home) {
		t.Errorf("Install returned %q, want the venv interpreter", path)
	}
	ran := shell.commands()
	if !strings.Contains(ran, "-m venv "+VenvDir(home)) {
		t.Errorf("no virtualenv was created:\n%s", ran)
	}
	if !strings.Contains(ran, venvPip(home)+" install --upgrade "+PackageSpec) {
		t.Errorf("the pinned package was not installed:\n%s", ran)
	}
	if len(steps) == 0 {
		t.Error("the installer reported no progress")
	}
}

func TestInstallReusesAnExistingVirtualenv(t *testing.T) {
	home := t.TempDir()
	venvWith(t, home)
	shell := &fakeShell{t: t, onPath: map[string]bool{"python3": true}}
	shell.install()

	if _, err := Install(context.Background(), home, nil); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if strings.Contains(shell.commands(), "-m venv") {
		t.Errorf("an existing virtualenv was rebuilt:\n%s", shell.commands())
	}
}

func TestInstallReportsFailures(t *testing.T) {
	cases := []struct {
		name    string
		onPath  map[string]bool
		respond func(home string) func([]string) (string, error)
		wantErr string
	}{
		{
			name:    "no usable python",
			onPath:  map[string]bool{},
			wantErr: "no Python interpreter found",
		},
		{
			name:   "the virtualenv cannot be created",
			onPath: map[string]bool{"python3": true},
			respond: func(string) func([]string) (string, error) {
				return func(argv []string) (string, error) {
					if len(argv) > 2 && argv[2] == "venv" {
						return "permission denied", errors.New("exit status 1")
					}
					return "", nil
				}
			},
			wantErr: "could not create",
		},
		{
			name:   "pip fails",
			onPath: map[string]bool{"python3": true},
			respond: func(home string) func([]string) (string, error) {
				return func(argv []string) (string, error) {
					if strings.Contains(argv[0], "pip") {
						return "network unreachable", errors.New("exit status 1")
					}
					return "", nil
				}
			},
			wantErr: "could not install " + PackageSpec,
		},
		{
			name:   "pip lies about success",
			onPath: map[string]bool{"python3": true},
			respond: func(string) func([]string) (string, error) {
				return func([]string) (string, error) { return "", nil }
			},
			wantErr: "still not importable",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			shell := &fakeShell{t: t, onPath: c.onPath}
			shell.install()
			if c.respond != nil {
				shell.respond = c.respond(home)
			}
			_, err := Install(context.Background(), home, nil)
			if err == nil {
				t.Fatalf("Install succeeded; wanted %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

func TestEnsurePatter(t *testing.T) {
	home := t.TempDir()
	shell := &fakeShell{t: t, onPath: map[string]bool{"python3": true}}
	shell.install()

	// Auto-install off and nothing installed: an actionable error, no work.
	_, installed, err := EnsurePatter(context.Background(), home, false, nil)
	if err == nil || installed {
		t.Fatalf("EnsurePatter installed with auto_install off (installed=%v, err=%v)", installed, err)
	}
	if !strings.Contains(err.Error(), InstallHint) {
		t.Errorf("error %q does not tell the user how to install it", err)
	}

	shell.respond = func(argv []string) (string, error) {
		if len(argv) > 2 && argv[2] == "venv" {
			venvWith(t, home)
		}
		return "", nil
	}
	path, installed, err := EnsurePatter(context.Background(), home, true, nil)
	if err != nil || !installed || path == "" {
		t.Fatalf("EnsurePatter = %q, %v, %v", path, installed, err)
	}

	// Already installed: found, not reinstalled.
	shell.ran = nil
	_, installed, err = EnsurePatter(context.Background(), home, true, nil)
	if err != nil || installed {
		t.Errorf("EnsurePatter reinstalled an existing venv (installed=%v, err=%v)", installed, err)
	}
	if strings.Contains(shell.commands(), "-m venv") {
		t.Errorf("an existing install was rebuilt:\n%s", shell.commands())
	}
}

func TestEnsurePatterPropagatesInstallFailure(t *testing.T) {
	shell := &fakeShell{t: t, onPath: map[string]bool{}}
	shell.install()
	if _, _, err := EnsurePatter(context.Background(), t.TempDir(), true, nil); err == nil {
		t.Error("a failed install was reported as success")
	}
}

func TestWriteScriptIsIdempotentAndSelfHealing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "voiceshell.py")
	if err := WriteScript(path); err != nil {
		t.Fatalf("WriteScript: %v", err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Mode().Perm() != 0o600 {
		t.Errorf("script mode = %v, want 0600 (it carries no secrets, but nothing else should edit it)", first.Mode().Perm())
	}

	// An unchanged script is left alone…
	if err := WriteScript(path); err != nil {
		t.Fatalf("second WriteScript: %v", err)
	}
	// …and an edited or truncated one is restored, so a Factor upgrade always
	// ships the script its Go side expects.
	if err := os.WriteFile(path, []byte("# tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteScript(path); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(voiceShellScript) {
		t.Error("an edited script was not restored")
	}
}

func TestWriteScriptReportsAnUnusableLocation(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteScript(filepath.Join(blocker, "sub", "voiceshell.py")); err == nil {
		t.Error("WriteScript succeeded under a file")
	}
}

// The embedded script is the whole Patter integration; if it is not valid
// Python nothing else in this package matters.
func TestEmbeddedScriptIsValidPython(t *testing.T) {
	if len(voiceShellScript) == 0 {
		t.Fatal("the voice shell script was not embedded")
	}
	for _, marker := range []string{"FACTOR_VOICE_CONFIG", "/internal/call-event", "getpatter"} {
		if !strings.Contains(string(voiceShellScript), marker) {
			t.Errorf("the embedded script never mentions %q", marker)
		}
	}

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed on this machine")
	}
	path := filepath.Join(t.TempDir(), "voiceshell.py")
	if err := WriteScript(path); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(python, "-m", "py_compile", path).CombinedOutput()
	if err != nil {
		t.Errorf("the embedded script does not compile: %v\n%s", err, out)
	}
}
