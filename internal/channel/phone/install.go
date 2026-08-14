package phone

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Patter is a Python package, so the voice shell needs an interpreter Factor
// can trust. Rather than fight whatever Python the machine happens to have,
// the installer builds one private virtualenv under $FACTOR_HOME and pins the
// version it installs there: Patter is young, and an unpinned upgrade must not
// be able to break phone calls overnight.

//go:embed voiceshell.py
var voiceShellScript []byte

const (
	// PackageSpec pins the Patter release the embedded shell script is written
	// against. Bump it together with the script.
	PackageSpec = "getpatter==0.6.2"

	// PackageName is the distribution the venv is probed for.
	PackageName = "getpatter"

	// MinPythonMinor is the oldest Python 3.x minor Patter supports.
	MinPythonMinor = 11

	// InstallHint is what a user is told to run when the automatic install is
	// off or has failed.
	InstallHint = "python3 -m venv ~/.factor/voice-venv && ~/.factor/voice-venv/bin/pip install " + PackageSpec

	// InstallTimeout bounds one install attempt (the wheels are chunky).
	InstallTimeout = 15 * time.Minute
)

// Shelling out goes through these hooks so the whole decision tree is
// unit-testable without touching the machine's Python.
var (
	lookPath = exec.LookPath
	runCmd   = func(ctx context.Context, argv []string) (string, error) {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.WaitDelay = 5 * time.Second
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
)

// Progress reports installer steps to whoever is watching (wizard, logs).
type Progress func(format string, args ...any)

func (p Progress) emit(format string, args ...any) {
	if p != nil {
		p(format, args...)
	}
}

// VenvDir is the private virtualenv the voice shell runs in.
func VenvDir(home string) string { return filepath.Join(home, "voice-venv") }

func venvPython(home string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(VenvDir(home), "Scripts", "python.exe")
	}
	return filepath.Join(VenvDir(home), "bin", "python")
}

func venvPip(home string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(VenvDir(home), "Scripts", "pip.exe")
	}
	return filepath.Join(VenvDir(home), "bin", "pip")
}

// FindVoiceShellPython returns the interpreter that can run the voice shell:
// the private venv, once Patter is actually installed in it. A venv that
// exists but has no Patter is not usable — reporting it as ready would make
// the supervisor restart-loop against an ImportError.
func FindVoiceShellPython(home string) (string, bool) {
	python := venvPython(home)
	if info, err := os.Stat(python); err != nil || info.IsDir() {
		return "", false
	}
	if !hasPatter(python) {
		return "", false
	}
	return python, true
}

// hasPatter asks the interpreter whether the pinned distribution is installed.
// Querying the distribution (not an import name) keeps this working across
// Patter's own module renames.
func hasPatter(python string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := runCmd(ctx, []string{python, "-c",
		"import importlib.metadata as m; m.version('" + PackageName + "')"})
	return err == nil
}

// resolveInterpreter accepts an explicit command from the config: an absolute
// path (or anything with a separator) is used as-is, a bare name is looked up
// on PATH.
func resolveInterpreter(command string) (string, error) {
	if strings.ContainsRune(command, os.PathSeparator) {
		if info, err := os.Stat(command); err == nil && !info.IsDir() {
			return command, nil
		}
		return "", fmt.Errorf("channels.phone.command %q does not exist", command)
	}
	path, err := lookPath(command)
	if err != nil {
		return "", fmt.Errorf("channels.phone.command %q is not on PATH", command)
	}
	return path, nil
}

// systemPython returns the first interpreter on the machine new enough for
// Patter, and says which ones it rejected when there is none.
func systemPython() (string, error) {
	var seen []string
	for _, candidate := range pythonCandidates() {
		path, err := lookPath(candidate)
		if err != nil {
			continue
		}
		seen = append(seen, candidate)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, err = runCmd(ctx, []string{path, "-c",
			fmt.Sprintf("import sys; sys.exit(0 if sys.version_info >= (3, %d) else 1)", MinPythonMinor)})
		cancel()
		if err == nil {
			return path, nil
		}
	}
	if len(seen) == 0 {
		return "", fmt.Errorf("no Python interpreter found (Patter needs Python 3.%d or newer)", MinPythonMinor)
	}
	return "", fmt.Errorf("found %s but none is Python 3.%d or newer",
		strings.Join(seen, ", "), MinPythonMinor)
}

func pythonCandidates() []string {
	return []string{"python3.14", "python3.13", "python3.12", "python3.11", "python3", "python"}
}

// Install builds the private venv and installs the pinned Patter release into
// it, returning the interpreter to run the voice shell with.
func Install(ctx context.Context, home string, progress Progress) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, InstallTimeout)
	defer cancel()

	python, err := systemPython()
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(venvPython(home)); statErr != nil {
		progress.emit("creating the voice virtualenv at %s…", VenvDir(home))
		if out, err := runCmd(ctx, []string{python, "-m", "venv", VenvDir(home)}); err != nil {
			return "", fmt.Errorf("could not create %s: %v\n%s", VenvDir(home), err, lastLines(out, 8))
		}
	}
	progress.emit("installing %s…", PackageSpec)
	if out, err := runCmd(ctx, []string{venvPip(home), "install", "--upgrade", PackageSpec}); err != nil {
		return "", fmt.Errorf("could not install %s: %v\n%s", PackageSpec, err, lastLines(out, 12))
	}
	path, ok := FindVoiceShellPython(home)
	if !ok {
		return "", fmt.Errorf("pip reported success but %s is still not importable", PackageName)
	}
	progress.emit("Patter ready (%s)", path)
	return path, nil
}

// EnsurePatter returns the voice shell interpreter, installing Patter when it
// is missing and allowed. installed reports whether this call did the work.
func EnsurePatter(ctx context.Context, home string, autoInstall bool, progress Progress) (path string, installed bool, err error) {
	if p, ok := FindVoiceShellPython(home); ok {
		return p, false, nil
	}
	if !autoInstall {
		return "", false, fmt.Errorf("Patter is not installed and auto_install is off — %s", InstallHint)
	}
	p, err := Install(ctx, home, progress)
	if err != nil {
		return "", false, err
	}
	return p, true, nil
}

// WriteScript materializes the embedded voice shell next to the config, and
// refreshes it whenever Factor is upgraded. The script is the only
// Patter-facing surface in the whole channel, so keeping it on disk also makes
// it inspectable when a call misbehaves.
func WriteScript(path string) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, voiceShellScript) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, voiceShellScript, 0o600); err != nil {
		return fmt.Errorf("write voice shell script: %w", err)
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
