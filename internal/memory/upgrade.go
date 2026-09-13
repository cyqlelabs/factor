package memory

// Keeping a smrti that was installed as a Python package current. The engine
// ships two ways — a container image, which internal/upgrade swaps, and a
// package on PATH, which is this file — and a Factor that upgrades itself while
// leaving either of them behind is only half current.
//
// The installer that made the install is the one that has to upgrade it.
// Running a different one leaves two smrtis on the machine with whichever comes
// first on PATH answering, so the method is read off the path the executable
// resolves to rather than guessed at, and a machine that no longer has that
// installer is told so instead of quietly acquiring a second copy.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The installers an install on disk can have come from.
const (
	MethodUv   = "uv"
	MethodPipx = "pipx"
	MethodVenv = "venv"
	MethodPip  = "pip"
)

// UpgradeMethod names the installer that owns the smrti at exe. The console
// script itself says which: uv and pipx both put it in a directory of their own
// and link it onto PATH, so the link is followed before the path is read.
// Anything else is a pip install — the one method that drops its script into a
// shared bin directory, and the fallback that is right whenever the layout is
// not one of the two this can recognise.
func UpgradeMethod(exe, home string) string {
	resolved := exe
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		resolved = r
	}
	path := filepath.ToSlash(resolved)
	venv := filepath.ToSlash(VenvDir(home)) + "/"
	switch {
	case strings.HasPrefix(path, venv):
		return MethodVenv
	case strings.Contains(path, "/uv/tools/"):
		return MethodUv
	case strings.Contains(path, "/pipx/venvs/"):
		return MethodPipx
	case goos == "windows" && toolVenvThere(uvToolRoots()):
		return MethodUv
	case goos == "windows" && toolVenvThere(pipxVenvRoots()):
		return MethodPipx
	default:
		return MethodPip
	}
}

// toolVenvThere reports whether one of these roots holds an environment for
// smrti. It is what reads the layout on Windows, where neither tool installer
// leaves a path to follow: uv and pipx both put a trampoline .exe in a shared
// bin directory instead of a symlink into the environment they made, so the
// executable says nothing about who made it and the default — pip — would
// install a second smrti beside the first and let PATH decide which answers.
// goos is a seam: the Windows layouts below have to be exercised on the
// machine the tests run on.
var goos = runtime.GOOS

func toolVenvThere(roots []string) bool {
	for _, root := range roots {
		if root == "" {
			continue
		}
		if info, err := os.Stat(filepath.Join(root, PackageName)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// uvToolRoots are the directories uv keeps tool environments in.
func uvToolRoots() []string {
	if dir := os.Getenv("UV_TOOL_DIR"); dir != "" {
		return []string{dir}
	}
	if goos == "windows" {
		return under(os.Getenv("APPDATA"), "uv", "tools")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".local", "share", "uv", "tools")}
}

// pipxVenvRoots are the directories pipx keeps its environments in.
func pipxVenvRoots() []string {
	if dir := os.Getenv("PIPX_HOME"); dir != "" {
		return under(dir, "venvs")
	}
	if goos == "windows" {
		return under(os.Getenv("LOCALAPPDATA"), "pipx", "pipx", "venvs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".local", "share", "pipx", "venvs"),
		filepath.Join(home, ".local", "pipx", "venvs"),
	}
}

// under joins a root that may not be set. An empty base is no directory at
// all: joining onto it yields a relative path, which would be resolved against
// whatever this process happens to have as its working directory and could
// answer for a directory belonging to something else entirely.
func under(base string, parts ...string) []string {
	if base == "" {
		return nil
	}
	return []string{filepath.Join(append([]string{base}, parts...)...)}
}

// Upgrade re-runs the installer behind exe so it installs the newest published
// smrti, and reports which installer did it. The running engine is untouched:
// a Python process keeps the modules it imported, so the new code only takes
// effect once something restarts it (StopEngine).
func Upgrade(ctx context.Context, exe, home string, progress Progress) (method string, err error) {
	ctx, cancel := context.WithTimeout(ctx, InstallTimeout)
	defer cancel()

	method = UpgradeMethod(exe, home)
	s, ok := strategyNamed(method)
	if !ok {
		return "", fmt.Errorf("no installer knows how to upgrade %s", exe)
	}
	cmds := s.upgrade(home)
	if len(cmds) == 0 {
		return "", fmt.Errorf("%s was installed with %s, which is not available here", exe, method)
	}
	if s.probe != "" {
		if _, err := lookPath(s.probe); err != nil {
			return "", fmt.Errorf("%s was installed with %s, which is no longer on PATH", exe, method)
		}
	}

	progress.emit("upgrading %s with %s…", PackageName, method)
	out, runErr := runAll(ctx, cmds)
	if runErr != nil && s.retry != nil {
		if retryCmds := s.retry(home, out); retryCmds != nil {
			progress.emit("retrying with --break-system-packages (externally managed Python)…")
			out, runErr = runAll(ctx, retryCmds)
		}
	}
	if runErr != nil {
		return "", fmt.Errorf("upgrading %s with %s: %v\n%s", PackageName, method, runErr, lastLines(out, 12))
	}
	return method, nil
}

func strategyNamed(name string) (installStrategy, bool) {
	for _, s := range strategies() {
		if s.name == name && s.upgrade != nil {
			return s, true
		}
	}
	return installStrategy{}, false
}

// versionProbe bounds the interpreter call below. It reads one metadata file
// after a bare interpreter start — 50ms on this machine — so a probe that takes
// seconds is a machine under load rather than an answer on its way.
const versionProbe = 20 * time.Second

// versionScript prints the version of the installed smrti distribution.
const versionScript = "import importlib.metadata as m; print(m.version('" + PackageName + "'))"

// InstalledVersion reads the version of the smrti package behind exe, or ""
// when nothing here can say. The package's own metadata is the answer — the CLI
// has no --version flag — so the interpreter that runs the console script is
// asked for it.
func InstalledVersion(ctx context.Context, exe string) string {
	ctx, cancel := context.WithTimeout(ctx, versionProbe)
	defer cancel()
	for _, py := range interpretersFor(exe) {
		out, err := runCmd(ctx, []string{py, "-c", versionScript})
		if err != nil {
			continue
		}
		if v := strings.TrimSpace(lastLines(out, 1)); v != "" && v[0] >= '0' && v[0] <= '9' {
			return v
		}
	}
	return ""
}

// interpretersFor lists the Pythons that might own exe, best first. A venv
// install — uv, pipx and Factor's own venv are all venvs — keeps its
// interpreter beside the script; a pip --user install does not, and names its
// interpreter in the script's shebang instead. PATH is the last resort, which
// is the right answer for exactly the install that has no interpreter of its
// own.
func interpretersFor(exe string) []string {
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	var pythons []string
	dir := filepath.Dir(exe)
	for _, name := range []string{"python3", "python"} {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			pythons = append(pythons, candidate)
		}
	}
	if shebang := scriptInterpreter(exe); shebang != "" {
		pythons = append(pythons, shebang)
	}
	if py := pythonBin(); py != "" {
		pythons = append(pythons, py)
	}
	return pythons
}

// scriptInterpreter reads the interpreter out of a console script's shebang.
// Windows console scripts are executables with the shebang buried in them, so
// nothing is read there rather than a binary being scanned for a path.
func scriptInterpreter(exe string) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "#!") {
		return ""
	}
	// "#!/usr/bin/env python3" names the interpreter in the second field.
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(fields) == 0 {
		return ""
	}
	if strings.HasSuffix(fields[0], "env") && len(fields) > 1 {
		return fields[1]
	}
	return fields[0]
}
