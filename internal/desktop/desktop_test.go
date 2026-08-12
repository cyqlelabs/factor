package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/tools"
)

// fakeMachine scripts the helper programs: which exist, what they print, and
// which fail. Every invocation is recorded so tests can assert on the exact
// command line built for each desktop operation.
type fakeMachine struct {
	installed map[string]bool
	env       map[string]string
	outputs   map[string]string
	errs      map[string]error
	calls     []fakeCall
	onRun     func(argv []string)
	goos      string
}

type fakeCall struct {
	argv  []string
	stdin string
}

func newMachine(goos string, installed ...string) *fakeMachine {
	m := &fakeMachine{
		installed: map[string]bool{},
		env:       map[string]string{"DISPLAY": ":0"},
		outputs:   map[string]string{},
		errs:      map[string]error{},
		goos:      goos,
	}
	for _, b := range installed {
		m.installed[b] = true
	}
	return m
}

// key matching: exact command line first, then "bin subcommand", then bin.
func (m *fakeMachine) lookup(table map[string]string, argv []string) (string, bool) {
	line := strings.Join(argv, " ")
	for _, k := range candidateKeys(line, argv) {
		if v, ok := table[k]; ok {
			return v, true
		}
	}
	return "", false
}

func candidateKeys(line string, argv []string) []string {
	keys := []string{line}
	if len(argv) >= 2 {
		keys = append(keys, argv[0]+" "+argv[1])
	}
	return append(keys, argv[0])
}

func (m *fakeMachine) Env() Env {
	return Env{
		GOOS:   m.goos,
		Has:    func(bin string) bool { return m.installed[bin] },
		Getenv: func(k string) string { return m.env[k] },
		Run: func(_ context.Context, stdin string, argv ...string) (string, error) {
			m.calls = append(m.calls, fakeCall{argv: argv, stdin: stdin})
			if m.onRun != nil {
				m.onRun(argv)
			}
			line := strings.Join(argv, " ")
			for _, k := range candidateKeys(line, argv) {
				if err, ok := m.errs[k]; ok {
					return "", err
				}
			}
			out, _ := m.lookup(m.outputs, argv)
			return out, nil
		},
	}
}

func (m *fakeMachine) lastCall() []string {
	if len(m.calls) == 0 {
		return nil
	}
	return m.calls[len(m.calls)-1].argv
}

func (m *fakeMachine) ranWith(substr string) bool {
	for _, c := range m.calls {
		if strings.Contains(strings.Join(c.argv, " "), substr) {
			return true
		}
	}
	return false
}

const wmctrlOutput = `0x03c00003  0 4242   0    27   1920 1053 code.Code             box  factor — Visual Studio Code
0x02200007  1 1337   100  100  800  600  gnome-terminal-server.Gnome-terminal  box  you@box: ~/factor
0x05000011 -1 55     0    0    1920 30   xfce4-panel.Xfce4-panel  box  panel
`

func newTools(t *testing.T, m *fakeMachine) (map[string]tools.Tool, string) {
	t.Helper()
	ws := t.TempDir()
	guard := tools.NewPathGuard(ws, true, false, nil)
	byName := map[string]tools.Tool{}
	for _, tool := range NewTools(m.Env(), guard, filepath.Join(ws, "screenshots")) {
		byName[tool.Name()] = tool
	}
	return byName, ws
}

func run(t *testing.T, tool tools.Tool, args map[string]any) *tools.Result {
	t.Helper()
	if err := tools.ValidateArgs(tool.Parameters(), args); err != nil {
		t.Fatalf("%s: arguments rejected by its own schema: %v", tool.Name(), err)
	}
	return tool.Execute(context.Background(), args)
}

func TestParseWmctrl(t *testing.T) {
	wins := parseWmctrl(wmctrlOutput)
	if len(wins) != 3 {
		t.Fatalf("parsed %d windows, want 3", len(wins))
	}
	w := wins[0]
	if w.ID != "0x03c00003" || w.PID != 4242 || w.App != "Code" {
		t.Errorf("window = %+v", w)
	}
	if w.X != 0 || w.Y != 27 || w.W != 1920 || w.H != 1053 {
		t.Errorf("geometry = %+v", w)
	}
	if w.Title != "factor — Visual Studio Code" {
		t.Errorf("title = %q (titles with spaces must survive)", w.Title)
	}
	if wins[1].Title != "you@box: ~/factor" {
		t.Errorf("second title = %q", wins[1].Title)
	}
}

func TestWindowListFiltersAndFormats(t *testing.T) {
	m := newMachine("linux", "wmctrl")
	m.outputs["wmctrl -l"] = wmctrlOutput
	byName, _ := newTools(t, m)

	res := run(t, byName["window_list"], map[string]any{})
	if res.IsError || !strings.Contains(res.ForLLM, "3 window(s)") {
		t.Fatalf("list = %+v", res)
	}
	if !strings.Contains(res.ForLLM, "pid=4242") || !strings.Contains(res.ForLLM, "1920x1053+0+27") {
		t.Errorf("list is missing detail:\n%s", res.ForLLM)
	}

	res = run(t, byName["window_list"], map[string]any{"filter": "terminal"})
	if strings.Contains(res.ForLLM, "Visual Studio") {
		t.Errorf("filter did not apply:\n%s", res.ForLLM)
	}
	res = run(t, byName["window_list"], map[string]any{"filter": "nothing-matches"})
	if !strings.Contains(res.ForLLM, "No window matches") {
		t.Errorf("empty filter result = %q", res.ForLLM)
	}
}

func TestWindowListFallsBackToXdotool(t *testing.T) {
	m := newMachine("linux", "xdotool")
	m.outputs["xdotool search"] = "111\n222\n"
	m.outputs["xdotool getwindowname"] = "A window"
	m.outputs["xdotool getwindowpid"] = "9"
	byName, _ := newTools(t, m)

	res := run(t, byName["window_list"], map[string]any{})
	if res.IsError || !strings.Contains(res.ForLLM, "2 window(s)") {
		t.Fatalf("list = %+v", res)
	}
}

func TestWindowListWithoutHelpers(t *testing.T) {
	m := newMachine("linux")
	byName, _ := newTools(t, m)
	res := run(t, byName["window_list"], map[string]any{})
	if !res.IsError || !strings.Contains(res.ForLLM, "wmctrl") {
		t.Fatalf("expected an actionable error, got %+v", res)
	}
}

func TestWindowControlResolvesByTitleAndActs(t *testing.T) {
	m := newMachine("linux", "wmctrl", "xdotool")
	m.outputs["wmctrl -l"] = wmctrlOutput
	byName, _ := newTools(t, m)

	cases := []struct {
		action string
		want   string
	}{
		{"focus", "wmctrl -i -a 0x02200007"},
		{"close", "wmctrl -i -c 0x02200007"},
		{"minimize", "xdotool windowminimize 0x02200007"},
		{"maximize", "wmctrl -i -r 0x02200007 -b add,maximized_vert,maximized_horz"},
		{"fullscreen", "wmctrl -i -r 0x02200007 -b add,fullscreen"},
	}
	for _, tc := range cases {
		m.calls = nil
		res := run(t, byName["window_control"], map[string]any{"action": tc.action, "window": "you@box"})
		if res.IsError {
			t.Fatalf("%s: %s", tc.action, res.ForLLM)
		}
		if !m.ranWith(tc.want) {
			t.Errorf("%s ran %v, want %q", tc.action, m.calls, tc.want)
		}
	}

	m.calls = nil
	res := run(t, byName["window_control"], map[string]any{
		"action": "move", "window": "0x03c00003", "x": 10, "y": 20, "width": 800, "height": 600})
	if res.IsError {
		t.Fatalf("move: %s", res.ForLLM)
	}
	if !m.ranWith("wmctrl -i -r 0x03c00003 -e 0,10,20,800,600") {
		t.Errorf("move ran %v", m.calls)
	}
	if !m.ranWith("remove,maximized_vert") {
		t.Error("move should un-maximize first; wmctrl ignores geometry otherwise")
	}
}

func TestWindowControlMoveNeedsGeometry(t *testing.T) {
	m := newMachine("linux", "wmctrl")
	m.outputs["wmctrl -l"] = wmctrlOutput
	byName, _ := newTools(t, m)
	res := run(t, byName["window_control"], map[string]any{"action": "move", "window": "panel"})
	if !res.IsError || !strings.Contains(res.ForLLM, "x/y") {
		t.Fatalf("res = %+v", res)
	}
}

func TestWindowResolutionErrors(t *testing.T) {
	m := newMachine("linux", "wmctrl")
	m.outputs["wmctrl -l"] = wmctrlOutput
	byName, _ := newTools(t, m)

	res := run(t, byName["window_control"], map[string]any{"action": "focus", "window": "nope"})
	if !res.IsError || !strings.Contains(res.ForLLM, "no window matches") {
		t.Errorf("unmatched window = %+v", res)
	}
	// A substring hitting several windows must be reported, not guessed at.
	res = run(t, byName["window_control"], map[string]any{"action": "focus", "window": "factor"})
	if !res.IsError || !strings.Contains(res.ForLLM, "matches 2 windows") {
		t.Errorf("ambiguous window = %+v", res)
	}
}

func TestWindowControlActiveWindow(t *testing.T) {
	m := newMachine("linux", "wmctrl", "xdotool")
	m.outputs["xdotool getactivewindow"] = "0x02200007\n"
	m.outputs["xdotool getwindowname"] = "you@box: ~/factor"
	byName, _ := newTools(t, m)

	res := run(t, byName["window_control"], map[string]any{"action": "focus", "window": "active"})
	if res.IsError || !m.ranWith("wmctrl -i -a 0x02200007") {
		t.Fatalf("res = %+v, calls = %v", res, m.calls)
	}
}

func TestScreenshotWritesInsideWorkspace(t *testing.T) {
	m := newMachine("linux", "scrot")
	byName, ws := newTools(t, m)
	m.onRun = func(argv []string) { // scrot writes the file
		if err := os.WriteFile(argv[len(argv)-1], []byte("PNG"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res := run(t, byName["screenshot"], map[string]any{})
	if res.IsError {
		t.Fatalf("screenshot: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, filepath.Join(ws, "screenshots")) {
		t.Errorf("saved outside the default dir: %s", res.ForLLM)
	}
	if !m.ranWith("scrot --overwrite") {
		t.Errorf("calls = %v", m.calls)
	}

	// Region mode passes the crop through, window mode focuses first.
	m.calls = nil
	res = run(t, byName["screenshot"], map[string]any{
		"target": "region", "x": 5, "y": 6, "width": 100, "height": 50, "path": "shots/region.png"})
	if res.IsError {
		t.Fatalf("region: %s", res.ForLLM)
	}
	if !m.ranWith("scrot --overwrite -a 5,6,100,50") {
		t.Errorf("region calls = %v", m.calls)
	}
}

func TestScreenshotRejectsEscapeAndBadRegion(t *testing.T) {
	m := newMachine("linux", "maim")
	byName, _ := newTools(t, m)

	res := run(t, byName["screenshot"], map[string]any{"path": "/etc/factor-escape.png"})
	if !res.IsError {
		t.Error("screenshot escaped the workspace")
	}
	res = run(t, byName["screenshot"], map[string]any{"target": "region", "x": 1, "y": 1})
	if !res.IsError || !strings.Contains(res.ForLLM, "width and height") {
		t.Errorf("region without size = %+v", res)
	}
}

func TestScreenshotReportsMissingFile(t *testing.T) {
	m := newMachine("linux", "maim") // runner succeeds but writes nothing
	byName, _ := newTools(t, m)
	res := run(t, byName["screenshot"], map[string]any{})
	if !res.IsError || !strings.Contains(res.ForLLM, "does not exist") {
		t.Fatalf("res = %+v", res)
	}
}

func TestScreenshotWithoutHelper(t *testing.T) {
	m := newMachine("linux")
	byName, _ := newTools(t, m)
	res := run(t, byName["screenshot"], map[string]any{})
	if !res.IsError || !strings.Contains(res.ForLLM, "scrot") {
		t.Fatalf("res = %+v", res)
	}
}

func TestMouse(t *testing.T) {
	m := newMachine("linux", "xdotool")
	byName, _ := newTools(t, m)

	if res := run(t, byName["mouse"], map[string]any{"action": "move", "x": 30, "y": 40}); res.IsError {
		t.Fatalf("move: %s", res.ForLLM)
	} else if !m.ranWith("xdotool mousemove 30 40") {
		t.Errorf("calls = %v", m.calls)
	}

	m.calls = nil
	if res := run(t, byName["mouse"], map[string]any{"action": "double_click", "x": 1, "y": 2}); res.IsError {
		t.Fatalf("double_click: %s", res.ForLLM)
	}
	if !m.ranWith("xdotool mousemove 1 2") || !m.ranWith("xdotool click --repeat 2 --delay 80 1") {
		t.Errorf("double click calls = %v", m.calls)
	}

	m.calls = nil
	if res := run(t, byName["mouse"], map[string]any{"action": "right_click"}); res.IsError {
		t.Fatalf("right_click: %s", res.ForLLM)
	}
	if got := strings.Join(m.lastCall(), " "); got != "xdotool click 3" {
		t.Errorf("right click ran %q", got)
	}

	if res := run(t, byName["mouse"], map[string]any{"action": "move", "x": 5}); !res.IsError {
		t.Error("x without y should fail")
	}
	if res := run(t, byName["mouse"], map[string]any{"action": "fly"}); !res.IsError {
		t.Error("unknown action should fail")
	}
}

func TestTypeText(t *testing.T) {
	m := newMachine("linux", "xdotool")
	byName, _ := newTools(t, m)

	res := run(t, byName["type_text"], map[string]any{"text": "--not-a-flag"})
	if res.IsError {
		t.Fatalf("type: %s", res.ForLLM)
	}
	got := m.lastCall()
	if got[len(got)-2] != "--" || got[len(got)-1] != "--not-a-flag" {
		t.Errorf("text must be passed after --; got %v", got)
	}
	if !m.ranWith("--delay 12") {
		t.Errorf("default delay missing: %v", got)
	}

	if res := run(t, byName["type_text"], map[string]any{"text": ""}); !res.IsError {
		t.Error("empty text should fail")
	}
	if res := run(t, byName["type_text"], map[string]any{"text": strings.Repeat("x", maxTypeChars+1)}); !res.IsError {
		t.Error("oversized text should fail")
	}
}

func TestPressKey(t *testing.T) {
	m := newMachine("linux", "xdotool")
	byName, _ := newTools(t, m)

	if res := run(t, byName["press_key"], map[string]any{"keys": "ctrl+s"}); res.IsError {
		t.Fatalf("press: %s", res.ForLLM)
	}
	if got := strings.Join(m.lastCall(), " "); got != "xdotool key --clearmodifiers -- ctrl+s" {
		t.Errorf("ran %q", got)
	}

	m.calls = nil
	if res := run(t, byName["press_key"], map[string]any{"keys": "Down", "repeat": 3}); res.IsError {
		t.Fatalf("repeat: %s", res.ForLLM)
	}
	if !m.ranWith("--repeat 3") {
		t.Errorf("ran %v", m.lastCall())
	}
	if res := run(t, byName["press_key"], map[string]any{"keys": "a", "repeat": 500}); !res.IsError {
		t.Error("repeat should be capped")
	}
	if res := run(t, byName["press_key"], map[string]any{"keys": "  "}); !res.IsError {
		t.Error("blank keys should fail")
	}
}

func TestClipboard(t *testing.T) {
	m := newMachine("linux", "xclip")
	m.outputs["xclip -selection"] = "copied text"
	byName, _ := newTools(t, m)

	res := run(t, byName["clipboard"], map[string]any{"action": "get"})
	if res.IsError || res.ForLLM != "copied text" {
		t.Fatalf("get = %+v", res)
	}

	res = run(t, byName["clipboard"], map[string]any{"action": "set", "text": "hello"})
	if res.IsError {
		t.Fatalf("set: %s", res.ForLLM)
	}
	last := m.calls[len(m.calls)-1]
	if last.stdin != "hello" || !strings.Contains(strings.Join(last.argv, " "), "-i") {
		t.Errorf("set ran %v with stdin %q", last.argv, last.stdin)
	}

	if res := run(t, byName["clipboard"], map[string]any{"action": "set"}); !res.IsError {
		t.Error("set without text should fail")
	}
	if res := run(t, byName["clipboard"], map[string]any{"action": "paste"}); !res.IsError {
		t.Error("unknown action should fail")
	}
}

func TestClipboardPrefersWaylandWhenNative(t *testing.T) {
	m := newMachine("linux", "wl-copy", "wl-paste")
	m.env = map[string]string{"WAYLAND_DISPLAY": "wayland-0"}
	m.outputs["wl-paste"] = "wayland text"
	byName, _ := newTools(t, m)

	if res := run(t, byName["clipboard"], map[string]any{"action": "get"}); res.ForLLM != "wayland text" {
		t.Fatalf("get = %+v", res)
	}
}

func TestNotifyAndOpen(t *testing.T) {
	m := newMachine("linux", "notify-send", "xdg-open")
	byName, ws := newTools(t, m)

	if res := run(t, byName["notify"], map[string]any{"title": "Done", "message": "build finished"}); res.IsError {
		t.Fatalf("notify: %s", res.ForLLM)
	}
	if !m.ranWith("notify-send -a factor -u normal -- Done build finished") {
		t.Errorf("ran %v", m.lastCall())
	}

	m.calls = nil
	if res := run(t, byName["open"], map[string]any{"target": "https://example.com"}); res.IsError {
		t.Fatalf("open url: %s", res.ForLLM)
	}
	if got := strings.Join(m.lastCall(), " "); got != "xdg-open https://example.com" {
		t.Errorf("ran %q", got)
	}

	// Local paths are workspace-resolved like every other file operation.
	if err := os.WriteFile(filepath.Join(ws, "note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := run(t, byName["open"], map[string]any{"target": "note.txt"}); res.IsError {
		t.Fatalf("open file: %s", res.ForLLM)
	}
	if !m.ranWith(filepath.Join(ws, "note.txt")) {
		t.Errorf("ran %v", m.lastCall())
	}
	if res := run(t, byName["open"], map[string]any{"target": "/etc/passwd"}); !res.IsError {
		t.Error("open escaped the workspace")
	}
}

func TestDesktopInfo(t *testing.T) {
	m := newMachine("linux", "xdotool", "wmctrl")
	m.env["XDG_CURRENT_DESKTOP"] = "XFCE"
	m.outputs["xdotool getdisplaygeometry"] = "1920 1080\n"
	m.outputs["wmctrl -l"] = wmctrlOutput
	byName, _ := newTools(t, m)

	res := run(t, byName["desktop_info"], map[string]any{})
	for _, want := range []string{"backend: x11", "desktop: XFCE", "screen:  1920x1080", "open windows: 3", "helpers missing"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("missing %q in:\n%s", want, res.ForLLM)
		}
	}
	if !strings.Contains(res.ForLLM, "scrot (screenshots)") {
		t.Errorf("missing helpers should name their purpose:\n%s", res.ForLLM)
	}
}

func TestScreenSizeFromXrandr(t *testing.T) {
	if w, h, ok := parseXrandr("eDP-1 connected primary 1366x768+0+0 (normal) 309mm x 174mm"); !ok || w != 1366 || h != 768 {
		t.Errorf("parseXrandr = %d, %d, %v", w, h, ok)
	}
	if _, _, ok := parseXrandr("Screen 0: minimum 320 x 200"); ok {
		t.Error("parseXrandr matched a non-display line")
	}
}

func TestHelperErrorsAreActionable(t *testing.T) {
	m := newMachine("linux") // bare X11 session, nothing installed
	byName, _ := newTools(t, m)
	for name, args := range map[string]map[string]any{
		"mouse":     {"action": "click"},
		"type_text": {"text": "hi"},
		"press_key": {"keys": "a"},
		"clipboard": {"action": "get"},
		"notify":    {"title": "t"},
		"open":      {"target": "https://x.dev"},
	} {
		res := run(t, byName[name], args)
		if !res.IsError {
			t.Errorf("%s succeeded without any helper installed", name)
			continue
		}
		if !strings.Contains(res.ForLLM, "install") {
			t.Errorf("%s error is not actionable: %s", name, res.ForLLM)
		}
	}
}

func TestRunnerErrorsSurface(t *testing.T) {
	m := newMachine("linux", "xdotool")
	m.errs["xdotool key"] = errors.New("xdotool: XTEST extension missing")
	byName, _ := newTools(t, m)
	res := run(t, byName["press_key"], map[string]any{"keys": "a"})
	if !res.IsError || !strings.Contains(res.ForLLM, "XTEST") {
		t.Fatalf("res = %+v", res)
	}
}

func TestToolSchemasAreWellFormed(t *testing.T) {
	m := newMachine("linux")
	byName, _ := newTools(t, m)
	if len(byName) != 10 {
		t.Fatalf("registered %d tools, want 10", len(byName))
	}
	for name, tool := range byName {
		if tool.Description() == "" {
			t.Errorf("%s has no description", name)
		}
		params := tool.Parameters()
		if params["type"] != "object" {
			t.Errorf("%s: schema type = %v", name, params["type"])
		}
		if _, ok := params["properties"].(map[string]any); !ok {
			t.Errorf("%s: no properties map", name)
		}
		// Required arguments must actually be declared as properties.
		props, _ := params["properties"].(map[string]any)
		req, _ := params["required"].([]any)
		for _, r := range req {
			if _, ok := props[r.(string)]; !ok {
				t.Errorf("%s: required %q is not a declared property", name, r)
			}
		}
	}
}

func TestMissingHelpersAndPackages(t *testing.T) {
	m := newMachine("linux", "xdotool", "xdg-open")
	env := m.Env()
	missing := MissingHelpers(env, NewController(env))
	pkgs := PackagesFor(missing, "apt")
	want := []string{"libnotify-bin", "scrot", "wmctrl", "xclip"}
	if strings.Join(pkgs, ",") != strings.Join(want, ",") {
		t.Errorf("apt packages = %v, want %v", pkgs, want)
	}
	if got := PackagesFor(missing, "pacman"); got[0] != "libnotify" {
		t.Errorf("pacman packages = %v", got)
	}
}

func TestHasDisplay(t *testing.T) {
	headless := newMachine("linux")
	headless.env = map[string]string{}
	if HasDisplay(headless.Env()) {
		t.Error("headless linux reported a display")
	}
	wayland := newMachine("linux")
	wayland.env = map[string]string{"WAYLAND_DISPLAY": "wayland-0"}
	if !HasDisplay(wayland.Env()) {
		t.Error("wayland session reported no display")
	}
	if !HasDisplay(newMachine("darwin").Env()) {
		t.Error("macOS always has a display")
	}
}

func TestFieldsN(t *testing.T) {
	got := fieldsN("a  b   c d e", 3)
	if strings.Join(got, "|") != "a|b|c d e" {
		t.Errorf("fieldsN = %v", got)
	}
	if got := fieldsN("   ", 3); len(got) != 0 {
		t.Errorf("blank line = %v", got)
	}
}
