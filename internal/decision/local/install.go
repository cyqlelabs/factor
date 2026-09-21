// Package local runs Factor's decision model on this machine.
//
// The typed decisions the loop and the browser executor ask for are small,
// bounded judgements, and the hosted answer to them costs a network round
// trip, an API key and a per-token bill. None of those is a good fit for a
// personal agent on somebody's own box, and the decision itself does not need
// a frontier model: Laya answers choice, score and noul questions in one
// forward pass of a 322M-parameter encoder, with Apache-2.0 weights, and its
// own reply is already the shape TypeSafe's /v1/systemone returns.
//
// So Factor runs it the way it runs its other Python engines — a private
// virtualenv, an embedded server written to disk, a supervised child, health
// probes and a backoff — and there is nothing to configure: no key, no
// endpoint, no model to choose. Keeping the published /v1/systemone shape on
// the wire is what makes the server on the other end replaceable by anything
// else that speaks it, and what let this be written against a contract that
// already had an implementation to check against.
package local

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

	"golang.org/x/sys/cpu"
)

//go:embed layaserve.py
var serverScript []byte

const (
	// PackageSpec is pinned because the embedded server is written against
	// this API — laya.load, Router(preload=True), and system_one's reply
	// shape. Bump it together with layaserve.py.
	PackageSpec = "laya==0.3.4"
	// PackageName is what the venv is probed for.
	PackageName = "laya"

	// Checkpoint is the one this server loads, and it is a constant rather
	// than a setting. Laya's English checkpoint scores higher on English and
	// collapses on everything else while staying confident, which is the one
	// failure shape a confidence gate cannot catch; the multilingual one is
	// smaller, faster, carries twice the context and reads 45 of 51
	// languages. There is no version of this choice worth putting in front
	// of a user.
	Checkpoint = "multilingual"

	// MinPythonMinor is Laya's floor (it declares >=3.8); torch is the real
	// constraint on old interpreters, and 3.9 is the oldest anything current
	// publishes wheels for.
	MinPythonMinor = 9

	// UVPython is the interpreter uv is asked to supply when the machine has
	// none of its own that is new enough. It is pinned rather than left open
	// because torch publishes wheels a release or two behind the newest
	// CPython, and a virtualenv built on a version nothing has built for is a
	// long download that ends in "no matching distribution".
	UVPython = "3.12"

	// DefaultPort sits clear of the gateway (8720), the speech server (8726),
	// the phone bridge and the voice control endpoint (8730).
	DefaultPort = 8731

	// NumpyConstraint rides along with Laya on machines whose CPU numpy's own
	// wheels cannot run on. Since 2.0 those wheels target the x86-64-v2
	// baseline — SSE4.2 — and below it `import numpy` does not run slowly, it
	// executes an illegal instruction and takes the interpreter with it.
	// Measured on the oldest box here: torch imports and computes fine, and
	// Laya then dies on SIGILL the moment its own import reaches numpy, which
	// no supervisor can catch or retry because a signal is not an exception.
	// The memory engine pins the same ceiling for the same reason.
	NumpyConstraint = "numpy<2"

	// TorchCPUIndex is where the CPU-only build of torch lives.
	//
	// This matters more than it looks. Laya declares nothing but
	// "torch>=2.0.0", and on Linux and Windows pip resolves that to the CUDA
	// build: measured here, a plain `pip install laya` lays down 5.6 GB, of
	// which 3.2 GB is NVIDIA runtime libraries and 897 MB is triton — on a
	// machine that may well have no GPU at all. Factor runs on boxes with a
	// couple of slow cores and a few gigabytes of disk, so the CPU wheel is
	// installed first and Laya then finds its dependency already satisfied,
	// which brings the whole virtualenv to about 600 MB.
	//
	// Someone who has asked for CUDA gets the default resolution instead:
	// they have the hardware and they said so.
	TorchCPUIndex = "https://download.pytorch.org/whl/cpu"

	// InstallTimeout bounds one install. Laya is a small wheel, but it pulls
	// torch and transformers behind it, which on a slow connection is a few
	// hundred megabytes.
	InstallTimeout = 30 * time.Minute

	// LoadTimeout is how long the server may take to build its checkpoint
	// before Factor gives up on this attempt and backs off. The first run
	// downloads the weights — a few hundred megabytes — so it is generous;
	// every run after that is seconds.
	LoadTimeout = 20 * time.Minute
)

// Config is what little the local model exposes. Every field is optional and
// the defaults are meant to be what everyone uses: there is no model to pick,
// no key to hold and no endpoint to name, because there is only one model and
// it runs here.
type Config struct {
	// Port is where the managed server listens.
	Port int `json:"port,omitempty"`
	// Device is what torch runs on: blank lets the model choose, "cpu" and
	// "cuda" force one.
	Device string `json:"device,omitempty"`
	// Command runs the server on an interpreter of your choosing instead of
	// the private virtualenv.
	Command string `json:"command,omitempty"`
	// AutoInstall lets Factor build that virtualenv when it is missing.
	// Nil means yes.
	AutoInstall *bool `json:"auto_install,omitempty"`
}

func (c Config) port() int {
	if c.Port > 0 {
		return c.Port
	}
	return DefaultPort
}

func (c Config) autoInstall() bool { return c.AutoInstall == nil || *c.AutoInstall }

// BaseURL is where the managed server answers.
func (c Config) BaseURL() string { return fmt.Sprintf("http://127.0.0.1:%d", c.port()) }

// VenvDir is the private virtualenv the decision model runs in, kept apart
// from the voice and memory ones: torch is a large and opinionated
// dependency, and an install that breaks must break one engine rather than
// all of them.
func VenvDir(home string) string { return filepath.Join(home, "decision-venv") }

func venvBin(home, name string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(VenvDir(home), "Scripts", name+".exe")
	}
	return filepath.Join(VenvDir(home), "bin", name)
}

func venvPython(home string) string { return venvBin(home, "python") }
func venvPip(home string) string    { return venvBin(home, "pip") }

// ScriptPath is where the embedded server is written. On disk rather than
// piped in, so it is inspectable when a decision misbehaves.
func ScriptPath(home string) string { return filepath.Join(home, "layaserve.py") }

// WriteScript materializes the embedded server, leaving an unchanged file
// alone so an upgrade does not disturb one a running child is reading.
func WriteScript(path string) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, serverScript) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, serverScript, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

// FindPython returns the private virtualenv's interpreter once Laya is
// actually importable in it. A virtualenv that exists but has no Laya is a
// half-finished install, not an engine.
func FindPython(home string) (string, bool) {
	python := venvPython(home)
	if _, err := os.Stat(python); err != nil {
		return "", false
	}
	if !hasLaya(python) {
		return "", false
	}
	return python, true
}

func hasLaya(python string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := runCmd(ctx, []string{python, "-c", "import " + PackageName})
	return err == nil
}

// needsNumpyPin reports whether this machine is one whose CPU cannot execute
// numpy's current wheels. A var so a test can drive both paths on whatever CPU
// it happens to run on.
var needsNumpyPin = func() bool {
	// Only x86 has the baseline problem, and cpu.X86 reads as all-false
	// elsewhere, which would otherwise pin numpy on every arm64 machine.
	switch runtime.GOARCH {
	case "amd64", "386":
		return !cpu.X86.HasSSE42
	default:
		return false
	}
}

// wantsCUDA reports whether the default (GPU) resolution of torch is what
// this machine asked for. On macOS the published wheels are CPU-only anyway,
// so the separate index is neither needed nor always available there.
func wantsCUDA(device string) bool {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		return true // leave the resolution alone
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(device)), "cuda")
}

// Install builds the virtualenv and installs Laya into it, returning the
// interpreter to run the server with. device is what the model will run on,
// and decides which build of torch is worth fetching.
func Install(ctx context.Context, home, device string, progress func(format string, args ...any)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, InstallTimeout)
	defer cancel()
	emit := func(format string, args ...any) {
		if progress != nil {
			progress(format, args...)
		}
	}

	if _, statErr := os.Stat(venvPython(home)); statErr != nil {
		if err := createVenv(ctx, home, emit); err != nil {
			return "", err
		}
	}
	// torch first, and deliberately from the CPU index: see TorchCPUIndex.
	// Laya's own install then finds the dependency satisfied and leaves it
	// alone, instead of pulling several gigabytes of GPU runtime onto a
	// machine that cannot use it.
	if !wantsCUDA(device) {
		emit("installing the CPU build of torch (about 200 MB)…")
		if out, err := runCmd(ctx, []string{venvPip(home), "install", "--index-url", TorchCPUIndex, "torch"}); err != nil {
			// Not fatal: the next step resolves torch the ordinary way, which
			// works, just larger. A machine that cannot reach this index can
			// still have a decision model.
			emit("the CPU build could not be fetched (%v); falling back to the default, which is larger", err)
			_ = out
		}
	}
	emit("installing %s…", PackageSpec)
	// The numpy ceiling goes in the same command rather than after it: pip
	// resolves both at once, where installing Laya first and correcting it
	// afterwards downloads the wheel that cannot run on this machine and then
	// replaces it.
	install := []string{venvPip(home), "install", "--upgrade", PackageSpec}
	if needsNumpyPin() {
		install = append(install, NumpyConstraint)
	}
	if out, err := runCmd(ctx, install); err != nil {
		return "", fmt.Errorf("could not install %s: %v\n%s", PackageSpec, err, lastLines(out, 12))
	}
	path, ok := FindPython(home)
	if !ok {
		return "", fmt.Errorf("pip reported success but %s is still not importable", PackageName)
	}
	emit("the local decision model is ready (%s)", path)
	return path, nil
}

// ErrTooSmall is returned instead of an install or a spawn on a machine that
// has not the memory to load the model. It is exported so a caller can tell
// "this box cannot" from "this box has not yet".
var ErrTooSmall = errTooSmall

// EnsureLaya returns the interpreter to run the server with, installing Laya
// when it is missing and allowed. installed reports whether this call did it.
func EnsureLaya(ctx context.Context, home, device string, autoInstall bool,
	progress func(format string, args ...any)) (path string, installed bool, err error) {
	if p, ok := FindPython(home); ok {
		return p, false, nil
	}
	// A gigabyte of wheels is not worth fetching onto a machine that cannot
	// load what they install.
	if have, small := tooSmall(); small {
		return "", false, fmt.Errorf("%w: this machine has %d MB free and the model needs about %d MB to load",
			errTooSmall, have, minAvailableMB)
	}
	if !autoInstall {
		return "", false, fmt.Errorf("the local decision model is not installed and decision.auto_install is off — %s", InstallHint())
	}
	p, err := Install(ctx, home, device, progress)
	if err != nil {
		return "", false, err
	}
	return p, true, nil
}

// InstallHint is the command a user runs to do it by hand.
func InstallHint() string {
	if runtime.GOOS == "windows" {
		return `py -m venv %USERPROFILE%\.factor\decision-venv && %USERPROFILE%\.factor\decision-venv\Scripts\pip install --index-url ` +
			TorchCPUIndex + ` torch && %USERPROFILE%\.factor\decision-venv\Scripts\pip install ` + PackageSpec
	}
	return "python3 -m venv ~/.factor/decision-venv && ~/.factor/decision-venv/bin/pip install --index-url " +
		TorchCPUIndex + " torch && ~/.factor/decision-venv/bin/pip install " + PackageSpec
}

// resolveInterpreter accepts a configured command as either a path or a name
// on PATH.
func resolveInterpreter(command string) (string, error) {
	if strings.ContainsRune(command, os.PathSeparator) {
		if info, err := os.Stat(command); err == nil && !info.IsDir() {
			return command, nil
		}
		return "", fmt.Errorf("decision.local.command %q does not exist", command)
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("decision.local.command %q is not on PATH", command)
	}
	return path, nil
}

// createVenv builds the private virtualenv, preferring an interpreter the
// machine already has and falling back to uv, which can fetch one.
//
// That fallback is not a nicety. The oldest boxes Factor is meant to run on
// are exactly the ones whose system Python is years behind — Factor's own
// live box answers python3 with 3.8 and has no package manager to raise that
// with — while uv is already installed there and already runs the memory
// engine on a 3.12 it downloaded itself. Refusing the decision model on a
// machine that demonstrably can run it is the wrong answer to a feature that
// is supposed to need no setup.
func createVenv(ctx context.Context, home string, emit func(string, ...any)) error {
	python, err := systemPython()
	if err == nil {
		emit("creating the decision virtualenv at %s…", VenvDir(home))
		if out, runErr := runCmd(ctx, []string{python, "-m", "venv", VenvDir(home)}); runErr != nil {
			return fmt.Errorf("could not create %s: %v\n%s", VenvDir(home), runErr, lastLines(out, 8))
		}
		return nil
	}
	uv, lookErr := exec.LookPath("uv")
	if lookErr != nil {
		return err
	}
	emit("%v; letting uv fetch Python %s…", err, UVPython)
	// --seed is what puts pip in the virtualenv: uv leaves it out by default
	// and every step after this one installs through it. --allow-existing is
	// what makes this as idempotent as `python -m venv`, which is reached only
	// when there is no interpreter in the directory: uv refuses outright when
	// the directory is there at all, so a virtualenv an interrupted install
	// left half-built would be a dead end rather than something to finish.
	out, runErr := runCmd(ctx, []string{uv, "venv", "--seed", "--allow-existing", "--python", UVPython, VenvDir(home)})
	if runErr != nil {
		return fmt.Errorf("%v, and uv could not supply one: %v\n%s", err, runErr, lastLines(out, 8))
	}
	if _, statErr := os.Stat(venvPip(home)); statErr != nil {
		return fmt.Errorf("uv built %s without a pip to install into", VenvDir(home))
	}
	emit("uv supplied Python %s at %s", UVPython, VenvDir(home))
	return nil
}

// systemPython returns the first interpreter new enough to build the venv,
// and says which ones it rejected when there is none.
func systemPython() (string, error) {
	var seen []string
	for _, candidate := range pythonCandidates() {
		path, err := exec.LookPath(candidate)
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
		return "", fmt.Errorf("no Python interpreter found (the local decision model needs Python 3.%d or newer)", MinPythonMinor)
	}
	return "", fmt.Errorf("found %s but none is Python 3.%d or newer", strings.Join(seen, ", "), MinPythonMinor)
}

func pythonCandidates() []string {
	names := []string{"python3.14", "python3.13", "python3.12", "python3.11", "python3", "python"}
	if runtime.GOOS == "windows" {
		// The versioned names do not exist there, and python.exe resolves to
		// an App Execution Alias stub on a stock machine. py.exe lives in
		// C:\Windows, is always on PATH, and picks the newest Python 3.
		names = append(names, "py")
	}
	return names
}

// runCmd runs a command to completion and returns its combined output.
func runCmd(ctx context.Context, argv []string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// lastLines keeps the tail of a failed command's output, which is where the
// reason is.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
