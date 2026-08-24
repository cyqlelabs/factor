// Package desktop gives the agent hands on the graphical session: listing and
// controlling windows, taking screenshots, moving the mouse, typing, the
// clipboard, notifications, and opening files or URLs.
//
// Everything is done through the desktop's own helper programs (xdotool,
// wmctrl, scrot, xclip, notify-send on X11; grim/wl-clipboard/wtype on
// Wayland; osascript/screencapture on macOS; PowerShell on Windows) rather
// than CGO bindings, which keeps Factor a single static binary that still
// runs on an old Puppy Linux box. Missing helpers are reported as actionable
// errors ("install xdotool"), never as silent no-ops.
package desktop

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Runner executes one helper program with optional stdin and returns its
// standard output. Tests substitute a scripted runner.
type Runner func(ctx context.Context, stdin string, argv ...string) (string, error)

// Env is the seam between the controllers and the machine.
type Env struct {
	Run    Runner
	Has    func(bin string) bool
	Getenv func(key string) string
	Glob   func(pattern string) ([]string, error)
	GOOS   string
}

// defaultRunnerTimeout bounds one helper invocation; desktop helpers are
// interactive-speed programs, so anything slower than this is hung.
const defaultRunnerTimeout = 30 * time.Second

// DefaultEnv wires Env to the real machine.
func DefaultEnv() Env {
	return Env{
		Run:    execRunner(defaultRunnerTimeout),
		Has:    hasBinary,
		Getenv: os.Getenv,
		Glob:   filepath.Glob,
		GOOS:   runtime.GOOS,
	}
}

func (e Env) has(bin string) bool {
	if e.Has == nil {
		return false
	}
	return e.Has(bin)
}

func (e Env) env(key string) string {
	if e.Getenv == nil {
		return ""
	}
	return e.Getenv(key)
}

// first returns the first installed binary from the preference list.
func (e Env) first(bins ...string) (string, bool) {
	for _, b := range bins {
		if e.has(b) {
			return b, true
		}
	}
	return "", false
}

func hasBinary(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

func execRunner(timeout time.Duration) Runner {
	return func(ctx context.Context, stdin string, argv ...string) (string, error) {
		if len(argv) == 0 {
			return "", fmt.Errorf("empty command")
		}
		bin, err := exec.LookPath(argv[0])
		if err != nil {
			return "", fmt.Errorf("%s is not installed", argv[0])
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, argv[1:]...)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		// Separate streams: stderr noise must not corrupt parsed output
		// (clipboard contents, window tables). Files rather than buffers,
		// because a buffer makes os/exec build a pipe and wait for every
		// writer to close it — and the clipboard helpers (xclip, xsel,
		// wl-copy) must fork and stay resident to own the X selection. That
		// resident child inherits the pipe, so Wait blocks until WaitDelay
		// kills it, turning every successful clipboard write into a
		// two-second failure. A file descriptor costs it nothing.
		out, errf, cleanup, err := captureFiles()
		if err != nil {
			return "", err
		}
		defer cleanup()
		cmd.Stdout, cmd.Stderr = out, errf
		cmd.WaitDelay = 2 * time.Second
		runErr := cmd.Run()
		stdout := readCapture(out)
		if runErr != nil {
			if detail := strings.TrimSpace(readCapture(errf)); detail != "" {
				return stdout, fmt.Errorf("%s: %v: %s", argv[0], runErr, firstLine(detail))
			}
			return stdout, fmt.Errorf("%s: %v", argv[0], runErr)
		}
		return stdout, nil
	}
}

// captureFiles returns two throwaway files to collect a helper's output,
// along with the func that closes and removes them.
func captureFiles() (stdout, stderr *os.File, cleanup func(), err error) {
	if stdout, err = os.CreateTemp("", "factor-helper-out"); err != nil {
		return nil, nil, nil, err
	}
	if stderr, err = os.CreateTemp("", "factor-helper-err"); err != nil {
		_ = stdout.Close()
		_ = os.Remove(stdout.Name())
		return nil, nil, nil, err
	}
	return stdout, stderr, func() {
		for _, f := range []*os.File{stdout, stderr} {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}, nil
}

func readCapture(f *os.File) string {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(data)
}

// Window is one on-screen window.
type Window struct {
	ID      string
	PID     int
	App     string
	Title   string
	Desktop string
	X, Y    int
	W, H    int
	HasGeom bool
}

func (w Window) String() string {
	s := fmt.Sprintf("%s  %s", w.ID, w.Title)
	if w.App != "" {
		s += fmt.Sprintf("  [%s]", w.App)
	}
	if w.HasGeom {
		s += fmt.Sprintf("  %dx%d+%d+%d", w.W, w.H, w.X, w.Y)
	}
	if w.PID > 0 {
		s += fmt.Sprintf("  pid=%d", w.PID)
	}
	return s
}

// Geometry is a position/size pair; the Has* flags distinguish "0" from unset.
type Geometry struct {
	X, Y, W, H      int
	HasPos, HasSize bool
}

// Point is an absolute screen coordinate.
type Point struct{ X, Y int }

// Shot describes a screenshot request.
type Shot struct {
	Mode   string // screen | window | region
	Window Window
	Region Geometry
}

// Helper is an external program the desktop tools rely on.
type Helper struct {
	Bin      string
	Purpose  string
	Packages map[string]string // package manager -> package name (default: Bin)
}

// Package returns the package to install for the given manager.
func (h Helper) Package(manager string) string {
	if name, ok := h.Packages[manager]; ok {
		return name
	}
	return h.Bin
}

// Controller is the per-platform implementation seam.
type Controller interface {
	Backend() string
	Helpers() []Helper // in preference order; the tools report what's missing
	ListWindows(ctx context.Context) ([]Window, error)
	ActiveWindow(ctx context.Context) (Window, error)
	Focus(ctx context.Context, w Window) error
	CloseWindow(ctx context.Context, w Window) error
	SetState(ctx context.Context, w Window, state string) error
	MoveResize(ctx context.Context, w Window, g Geometry) error
	Screenshot(ctx context.Context, path string, shot Shot) error
	MoveMouse(ctx context.Context, x, y int) error
	Click(ctx context.Context, button string, count int, at *Point) error
	TypeText(ctx context.Context, text string, delayMs int) error
	PressKey(ctx context.Context, keys string, repeat int) error
	ClipboardGet(ctx context.Context) (string, error)
	ClipboardSet(ctx context.Context, text string) error
	Notify(ctx context.Context, title, body, urgency string) error
	Open(ctx context.Context, target string) error
	ScreenSize(ctx context.Context) (int, int, error)
}

// NewController picks the controller for the environment's platform.
func NewController(env Env) Controller {
	switch env.GOOS {
	case "darwin":
		return &macController{env: env}
	case "windows":
		return &windowsController{env: env}
	default:
		return &linuxController{env: env}
	}
}

// HasDisplay reports whether a graphical session is reachable. Factor runs on
// headless boxes too; there the desktop tools are pure prompt weight, so the
// composition root skips registering them (config can force them on).
func HasDisplay(env Env) bool {
	switch env.GOOS {
	case "darwin", "windows":
		return true
	default:
		return env.env("DISPLAY") != "" || env.env("WAYLAND_DISPLAY") != ""
	}
}

// MachineHasDisplay reports whether this machine drives a screen, which is
// not the same question as whether this process can reach it. A setup run
// over ssh has no DISPLAY of its own while the box in front of the user is
// running X the whole time — and deciding from the environment alone is how
// `factor init` came to skip the desktop step, and its dependencies, on
// exactly the desktop machines that needed them.
func MachineHasDisplay(env Env) bool {
	if HasDisplay(env) || env.GOOS == "darwin" || env.GOOS == "windows" {
		return true
	}
	key, _ := findDisplay(env)
	return key != ""
}

// AdoptDisplay points this process at the screen the machine drives when it
// was started without one, and reports what it adopted ("DISPLAY=:1"), or ""
// when this process already had a screen or the machine has none.
//
// A gateway started by systemd, launchd or a desktop autostart entry inherits
// no DISPLAY while X is running for that same user the whole time. Every
// helper program then fails to open a display, and ask_user tells a user
// sitting in front of the screen that the machine hasn't got one. The
// variable is set on the process rather than on each command so that
// everything downstream inherits it: the dialogs, the desktop helpers, the
// browser, and anything they spawn in turn.
func AdoptDisplay(env Env, setenv func(key, value string) error) string {
	if setenv == nil || HasDisplay(env) || env.GOOS == "darwin" || env.GOOS == "windows" {
		return ""
	}
	key, value := findDisplay(env)
	if key == "" || setenv(key, value) != nil {
		return ""
	}
	return key + "=" + value
}

// findDisplay looks for a display server this process was not told about and
// names it the way a helper program expects it: DISPLAY=:1 for X, and
// WAYLAND_DISPLAY=wayland-0 for Wayland.
func findDisplay(env Env) (key, value string) {
	if env.Glob == nil {
		return "", ""
	}
	if sock := firstSocket(env, "/tmp/.X11-unix/X*"); sock != "" {
		return "DISPLAY", ":" + strings.TrimPrefix(filepath.Base(sock), "X")
	}
	// With no XDG_RUNTIME_DIR the pattern would be a bare "wayland-*",
	// which Glob would resolve against the working directory.
	if dir := env.env("XDG_RUNTIME_DIR"); dir != "" {
		if sock := firstSocket(env, filepath.Join(dir, "wayland-*")); sock != "" {
			return "WAYLAND_DISPLAY", filepath.Base(sock)
		}
	}
	return "", ""
}

// firstSocket returns the lowest-numbered match, skipping the lock file a
// Wayland compositor leaves beside its socket: it is not one to connect to,
// and it outlives the compositor that wrote it.
func firstSocket(env Env, pattern string) string {
	matches, err := env.Glob(pattern)
	if err != nil {
		return ""
	}
	sort.Strings(matches)
	for _, m := range matches {
		if !strings.HasSuffix(m, ".lock") {
			return m
		}
	}
	return ""
}

// MissingHelpers lists helpers the controller wants but cannot find.
func MissingHelpers(env Env, c Controller) []Helper {
	var missing []Helper
	for _, h := range c.Helpers() {
		if !env.has(h.Bin) {
			missing = append(missing, h)
		}
	}
	return missing
}

// PackagesFor maps helpers to package names for one package manager,
// de-duplicated and sorted so install commands are stable.
func PackagesFor(helpers []Helper, manager string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range helpers {
		name := h.Package(manager)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// unsupported builds the error for an operation a backend cannot perform.
func unsupported(op, backend, hint string) error {
	msg := fmt.Sprintf("%s is not supported on %s", op, backend)
	if hint != "" {
		msg += " — " + hint
	}
	return fmt.Errorf("%s", msg)
}

// missingHelper builds the error for an operation whose helper is absent.
func missingHelper(op string, bins ...string) error {
	return fmt.Errorf("%s needs one of these programs, none of which is installed: %s (install one, e.g. with pkg_install)",
		op, strings.Join(bins, ", "))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// fieldsN splits on whitespace into at most n fields; the last field keeps
// the remainder verbatim (window titles contain spaces).
func fieldsN(s string, n int) []string {
	var out []string
	rest := strings.TrimLeft(s, " \t")
	for len(out) < n-1 {
		i := strings.IndexAny(rest, " \t")
		if i < 0 {
			break
		}
		out = append(out, rest[:i])
		rest = strings.TrimLeft(rest[i:], " \t")
	}
	if rest != "" {
		out = append(out, rest)
	}
	return out
}

func atoi(s string) int {
	n := 0
	neg := false
	for i, r := range s {
		if i == 0 && r == '-' {
			neg = true
			continue
		}
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	if neg {
		return -n
	}
	return n
}
