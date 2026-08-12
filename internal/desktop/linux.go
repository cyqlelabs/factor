package desktop

import (
	"context"
	"fmt"
	"strings"
)

// linuxController drives an X11 (or XWayland) session with the classic
// helper set, falling back to Wayland-native tools where they exist. Nothing
// is assumed to be installed: every operation picks the best helper present
// and says exactly what to install when none is.
type linuxController struct{ env Env }

func (c *linuxController) wayland() bool {
	return c.env.env("WAYLAND_DISPLAY") != "" && c.env.env("DISPLAY") == ""
}

func (c *linuxController) Backend() string {
	if c.wayland() {
		return "wayland"
	}
	return "x11"
}

func (c *linuxController) Helpers() []Helper {
	if c.wayland() {
		return []Helper{
			{Bin: "grim", Purpose: "screenshots"},
			{Bin: "wl-copy", Purpose: "clipboard", Packages: map[string]string{"apt": "wl-clipboard", "dnf": "wl-clipboard", "pacman": "wl-clipboard", "apk": "wl-clipboard", "xbps": "wl-clipboard"}},
			{Bin: "wtype", Purpose: "typing and key presses"},
			{Bin: "notify-send", Purpose: "desktop notifications", Packages: map[string]string{"apt": "libnotify-bin", "dnf": "libnotify", "pacman": "libnotify", "apk": "libnotify", "xbps": "libnotify"}},
			{Bin: "xdg-open", Purpose: "opening files and URLs", Packages: map[string]string{"apt": "xdg-utils", "dnf": "xdg-utils", "pacman": "xdg-utils", "apk": "xdg-utils", "xbps": "xdg-utils"}},
		}
	}
	return []Helper{
		{Bin: "xdotool", Purpose: "mouse, keyboard, window focus"},
		{Bin: "wmctrl", Purpose: "window listing and window states"},
		{Bin: "scrot", Purpose: "screenshots"},
		{Bin: "xclip", Purpose: "clipboard"},
		{Bin: "notify-send", Purpose: "desktop notifications", Packages: map[string]string{"apt": "libnotify-bin", "dnf": "libnotify", "pacman": "libnotify", "apk": "libnotify", "xbps": "libnotify"}},
		{Bin: "xdg-open", Purpose: "opening files and URLs", Packages: map[string]string{"apt": "xdg-utils", "dnf": "xdg-utils", "pacman": "xdg-utils", "apk": "xdg-utils", "xbps": "xdg-utils"}},
	}
}

func (c *linuxController) ListWindows(ctx context.Context) ([]Window, error) {
	switch {
	case c.env.has("wmctrl"):
		out, err := c.env.Run(ctx, "", "wmctrl", "-l", "-G", "-p", "-x")
		if err != nil {
			return nil, err
		}
		return parseWmctrl(out), nil
	case c.env.has("xdotool"):
		return c.listWithXdotool(ctx)
	}
	return nil, missingHelper("listing windows", "wmctrl", "xdotool")
}

// parseWmctrl reads `wmctrl -lGpx` rows:
//
//	0x03c00003  0 4242   0    27   1920 1053 code.Code   host  factor — Code
//	id          dsk pid  x    y    w    h    WM_CLASS    host  title
func parseWmctrl(out string) []Window {
	var wins []Window
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := fieldsN(line, 10)
		if len(f) < 9 {
			continue
		}
		app := f[7]
		if i := strings.LastIndex(app, "."); i >= 0 && i+1 < len(app) {
			app = app[i+1:] // WM_CLASS is "instance.Class"
		}
		w := Window{
			ID: f[0], Desktop: f[1], PID: atoi(f[2]),
			X: atoi(f[3]), Y: atoi(f[4]), W: atoi(f[5]), H: atoi(f[6]),
			App: app, HasGeom: true,
		}
		if len(f) >= 10 {
			w.Title = strings.TrimSpace(f[9])
		}
		wins = append(wins, w)
	}
	return wins
}

const maxXdotoolWindows = 60

func (c *linuxController) listWithXdotool(ctx context.Context) ([]Window, error) {
	out, err := c.env.Run(ctx, "", "xdotool", "search", "--onlyvisible", "--name", ".")
	if err != nil {
		return nil, err
	}
	var wins []Window
	for _, id := range strings.Fields(out) {
		if len(wins) >= maxXdotoolWindows {
			break
		}
		name, err := c.env.Run(ctx, "", "xdotool", "getwindowname", id)
		if err != nil {
			continue // windows disappear between the search and the query
		}
		w := Window{ID: id, Title: strings.TrimSpace(name)}
		if pid, err := c.env.Run(ctx, "", "xdotool", "getwindowpid", id); err == nil {
			w.PID = atoi(strings.TrimSpace(pid))
		}
		wins = append(wins, w)
	}
	return wins, nil
}

func (c *linuxController) ActiveWindow(ctx context.Context) (Window, error) {
	if !c.env.has("xdotool") {
		return Window{}, missingHelper("finding the active window", "xdotool")
	}
	out, err := c.env.Run(ctx, "", "xdotool", "getactivewindow")
	if err != nil {
		return Window{}, err
	}
	id := strings.TrimSpace(out)
	w := Window{ID: id}
	if name, err := c.env.Run(ctx, "", "xdotool", "getwindowname", id); err == nil {
		w.Title = strings.TrimSpace(name)
	}
	return w, nil
}

func (c *linuxController) Focus(ctx context.Context, w Window) error {
	switch {
	case c.env.has("wmctrl"):
		_, err := c.env.Run(ctx, "", "wmctrl", "-i", "-a", w.ID)
		return err
	case c.env.has("xdotool"):
		_, err := c.env.Run(ctx, "", "xdotool", "windowactivate", "--sync", w.ID)
		return err
	}
	return missingHelper("focusing a window", "wmctrl", "xdotool")
}

func (c *linuxController) CloseWindow(ctx context.Context, w Window) error {
	switch {
	case c.env.has("wmctrl"):
		_, err := c.env.Run(ctx, "", "wmctrl", "-i", "-c", w.ID) // polite WM_DELETE_WINDOW
		return err
	case c.env.has("xdotool"):
		_, err := c.env.Run(ctx, "", "xdotool", "windowclose", w.ID)
		return err
	}
	return missingHelper("closing a window", "wmctrl", "xdotool")
}

var wmctrlStates = map[string][]string{
	"maximize":     {"add", "maximized_vert,maximized_horz"},
	"restore":      {"remove", "maximized_vert,maximized_horz"},
	"fullscreen":   {"add", "fullscreen"},
	"unfullscreen": {"remove", "fullscreen"},
	"shade":        {"add", "shaded"},
	"above":        {"add", "above"},
}

func (c *linuxController) SetState(ctx context.Context, w Window, state string) error {
	if state == "minimize" {
		if !c.env.has("xdotool") {
			return missingHelper("minimizing a window", "xdotool")
		}
		_, err := c.env.Run(ctx, "", "xdotool", "windowminimize", w.ID)
		return err
	}
	spec, ok := wmctrlStates[state]
	if !ok {
		return fmt.Errorf("unknown window state %q", state)
	}
	if !c.env.has("wmctrl") {
		return missingHelper("changing window state", "wmctrl")
	}
	if _, err := c.env.Run(ctx, "", "wmctrl", "-i", "-r", w.ID, "-b", spec[0]+","+spec[1]); err != nil {
		return err
	}
	if state == "restore" && c.env.has("xdotool") {
		// A minimized window stays hidden after un-maximizing; raise it too.
		_, _ = c.env.Run(ctx, "", "xdotool", "windowactivate", w.ID)
	}
	return nil
}

func (c *linuxController) MoveResize(ctx context.Context, w Window, g Geometry) error {
	if c.env.has("wmctrl") {
		// wmctrl refuses to move maximized windows; drop the state first.
		_, _ = c.env.Run(ctx, "", "wmctrl", "-i", "-r", w.ID, "-b", "remove,maximized_vert,maximized_horz")
		x, y, width, height := -1, -1, -1, -1
		if g.HasPos {
			x, y = g.X, g.Y
		}
		if g.HasSize {
			width, height = g.W, g.H
		}
		spec := fmt.Sprintf("0,%d,%d,%d,%d", x, y, width, height)
		_, err := c.env.Run(ctx, "", "wmctrl", "-i", "-r", w.ID, "-e", spec)
		return err
	}
	if !c.env.has("xdotool") {
		return missingHelper("moving a window", "wmctrl", "xdotool")
	}
	if g.HasPos {
		if _, err := c.env.Run(ctx, "", "xdotool", "windowmove", w.ID, itoa(g.X), itoa(g.Y)); err != nil {
			return err
		}
	}
	if g.HasSize {
		if _, err := c.env.Run(ctx, "", "xdotool", "windowsize", w.ID, itoa(g.W), itoa(g.H)); err != nil {
			return err
		}
	}
	return nil
}

func (c *linuxController) Screenshot(ctx context.Context, path string, shot Shot) error {
	region := shot.Region
	switch {
	case c.env.has("maim"):
		argv := []string{"maim", "--hidecursor"}
		switch shot.Mode {
		case "window":
			argv = append(argv, "-i", shot.Window.ID)
		case "region":
			argv = append(argv, "-g", fmt.Sprintf("%dx%d+%d+%d", region.W, region.H, region.X, region.Y))
		}
		_, err := c.env.Run(ctx, "", append(argv, path)...)
		return err
	case c.env.has("scrot"):
		argv := []string{"scrot", "--overwrite"}
		switch shot.Mode {
		case "window":
			// scrot can only capture the focused window: focus it first.
			if err := c.Focus(ctx, shot.Window); err != nil {
				return err
			}
			argv = append(argv, "-u")
		case "region":
			argv = append(argv, "-a", fmt.Sprintf("%d,%d,%d,%d", region.X, region.Y, region.W, region.H))
		}
		_, err := c.env.Run(ctx, "", append(argv, path)...)
		return err
	case c.env.has("grim"):
		argv := []string{"grim"}
		switch shot.Mode {
		case "region":
			argv = append(argv, "-g", fmt.Sprintf("%d,%d %dx%d", region.X, region.Y, region.W, region.H))
		case "window":
			return unsupported("window screenshots", "wayland/grim", "capture the full screen or a region instead")
		}
		_, err := c.env.Run(ctx, "", append(argv, path)...)
		return err
	case c.env.has("import"):
		argv := []string{"import", "-silent"}
		switch shot.Mode {
		case "window":
			argv = append(argv, "-window", shot.Window.ID)
		case "region":
			argv = append(argv, "-window", "root", "-crop",
				fmt.Sprintf("%dx%d+%d+%d", region.W, region.H, region.X, region.Y))
		default:
			argv = append(argv, "-window", "root")
		}
		_, err := c.env.Run(ctx, "", append(argv, path)...)
		return err
	case c.env.has("gnome-screenshot"):
		argv := []string{"gnome-screenshot", "-f", path}
		if shot.Mode == "window" {
			argv = append(argv, "-w")
		}
		_, err := c.env.Run(ctx, "", argv...)
		return err
	}
	return missingHelper("taking a screenshot", "maim", "scrot", "grim", "import", "gnome-screenshot")
}

func (c *linuxController) MoveMouse(ctx context.Context, x, y int) error {
	if !c.env.has("xdotool") {
		return missingHelper("moving the mouse", "xdotool")
	}
	_, err := c.env.Run(ctx, "", "xdotool", "mousemove", itoa(x), itoa(y))
	return err
}

// xButtons maps friendly names to X11 button numbers.
var xButtons = map[string]string{
	"left": "1", "middle": "2", "right": "3",
	"scroll_up": "4", "scroll_down": "5",
}

func (c *linuxController) Click(ctx context.Context, button string, count int, at *Point) error {
	if !c.env.has("xdotool") {
		return missingHelper("clicking", "xdotool")
	}
	num, ok := xButtons[button]
	if !ok {
		return fmt.Errorf("unknown mouse button %q", button)
	}
	if at != nil {
		if _, err := c.env.Run(ctx, "", "xdotool", "mousemove", itoa(at.X), itoa(at.Y)); err != nil {
			return err
		}
	}
	argv := []string{"xdotool", "click"}
	if count > 1 {
		argv = append(argv, "--repeat", itoa(count), "--delay", "80")
	}
	_, err := c.env.Run(ctx, "", append(argv, num)...)
	return err
}

func (c *linuxController) TypeText(ctx context.Context, text string, delayMs int) error {
	switch {
	case c.env.has("xdotool"):
		// "--" stops flag parsing so text beginning with "-" types literally.
		_, err := c.env.Run(ctx, "", "xdotool", "type", "--clearmodifiers", "--delay", itoa(delayMs), "--", text)
		return err
	case c.env.has("wtype"):
		_, err := c.env.Run(ctx, "", "wtype", "--", text)
		return err
	}
	return missingHelper("typing text", "xdotool", "wtype")
}

func (c *linuxController) PressKey(ctx context.Context, keys string, repeat int) error {
	switch {
	case c.env.has("xdotool"):
		argv := []string{"xdotool", "key", "--clearmodifiers"}
		if repeat > 1 {
			argv = append(argv, "--repeat", itoa(repeat), "--repeat-delay", "40")
		}
		_, err := c.env.Run(ctx, "", append(argv, "--", keys)...)
		return err
	case c.env.has("wtype"):
		argv := []string{"wtype"}
		for _, part := range strings.Split(keys, "+") {
			argv = append(argv, "-k", part)
		}
		_, err := c.env.Run(ctx, "", argv...)
		return err
	}
	return missingHelper("pressing keys", "xdotool", "wtype")
}

func (c *linuxController) ClipboardGet(ctx context.Context) (string, error) {
	switch {
	case c.env.has("wl-paste"):
		return c.env.Run(ctx, "", "wl-paste", "--no-newline")
	case c.env.has("xclip"):
		return c.env.Run(ctx, "", "xclip", "-selection", "clipboard", "-o")
	case c.env.has("xsel"):
		return c.env.Run(ctx, "", "xsel", "--clipboard", "--output")
	}
	return "", missingHelper("reading the clipboard", "xclip", "xsel", "wl-paste")
}

func (c *linuxController) ClipboardSet(ctx context.Context, text string) error {
	switch {
	case c.env.has("wl-copy"):
		_, err := c.env.Run(ctx, text, "wl-copy")
		return err
	case c.env.has("xclip"):
		_, err := c.env.Run(ctx, text, "xclip", "-selection", "clipboard", "-i")
		return err
	case c.env.has("xsel"):
		_, err := c.env.Run(ctx, text, "xsel", "--clipboard", "--input")
		return err
	}
	return missingHelper("writing the clipboard", "xclip", "xsel", "wl-copy")
}

func (c *linuxController) Notify(ctx context.Context, title, body, urgency string) error {
	if !c.env.has("notify-send") {
		return missingHelper("sending a notification", "notify-send")
	}
	if urgency == "" {
		urgency = "normal"
	}
	_, err := c.env.Run(ctx, "", "notify-send", "-a", "factor", "-u", urgency, "--", title, body)
	return err
}

func (c *linuxController) Open(ctx context.Context, target string) error {
	bin, ok := c.env.first("xdg-open", "gio", "exo-open", "gnome-open")
	if !ok {
		return missingHelper("opening a file or URL", "xdg-open")
	}
	argv := []string{bin}
	if bin == "gio" {
		argv = append(argv, "open")
	}
	_, err := c.env.Run(ctx, "", append(argv, target)...)
	return err
}

func (c *linuxController) ScreenSize(ctx context.Context) (int, int, error) {
	if c.env.has("xdotool") {
		out, err := c.env.Run(ctx, "", "xdotool", "getdisplaygeometry")
		if err != nil {
			return 0, 0, err
		}
		f := strings.Fields(out)
		if len(f) == 2 {
			return atoi(f[0]), atoi(f[1]), nil
		}
		return 0, 0, fmt.Errorf("unexpected xdotool output %q", strings.TrimSpace(out))
	}
	if c.env.has("xrandr") {
		out, err := c.env.Run(ctx, "", "xrandr")
		if err != nil {
			return 0, 0, err
		}
		if w, h, ok := parseXrandr(out); ok {
			return w, h, nil
		}
		return 0, 0, fmt.Errorf("could not parse xrandr output")
	}
	return 0, 0, missingHelper("reading the screen size", "xdotool", "xrandr")
}

// parseXrandr pulls the resolution off the connected+primary display line:
//
//	eDP-1 connected primary 1920x1080+0+0 (normal left inverted ...) 344mm x 194mm
func parseXrandr(out string) (int, int, bool) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, " connected") {
			continue
		}
		for _, field := range strings.Fields(line) {
			geom := field
			if i := strings.IndexByte(geom, '+'); i > 0 {
				geom = geom[:i]
			}
			w, h, ok := strings.Cut(geom, "x")
			if !ok || atoi(w) == 0 || atoi(h) == 0 {
				continue
			}
			return atoi(w), atoi(h), true
		}
	}
	return 0, 0, false
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
