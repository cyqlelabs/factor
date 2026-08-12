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
	// who configured a custom command means that exact command.
	if command != BinaryName() {
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
				return [][]string{{"uv", "tool", "install", PackageName}}
			},
		},
		{
			name:  "pipx",
			probe: "pipx",
			build: func(string) [][]string {
				return [][]string{{"pipx", "install", PackageName}}
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
				return [][]string{append(pip, "install", "--user", "--upgrade", PackageName)}
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
				return [][]string{append(pip, "install", "--user", "--upgrade", "--break-system-packages", PackageName)}
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
					{venvPip, "install", "--upgrade", PackageName},
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
		if p, ok := FindSmrti("", home); ok {
			progress.emit("installed %s via %s (%s)", PackageName, s.name, p)
			return p, s.name, nil
		}
		lastErr = fmt.Errorf("%s reported success but %s is still not on disk", s.name, BinaryName())
	}

	switch {
	case lastErr != nil:
		return "", "", fmt.Errorf("could not install %s (tried %s): %w", PackageName, strings.Join(tried, ", "), lastErr)
	default:
		return "", "", fmt.Errorf("could not install %s: no Python installer found (need one of uv, pipx, pip, or python3)", PackageName)
	}
}

// EnsureSmrti returns the smrti path, installing it when missing and allowed.
// installed reports whether this call performed the installation.
func EnsureSmrti(ctx context.Context, command, home string, autoInstall bool, progress Progress) (path string, installed bool, err error) {
	if p, ok := FindSmrti(command, home); ok {
		return p, false, nil
	}
	if !autoInstall {
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
