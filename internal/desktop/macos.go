package desktop

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// macController drives macOS through osascript (System Events), screencapture,
// pbcopy/pbpaste and open. Window access requires the user to grant Factor's
// terminal Accessibility permission; the error text says so, because the
// system's own message ("not allowed assistive access") is easy to miss.
type macController struct{ env Env }

func (c *macController) Backend() string { return "macos" }

func (c *macController) Helpers() []Helper {
	return []Helper{
		{Bin: "osascript", Purpose: "windows, typing, notifications"},
		{Bin: "screencapture", Purpose: "screenshots"},
		{Bin: "pbcopy", Purpose: "clipboard"},
		{Bin: "cliclick", Purpose: "mouse control (brew install cliclick)"},
	}
}

func (c *macController) osa(ctx context.Context, script string) (string, error) {
	if !c.env.has("osascript") {
		return "", missingHelper("controlling the desktop", "osascript")
	}
	out, err := c.env.Run(ctx, "", "osascript", "-e", script)
	if err != nil && strings.Contains(err.Error(), "assistive access") {
		return out, fmt.Errorf("%w — grant your terminal Accessibility permission in System Settings → Privacy & Security → Accessibility", err)
	}
	return out, err
}

// asString quotes a Go string as an AppleScript literal.
func asString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

const macListScript = `set out to ""
tell application "System Events"
	repeat with p in (every process whose visible is true)
		set pid to unix id of p
		set pname to name of p
		try
			set idx to 0
			repeat with w in windows of p
				set idx to idx + 1
				set {wx, wy} to position of w
				set {ww, wh} to size of w
				set out to out & pid & ":" & idx & tab & pid & tab & pname & tab & wx & tab & wy & tab & ww & tab & wh & tab & (name of w) & linefeed
			end repeat
		end try
	end repeat
end tell
return out`

func (c *macController) ListWindows(ctx context.Context) ([]Window, error) {
	out, err := c.osa(ctx, macListScript)
	if err != nil {
		return nil, err
	}
	var wins []Window
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) < 8 {
			continue
		}
		wins = append(wins, Window{
			ID: f[0], PID: atoi(f[1]), App: f[2],
			X: atoi(f[3]), Y: atoi(f[4]), W: atoi(f[5]), H: atoi(f[6]),
			Title: f[7], HasGeom: true,
		})
	}
	return wins, nil
}

// macTarget splits the "pid:index" window id used on macOS.
func macTarget(w Window) (pid, index int, err error) {
	pidStr, idxStr, ok := strings.Cut(w.ID, ":")
	if !ok {
		return 0, 0, fmt.Errorf("macOS window ids look like \"pid:index\" (got %q) — call window_list first", w.ID)
	}
	pid, err1 := strconv.Atoi(pidStr)
	index, err2 := strconv.Atoi(idxStr)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("invalid macOS window id %q", w.ID)
	}
	return pid, index, nil
}

// macWindowScript wraps a body operating on `w` (the target window) and `p`
// (its process).
func macWindowScript(pid, index int, body string) string {
	return fmt.Sprintf(`tell application "System Events"
	set p to first process whose unix id is %d
	set w to window %d of p
	%s
end tell`, pid, index, body)
}

func (c *macController) withWindow(ctx context.Context, w Window, body string) error {
	pid, index, err := macTarget(w)
	if err != nil {
		return err
	}
	_, err = c.osa(ctx, macWindowScript(pid, index, body))
	return err
}

func (c *macController) ActiveWindow(ctx context.Context) (Window, error) {
	out, err := c.osa(ctx, `tell application "System Events"
	set p to first process whose frontmost is true
	return ((unix id of p) as text) & ":1" & tab & (name of p)
end tell`)
	if err != nil {
		return Window{}, err
	}
	f := strings.Split(strings.TrimSpace(out), "\t")
	if len(f) < 2 {
		return Window{}, fmt.Errorf("could not determine the active window")
	}
	return Window{ID: f[0], App: f[1], Title: f[1]}, nil
}

func (c *macController) Focus(ctx context.Context, w Window) error {
	return c.withWindow(ctx, w, `set frontmost of p to true
	try
		perform action "AXRaise" of w
	end try`)
}

func (c *macController) CloseWindow(ctx context.Context, w Window) error {
	return c.withWindow(ctx, w, `click (first button of w whose subrole is "AXCloseButton")`)
}

func (c *macController) SetState(ctx context.Context, w Window, state string) error {
	switch state {
	case "minimize":
		return c.withWindow(ctx, w, `set value of attribute "AXMinimized" of w to true`)
	case "restore":
		return c.withWindow(ctx, w, `set value of attribute "AXMinimized" of w to false
	set frontmost of p to true`)
	case "maximize":
		width, height, err := c.ScreenSize(ctx)
		if err != nil {
			return err
		}
		return c.withWindow(ctx, w, fmt.Sprintf("set position of w to {0, 0}\n\tset size of w to {%d, %d}", width, height))
	case "fullscreen":
		return c.withWindow(ctx, w, `set value of attribute "AXFullScreen" of w to true`)
	case "unfullscreen":
		return c.withWindow(ctx, w, `set value of attribute "AXFullScreen" of w to false`)
	}
	return fmt.Errorf("unknown window state %q", state)
}

func (c *macController) MoveResize(ctx context.Context, w Window, g Geometry) error {
	var body []string
	if g.HasPos {
		body = append(body, fmt.Sprintf("set position of w to {%d, %d}", g.X, g.Y))
	}
	if g.HasSize {
		body = append(body, fmt.Sprintf("set size of w to {%d, %d}", g.W, g.H))
	}
	return c.withWindow(ctx, w, strings.Join(body, "\n\t"))
}

func (c *macController) Screenshot(ctx context.Context, path string, shot Shot) error {
	if !c.env.has("screencapture") {
		return missingHelper("taking a screenshot", "screencapture")
	}
	argv := []string{"screencapture", "-x"}
	switch shot.Mode {
	case "region":
		argv = append(argv, "-R", fmt.Sprintf("%d,%d,%d,%d", shot.Region.X, shot.Region.Y, shot.Region.W, shot.Region.H))
	case "window":
		// Focus the window and capture its screen area instead: screencapture
		// addresses windows by CGWindowID, which System Events does not expose.
		if err := c.Focus(ctx, shot.Window); err != nil {
			return err
		}
		if shot.Window.HasGeom {
			argv = append(argv, "-R", fmt.Sprintf("%d,%d,%d,%d",
				shot.Window.X, shot.Window.Y, shot.Window.W, shot.Window.H))
		}
	}
	_, err := c.env.Run(ctx, "", append(argv, path)...)
	return err
}

func (c *macController) MoveMouse(ctx context.Context, x, y int) error {
	if !c.env.has("cliclick") {
		return missingHelper("moving the mouse", "cliclick")
	}
	_, err := c.env.Run(ctx, "", "cliclick", fmt.Sprintf("m:%d,%d", x, y))
	return err
}

func (c *macController) Click(ctx context.Context, button string, count int, at *Point) error {
	if !c.env.has("cliclick") {
		return missingHelper("clicking", "cliclick")
	}
	verb := map[string]string{"left": "c", "right": "rc", "middle": "c"}[button]
	if verb == "" {
		return fmt.Errorf("unknown mouse button %q", button)
	}
	if count > 1 && button == "left" {
		verb = "dc"
	}
	pos := "."
	if at != nil {
		pos = fmt.Sprintf("%d,%d", at.X, at.Y)
	}
	_, err := c.env.Run(ctx, "", "cliclick", verb+":"+pos)
	return err
}

func (c *macController) TypeText(ctx context.Context, text string, _ int) error {
	_, err := c.osa(ctx, `tell application "System Events" to keystroke `+asString(text))
	return err
}

// macModifiers maps portable modifier names to AppleScript's.
var macModifiers = map[string]string{
	"cmd": "command down", "command": "command down", "super": "command down",
	"ctrl": "control down", "control": "control down",
	"alt": "option down", "option": "option down",
	"shift": "shift down",
}

// macKeyCodes covers the non-printable keys people actually script.
var macKeyCodes = map[string]int{
	"return": 36, "enter": 36, "tab": 48, "space": 49, "delete": 51, "backspace": 51,
	"escape": 53, "esc": 53, "left": 123, "right": 124, "down": 125, "up": 126,
	"home": 115, "end": 119, "pageup": 116, "pagedown": 121, "forwarddelete": 117,
	"f1": 122, "f2": 120, "f3": 99, "f4": 118, "f5": 96, "f6": 97, "f7": 98, "f8": 100,
	"f9": 101, "f10": 109, "f11": 103, "f12": 111,
}

func (c *macController) PressKey(ctx context.Context, keys string, repeat int) error {
	parts := strings.Split(strings.ToLower(keys), "+")
	key := parts[len(parts)-1]
	var mods []string
	for _, m := range parts[:len(parts)-1] {
		as, ok := macModifiers[m]
		if !ok {
			return fmt.Errorf("unknown modifier %q", m)
		}
		mods = append(mods, as)
	}
	var action string
	if code, ok := macKeyCodes[key]; ok {
		action = fmt.Sprintf("key code %d", code)
	} else if len([]rune(key)) == 1 {
		action = "keystroke " + asString(key)
	} else {
		return fmt.Errorf("unknown key %q", key)
	}
	if len(mods) > 0 {
		action += " using {" + strings.Join(mods, ", ") + "}"
	}
	if repeat < 1 {
		repeat = 1
	}
	for i := 0; i < repeat; i++ {
		if _, err := c.osa(ctx, `tell application "System Events" to `+action); err != nil {
			return err
		}
	}
	return nil
}

func (c *macController) ClipboardGet(ctx context.Context) (string, error) {
	if !c.env.has("pbpaste") {
		return "", missingHelper("reading the clipboard", "pbpaste")
	}
	return c.env.Run(ctx, "", "pbpaste")
}

func (c *macController) ClipboardSet(ctx context.Context, text string) error {
	if !c.env.has("pbcopy") {
		return missingHelper("writing the clipboard", "pbcopy")
	}
	_, err := c.env.Run(ctx, text, "pbcopy")
	return err
}

func (c *macController) Notify(ctx context.Context, title, body, _ string) error {
	_, err := c.osa(ctx, fmt.Sprintf("display notification %s with title %s", asString(body), asString(title)))
	return err
}

func (c *macController) Open(ctx context.Context, target string) error {
	if !c.env.has("open") {
		return missingHelper("opening a file or URL", "open")
	}
	_, err := c.env.Run(ctx, "", "open", target)
	return err
}

func (c *macController) ScreenSize(ctx context.Context) (int, int, error) {
	out, err := c.osa(ctx, `tell application "Finder" to get bounds of window of desktop`)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Split(strings.TrimSpace(out), ", ")
	if len(f) != 4 {
		return 0, 0, fmt.Errorf("unexpected desktop bounds %q", strings.TrimSpace(out))
	}
	return atoi(f[2]), atoi(f[3]), nil
}
