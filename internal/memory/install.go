package memory

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/cpu"

	"github.com/cyqlelabs/factor/internal/config"
)

// smrti is not optional garnish — it is the agent's long-term memory — so
// Factor installs it itself instead of leaving the user with a warning. The
// strategies below are ordered from most isolated to most universal, and all
// of them work without root:
//
//	uv tool install   — fastest, isolated, no venv management
//	pipx install      — same idea, older and more widely installed
//	pip install --user— the classic; retried with --break-system-packages
//	                    when the distro marks its Python externally managed
//	python3 -m venv   — last resort, always available: a private venv under
//	                    $FACTOR_HOME/venv that nothing else can break
//
// Everything here shells out through package-level hooks so the whole
// decision tree is unit-testable without touching the machine.

var (
	lookPath = exec.LookPath
	runCmd   = func(ctx context.Context, argv []string) (string, error) {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.WaitDelay = 5 * time.Second
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
)

// InstallTimeout bounds one install attempt (wheels + ONNX deps are chunky).
const InstallTimeout = 15 * time.Minute

// PackageName is what we install; the executable it provides is BinaryName.
const PackageName = "smrti"

// NumpyConstraint rides along with smrti on machines whose CPU numpy's own
// wheels cannot run on. Since 2.0 those wheels target the x86-64-v2 baseline —
// SSE4.2 — and below it the extension modules do not merely run slowly, they
// execute an illegal instruction: `import numpy` dies with SIGILL, taking the
// engine with it. SIGILL is a signal rather than an exception, so nothing
// downstream can catch or retry it; the only fix is to not install that wheel.
// 1.x is the last line whose baseline those CPUs can execute, and smrti's
// dependencies are all happy with it (gliner2-onnx asks for numpy>=1.26).
const NumpyConstraint = "numpy<2"

// needsNumpyPin reports whether this machine is one of them. A var so a test
// can drive both paths on whatever CPU it happens to run on.
var needsNumpyPin = func() bool {
	// Only x86 has the baseline problem; the check is meaningless elsewhere,
	// and cpu.X86 reads as all-false on other architectures, which would
	// otherwise pin numpy on every arm64 machine.
	switch runtime.GOARCH {
	case "amd64", "386":
		return !cpu.X86.HasSSE42
	default:
		return false
	}
}

// pinned appends the numpy constraint to a requirement list when this machine
// needs it, so every installer path expresses it the same way.
func pinned(requirements ...string) []string {
	if needsNumpyPin() {
		return append(requirements, NumpyConstraint)
	}
	return requirements
}

// BinaryName is the smrti executable name for this platform.
func BinaryName() string {
	if runtime.GOOS == "windows" {
		return "smrti.exe"
	}
	return "smrti"
}

// VenvDir is the private virtualenv Factor falls back to.
func VenvDir(home string) string { return filepath.Join(home, "venv") }

func venvBinDir(home string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(VenvDir(home), "Scripts")
	}
	return filepath.Join(VenvDir(home), "bin")
}

// searchDirs are the places user-level installers drop executables. They are
// frequently missing from a desktop session's PATH (notoriously so for
// non-login shells and .desktop launchers), which is why finding smrti must
// not stop at exec.LookPath.
func searchDirs(home string) []string {
	dirs := []string{venvBinDir(home)}
	if userHome, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
			filepath.Join(userHome, ".local", "bin"),
			filepath.Join(userHome, ".local", "share", "uv", "tools", PackageName, "bin"),
			filepath.Join(userHome, "bin"),
			filepath.Join(userHome, "AppData", "Roaming", "Python", "Scripts"), // pip --user on Windows
		)
		// pip --user on Windows actually drops scripts under a per-version
		// directory (AppData\Roaming\Python\Python313\Scripts), so the fixed
		// path above misses every real install; the version is globbed.
		if versioned, err := filepath.Glob(filepath.Join(userHome, "AppData", "Roaming", "Python", "Python*", "Scripts")); err == nil {
			dirs = append(dirs, versioned...)
		}
	}
	return append(dirs, systemBinDirs...)
}

// systemBinDirs are searched last (a var so tests stay hermetic on machines
// that already have smrti installed system-wide).
var systemBinDirs = []string{"/usr/local/bin", "/usr/bin", "/opt/homebrew/bin"}

// FindSmrti resolves the smrti executable. An explicit command wins (absolute
// path or PATH lookup); otherwise PATH is searched, then the well-known user
// install directories. The returned path is what the sidecar should exec.
func FindSmrti(command, home string) (string, bool) {
	if command == "" {
		command = BinaryName()
	}
	if strings.ContainsRune(command, os.PathSeparator) {
		if info, err := os.Stat(command); err == nil && !info.IsDir() {
			return command, true
		}
		return "", false
	}
	if path, err := lookPath(command); err == nil {
		return path, true
	}
	// Only the default binary name is worth hunting for outside PATH; a user
	// who configured a custom command means that exact command. The default
	// has two spellings — the config default ("smrti") and this platform's
	// binary ("smrti.exe" on Windows) — and both must count: comparing against
	// the binary name alone made a default Windows config read as a custom
	// command, which skipped the search below and reported the venv install
	// missing on every start.
	if strings.TrimSuffix(command, ".exe") != PackageName {
		return "", false
	}
	for _, dir := range searchDirs(home) {
		candidate := filepath.Join(dir, BinaryName())
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

// runnableTimeout bounds the probe below. Two seconds is typical even on slow
// hardware — it is one Python import chain — but a cold page cache on a busy
// machine can take considerably longer, and a timeout that fires early would
// condemn a working install.
const runnableTimeout = 60 * time.Second

// Runnable reports whether the smrti at path can actually execute. Finding the
// file is not the same as being able to run it: an install can carry wheels
// this CPU has no instructions for, and `import numpy` then dies with SIGILL
// before smrti prints a word. A binary like that must not be adopted with a
// checkmark — it has to send the caller back to the installer, which knows how
// to constrain the install so it works here.
//
// --help is the cheapest command that still loads the whole import chain, so
// the probe fails for exactly the reasons serving would. When it does, detail
// carries the tail of the probe's output: "cannot run" without the traceback
// that says why (an smrti importing fcntl on Windows, say) is a diagnosis
// that cannot be made from the log.
func Runnable(ctx context.Context, path string) (ok bool, detail string) {
	ctx, cancel := context.WithTimeout(ctx, runnableTimeout)
	defer cancel()
	out, err := runCmd(ctx, []string{path, "--help"})
	if err == nil {
		return true, ""
	}
	return false, strings.TrimSpace(lastLines(out, 6))
}

// findRunnableSmrti resolves the default binary the way FindSmrti does, but
// skips candidates that cannot execute. Right after an install the search
// order can still put a broken leftover ahead of the fresh binary — the PATH
// hit a constrained reinstall was working around, or a stale venv shadowing a
// pip --user install — and adopting one of those hands the supervisor a crash
// loop; the freshly installed one is whichever candidate actually runs. When
// nothing runs, detail carries the last candidate's probe output — the error
// an install that "succeeded" needs to explain itself with.
func findRunnableSmrti(ctx context.Context, home string) (path string, ok bool, detail string) {
	if p, err := lookPath(BinaryName()); err == nil {
		ok, probe := Runnable(ctx, p)
		if ok {
			return p, true, ""
		}
		detail = probe
	}
	for _, dir := range searchDirs(home) {
		candidate := filepath.Join(dir, BinaryName())
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			ok, probe := Runnable(ctx, candidate)
			if ok {
				return candidate, true, ""
			}
			detail = probe
		}
	}
	return "", false, detail
}

type installStrategy struct {
	name  string
	probe string // executable that must exist for this strategy
	// build returns the commands to run in order; home is $FACTOR_HOME.
	build func(home string) [][]string
	// retry, when non-nil, produces a second attempt for the given failure
	// output (used for PEP 668 externally-managed environments).
	retry func(home, output string) [][]string
}

func pythonBin() string {
	for _, c := range []string{"python3", "python"} {
		if _, err := lookPath(c); err == nil {
			return c
		}
	}
	return ""
}

func strategies() []installStrategy {
	return []installStrategy{
		{
			name:  "uv",
			probe: "uv",
			build: func(string) [][]string {
				cmd := []string{"uv", "tool", "install", PackageName}
				if needsNumpyPin() {
					// uv resolves the tool's own environment, so the constraint
					// has to enter it as another requirement of that tool.
					cmd = append(cmd, "--with", NumpyConstraint)
				}
				return [][]string{cmd}
			},
		},
		{
			name:  "pipx",
			probe: "pipx",
			build: func(string) [][]string {
				cmds := [][]string{{"pipx", "install", PackageName}}
				if needsNumpyPin() {
					// pipx install takes no extra requirements; inject puts
					// them into the venv it just made.
					cmds = append(cmds, []string{"pipx", "inject", PackageName, NumpyConstraint})
				}
				return cmds
			},
		},
		{
			name:  "pip",
			probe: "", // resolved dynamically: pip3, pip, or python -m pip
			build: func(string) [][]string {
				pip := pipCommand()
				if pip == nil {
					return nil
				}
				return [][]string{append(append(pip, "install", "--user", "--upgrade"), pinned(PackageName)...)}
			},
			retry: func(_ string, output string) [][]string {
				// Debian/Fedora/Arch mark the system Python externally managed
				// (PEP 668). --break-system-packages only affects the user
				// site-packages here, since we always pass --user.
				if !strings.Contains(output, "externally-managed-environment") &&
					!strings.Contains(output, "externally managed") {
					return nil
				}
				pip := pipCommand()
				if pip == nil {
					return nil
				}
				return [][]string{append(append(pip, "install", "--user", "--upgrade", "--break-system-packages"), pinned(PackageName)...)}
			},
		},
		{
			name:  "venv",
			probe: "",
			build: func(home string) [][]string {
				py := pythonBin()
				if py == "" {
					return nil
				}
				venvPip := filepath.Join(venvBinDir(home), "pip")
				if runtime.GOOS == "windows" {
					venvPip += ".exe"
				}
				return [][]string{
					{py, "-m", "venv", VenvDir(home)},
					append([]string{venvPip, "install", "--upgrade"}, pinned(PackageName)...),
				}
			},
		},
	}
}

func pipCommand() []string {
	for _, c := range []string{"pip3", "pip"} {
		if _, err := lookPath(c); err == nil {
			return []string{c}
		}
	}
	if py := pythonBin(); py != "" {
		return []string{py, "-m", "pip"}
	}
	return nil
}

func (s installStrategy) available(home string) bool {
	if s.probe != "" {
		if _, err := lookPath(s.probe); err != nil {
			return false
		}
	}
	return len(s.build(home)) > 0
}

// Progress reports installer steps to whoever is watching (wizard, logs).
type Progress func(format string, args ...any)

func (p Progress) emit(format string, args ...any) {
	if p != nil {
		p(format, args...)
	}
}

// Install installs smrti with the first available strategy and returns the
// resolved executable path and the strategy that produced it.
func Install(ctx context.Context, home string, progress Progress) (path, method string, err error) {
	ctx, cancel := context.WithTimeout(ctx, InstallTimeout)
	defer cancel()

	var tried []string
	var lastErr error
	for _, s := range strategies() {
		if !s.available(home) {
			continue
		}
		tried = append(tried, s.name)
		progress.emit("installing %s with %s…", PackageName, s.name)
		out, err := runAll(ctx, s.build(home))
		if err != nil && s.retry != nil {
			if retryCmds := s.retry(home, out); retryCmds != nil {
				progress.emit("retrying with --break-system-packages (externally managed Python)…")
				out, err = runAll(ctx, retryCmds)
			}
		}
		if err != nil {
			lastErr = fmt.Errorf("%s: %v\n%s", s.name, err, lastLines(out, 12))
			progress.emit("%s failed: %v", s.name, err)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		p, ok, detail := findRunnableSmrti(ctx, home)
		if ok {
			progress.emit("installed %s via %s (%s)", PackageName, s.name, p)
			return p, s.name, nil
		}
		if detail == "" {
			detail = "nothing on disk"
		}
		lastErr = fmt.Errorf("%s reported success but no runnable %s was found: %s", s.name, BinaryName(), detail)
		progress.emit("%s finished but no runnable %s was found; trying the next installer", s.name, BinaryName())
	}

	switch {
	case lastErr != nil:
		return "", "", fmt.Errorf("could not install %s (tried %s): %w", PackageName, strings.Join(tried, ", "), lastErr)
	default:
		return "", "", fmt.Errorf("could not install %s: no Python installer found (need one of uv, pipx, pip, or python3)", PackageName)
	}
}

// Answering reports whether a smrti is already serving at the configured
// endpoint. An engine somebody runs in Docker, in a venv, or on another box
// is a working memory whatever this filesystem holds, so nothing should
// offer to install one on top of it — the same reasoning the phone status
// applies to a live speech server.
func Answering(ctx context.Context, cfg config.MemoryConfig) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return NewClient(cfg.BaseURL(), cfg.APIKey, "").CheckHealth(ctx) == nil
}

// EnsureSmrti returns the smrti path, installing it when missing and allowed.
// installed reports whether this call performed the installation.
func EnsureSmrti(ctx context.Context, command, home string, autoInstall bool, progress Progress) (path string, installed bool, err error) {
	found, ok := FindSmrti(command, home)
	if ok {
		runnable, detail := Runnable(ctx, found)
		if runnable {
			return found, false, nil
		}
		progress.emit("%s is installed but cannot run on this machine (%s); reinstalling it", found, detail)
	}
	if !autoInstall {
		if ok {
			return "", false, fmt.Errorf("%s cannot run on this machine and memory.auto_install is off — reinstall it, constraining numpy if this CPU predates SSE4.2 (%s)", found, NumpyConstraint)
		}
		return "", false, fmt.Errorf("%s not found and memory.auto_install is off — install it with: pip install %s", BinaryName(), PackageName)
	}
	p, _, err := Install(ctx, home, progress)
	if err != nil {
		return "", false, err
	}
	return p, true, nil
}

func runAll(ctx context.Context, cmds [][]string) (string, error) {
	var combined strings.Builder
	for _, argv := range cmds {
		out, err := runCmd(ctx, argv)
		combined.WriteString(out)
		if err != nil {
			return combined.String(), err
		}
	}
	return combined.String(), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
