package desktop

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/decision"
	"github.com/cyqlelabs/factor/internal/tools"
)

// NewTools builds the desktop arsenal. screenshotDir receives screenshots
// when the caller does not name a path; guard keeps written files inside the
// workspace like every other file-producing tool.
func NewTools(env Env, guard *tools.PathGuard, screenshotDir string, opts ...Option) []tools.Tool {
	c := NewController(env)
	var access Accessor
	if env.Accessibility {
		access = NewAccessor(env, c)
	}
	return newToolsWith(env, c, access, guard, screenshotDir, opts...)
}

// newToolsWith builds the tools over a given controller and accessibility
// backend, which is the seam a test scripts both through.
func newToolsWith(env Env, c Controller, access Accessor, guard *tools.PathGuard, screenshotDir string, opts ...Option) []tools.Tool {
	d := &deps{env: env, ctl: c, guard: guard, shotDir: screenshotDir, vision: newVisionStore(), access: access}
	for _, o := range opts {
		o(d)
	}
	set := []tools.Tool{
		&windowListTool{d},
		&windowControlTool{d},
		&screenshotTool{d},
		&screenViewTool{d},
		&screenZoomTool{d},
		&mouseTool{d},
		&typeTextTool{d},
		&pressKeyTool{d},
		&clipboardTool{d},
		&notifyTool{d},
		&openTool{d},
		&desktopInfoTool{deps: d},
	}
	// The executor is mounted only where it can do something: a tree to
	// read, and a decider allowed to act on what it decides.
	if d.access != nil && d.decider.Active() {
		set = append(set, &runTool{d})
	}
	return set
}

type deps struct {
	env     Env
	ctl     Controller
	guard   *tools.PathGuard
	shotDir string
	vision  *visionStore
	// access reads the accessibility tree; nil where there is none to read,
	// and every tool then behaves exactly as the grid alone would.
	access Accessor
	// decider and text drive desktop_run; nil leaves it unmounted.
	decider *decision.Decider
	text    TextWriter
}

// resolveWindow turns a tool argument into a window: "active", a window id,
// or a case-insensitive substring of the title or application name.
func (d *deps) resolveWindow(ctx context.Context, target string) (Window, error) {
	target = strings.TrimSpace(target)
	if target == "" || strings.EqualFold(target, "active") || strings.EqualFold(target, "focused") {
		return d.ctl.ActiveWindow(ctx)
	}
	wins, err := d.ctl.ListWindows(ctx)
	if err != nil {
		return Window{}, err
	}
	for _, w := range wins {
		if strings.EqualFold(w.ID, target) {
			return w, nil
		}
	}
	var matches []Window
	needle := strings.ToLower(target)
	for _, w := range wins {
		if strings.Contains(strings.ToLower(w.Title), needle) || strings.Contains(strings.ToLower(w.App), needle) {
			matches = append(matches, w)
		}
	}
	switch len(matches) {
	case 0:
		return Window{}, fmt.Errorf("no window matches %q — call window_list to see what is open", target)
	case 1:
		return matches[0], nil
	}
	var titles []string
	for _, m := range matches {
		titles = append(titles, fmt.Sprintf("%s (%s)", m.Title, m.ID))
	}
	return Window{}, fmt.Errorf("%q matches %d windows: %s — pass the id instead",
		target, len(matches), strings.Join(titles, "; "))
}

const windowArgDoc = `Window id from window_list, a case-insensitive part of its title or app name, or "active" for the focused window.`

// ---- window_list -----------------------------------------------------------

type windowListTool struct{ *deps }

func (t *windowListTool) Name() string { return "window_list" }
func (t *windowListTool) Description() string {
	return "List the open windows on the user's desktop with their ids, titles, applications, geometry and pids. Use it before window_control or screenshots of a specific window."
}
func (t *windowListTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"filter": map[string]any{"type": "string", "description": "Only show windows whose title or app contains this text"},
		},
	}
}

func (t *windowListTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	wins, err := t.ctl.ListWindows(ctx)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	filter := strings.ToLower(tools.StringArg(args, "filter"))
	var lines []string
	for _, w := range wins {
		if filter != "" && !strings.Contains(strings.ToLower(w.Title+" "+w.App), filter) {
			continue
		}
		lines = append(lines, w.String())
	}
	if len(lines) == 0 {
		if filter != "" {
			return tools.Textf("No window matches %q (%d open).", filter, len(wins))
		}
		return tools.Text("No windows are open.")
	}
	return tools.Textf("%d window(s):\n%s", len(lines), strings.Join(lines, "\n"))
}

// ---- window_control --------------------------------------------------------

type windowControlTool struct{ *deps }

func (t *windowControlTool) Name() string { return "window_control" }
func (t *windowControlTool) Description() string {
	return "Act on a desktop window: focus, close, minimize, maximize, restore, fullscreen, unfullscreen, or move (position and/or size, in pixels)."
}
func (t *windowControlTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{"type": "string", "description": "What to do to the window; move also needs at least one of x, y, width, height", "enum": []any{
				"focus", "close", "minimize", "maximize", "restore", "fullscreen", "unfullscreen", "move"}},
			"window": map[string]any{"type": "string", "description": windowArgDoc},
			"x":      map[string]any{"type": "integer", "description": "move: left edge"},
			"y":      map[string]any{"type": "integer", "description": "move: top edge"},
			"width":  map[string]any{"type": "integer", "description": "move: new width"},
			"height": map[string]any{"type": "integer", "description": "move: new height"},
		},
		"required": []any{"action", "window"},
	}
}

func (t *windowControlTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	action := tools.StringArg(args, "action")
	w, err := t.resolveWindow(ctx, tools.StringArg(args, "window"))
	if err != nil {
		return tools.Errorf("%v", err)
	}
	label := w.Title
	if label == "" {
		label = w.ID
	}
	switch action {
	case "focus":
		err = t.ctl.Focus(ctx, w)
	case "close":
		err = t.ctl.CloseWindow(ctx, w)
	case "minimize", "maximize", "restore", "fullscreen", "unfullscreen":
		err = t.ctl.SetState(ctx, w, action)
	case "move":
		g := Geometry{}
		if _, ok := args["x"]; ok {
			g.X, g.Y, g.HasPos = tools.IntArg(args, "x", 0), tools.IntArg(args, "y", 0), true
		}
		if _, ok := args["width"]; ok {
			g.W, g.H, g.HasSize = tools.IntArg(args, "width", 0), tools.IntArg(args, "height", 0), true
		}
		if !g.HasPos && !g.HasSize {
			return tools.Errorf("move needs x/y and/or width/height")
		}
		err = t.ctl.MoveResize(ctx, w, g)
	default:
		return tools.Errorf("unknown action %q", action)
	}
	if err != nil {
		return tools.Errorf("%s %s: %v", action, label, err)
	}
	return tools.Textf("%s: %s", action, label)
}

// ---- screenshot ------------------------------------------------------------

type screenshotTool struct{ *deps }

func (t *screenshotTool) Name() string { return "screenshot" }
func (t *screenshotTool) Description() string {
	return "Capture the screen, one window, or a rectangular region to a PNG file in the workspace and return its path. Handy for showing the user what you see or keeping a record; you cannot read the image back yourself unless the model supports vision."
}
func (t *screenshotTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{"type": "string", "enum": []any{"screen", "window", "region"}, "description": "What to capture (default screen); region needs x, y, width, and height"},
			"window": map[string]any{"type": "string", "description": "target=window: " + windowArgDoc},
			"x":      map[string]any{"type": "integer", "description": "target=region: left edge in absolute screen pixels from the top-left"},
			"y":      map[string]any{"type": "integer", "description": "target=region: top edge in absolute screen pixels from the top-left"},
			"width":  map[string]any{"type": "integer", "description": "target=region: width in pixels"},
			"height": map[string]any{"type": "integer", "description": "target=region: height in pixels"},
			"path":   map[string]any{"type": "string", "description": "Where to save it (default: screenshots/<timestamp>.png in the workspace)"},
		},
	}
}

func (t *screenshotTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	mode := tools.StringArg(args, "target")
	if mode == "" {
		mode = "screen"
	}
	shot := Shot{Mode: mode}
	switch mode {
	case "screen":
	case "window":
		w, err := t.resolveWindow(ctx, tools.StringArg(args, "window"))
		if err != nil {
			return tools.Errorf("%v", err)
		}
		shot.Window = w
	case "region":
		width, height := tools.IntArg(args, "width", 0), tools.IntArg(args, "height", 0)
		if width <= 0 || height <= 0 {
			return tools.Errorf("region screenshots need width and height")
		}
		shot.Region = Geometry{
			X: tools.IntArg(args, "x", 0), Y: tools.IntArg(args, "y", 0),
			W: width, H: height, HasPos: true, HasSize: true,
		}
	default:
		return tools.Errorf("unknown target %q", mode)
	}

	path := tools.StringArg(args, "path")
	if path == "" {
		path = filepath.Join(t.shotDir, fmt.Sprintf("screenshot-%s.png", time.Now().Format("20060102-150405")))
	}
	resolved, err := t.guard.CheckWrite(path)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return tools.Errorf("%v", err)
	}
	if err := t.ctl.Screenshot(ctx, resolved, shot); err != nil {
		return tools.Errorf("screenshot: %v", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return tools.Errorf("the screenshot tool reported success but %s does not exist", resolved)
	}
	return &tools.Result{
		ForLLM:  fmt.Sprintf("Saved %s screenshot to %s (%d KB).%s", mode, resolved, info.Size()/1024, t.blankFile(resolved)),
		ForUser: "📸 " + resolved,
	}
}

// blankFile reads a saved screenshot back and says so when nothing is drawn
// in it. A flat frame is a small file, and from its size alone it looks like
// any other capture: the agent sent one as "the desktop" and then spent six
// tool calls reconstructing the screen by hand.
func (t *screenshotTool) blankFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return ""
	}
	if c, flat := flatColor(toRGBA(img)); flat {
		return blankNote(t.env, c)
	}
	return ""
}

// ---- mouse -----------------------------------------------------------------

type mouseTool struct{ *deps }

func (t *mouseTool) Name() string { return "mouse" }
func (t *mouseTool) Description() string {
	d := "Control the pointer: move it, or click at a position (or wherever it currently is). Target either a grid cell from the latest screen_view/screen_zoom image (cell=D4), or absolute screen pixels from the top-left (x/y); desktop_info reports the screen size. To click something in a web page, use browser_click instead: it addresses the element rather than a spot on the glass, so it still lands when the window moves or the user takes the mouse."
	if t.access != nil {
		d += " A control screen_view listed from the accessibility tree is better targeted by element=e3: a click is performed by the application itself without moving the pointer, and nothing is sent if the control has changed since."
	}
	return d
}
func (t *mouseTool) Parameters() map[string]any {
	props := map[string]any{
		"action": map[string]any{"type": "string", "enum": []any{"move", "click", "double_click", "right_click", "middle_click", "scroll_up", "scroll_down"}, "description": "Pointer action to perform at element, cell or x/y, or at the current position when all are omitted"},
		"cell":   map[string]any{"type": "string", "description": "Grid cell on the latest screen_view or screen_zoom image, e.g. D4 (or f7:D4 to name which image); acts at the cell's center. Zoom first for small targets"},
		"x":      map[string]any{"type": "integer", "description": "Absolute screen pixels from the left edge; omit cell and x/y to act where the pointer already is"},
		"y":      map[string]any{"type": "integer", "description": "Absolute screen pixels from the top edge; omit cell and x/y to act where the pointer already is"},
	}
	if t.access != nil {
		props["element"] = map[string]any{"type": "string", "description": elementArgDoc}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   []any{"action"},
	}
}

func (t *mouseTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	action := tools.StringArg(args, "action")
	_, hasX := args["x"]
	_, hasY := args["y"]
	if ref := strings.TrimSpace(tools.StringArg(args, "element")); ref != "" {
		if hasX || hasY || strings.TrimSpace(tools.StringArg(args, "cell")) != "" {
			return tools.Errorf("give element, cell or x/y — one target, not several")
		}
		return t.actOnElement(ctx, action, ref)
	}
	var at *Point
	var resolved string
	if hasX && hasY {
		at = &Point{X: tools.IntArg(args, "x", 0), Y: tools.IntArg(args, "y", 0)}
	} else if hasX != hasY {
		return tools.Errorf("x and y must be given together")
	}
	if cell := strings.TrimSpace(tools.StringArg(args, "cell")); cell != "" {
		if at != nil {
			return tools.Errorf("give either cell or x/y, not both")
		}
		p, view, err := t.vision.forCtx(ctx).resolveCell(cell)
		if err != nil {
			return tools.Errorf("%v", err)
		}
		at = &Point{X: p.X, Y: p.Y}
		resolved = fmt.Sprintf(" (%s %s → %d,%d)", view, strings.ToUpper(cell), p.X, p.Y)
	}

	if action == "move" {
		if at == nil {
			return tools.Errorf("move needs cell or x and y")
		}
		if err := t.ctl.MoveMouse(ctx, at.X, at.Y); err != nil {
			return tools.Errorf("%v", err)
		}
		return tools.Textf("Moved the pointer to %d,%d%s.", at.X, at.Y, resolved)
	}

	button, count := "left", 1
	switch action {
	case "click":
	case "double_click":
		count = 2
	case "right_click":
		button = "right"
	case "middle_click":
		button = "middle"
	case "scroll_up", "scroll_down":
		button = action
	default:
		return tools.Errorf("unknown action %q", action)
	}
	if err := t.ctl.Click(ctx, button, count, at); err != nil {
		return tools.Errorf("%v", err)
	}
	where := "at the current position"
	if at != nil {
		where = fmt.Sprintf("at %d,%d%s", at.X, at.Y, resolved)
	}
	return tools.Textf("%s %s. The screen may have changed — screen_view to verify the result.", action, where)
}

// ---- type_text / press_key -------------------------------------------------

type typeTextTool struct{ *deps }

func (t *typeTextTool) Name() string { return "type_text" }
func (t *typeTextTool) Description() string {
	d := "Type text into whatever window has keyboard focus, as if the user typed it. Focus the target window first (window_control action=focus)."
	if t.access != nil {
		d += " With element=e3 (a field screen_view listed), the field's whole contents are replaced by text — written directly through the application where it allows, so no key is pressed and the result is read back."
	}
	return d
}
func (t *typeTextTool) Parameters() map[string]any {
	props := map[string]any{
		"text":     map[string]any{"type": "string", "description": "Literal text to type; this types characters, it does not send shortcuts (use press_key for those)"},
		"delay_ms": map[string]any{"type": "integer", "description": "Per-keystroke delay (default 12)"},
	}
	if t.access != nil {
		props["element"] = map[string]any{"type": "string", "description": "A field to fill: " + elementArgDoc}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   []any{"text"},
	}
}

const maxTypeChars = 4000

func (t *typeTextTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	text := tools.StringArg(args, "text")
	if text == "" {
		return tools.Errorf("text must not be empty")
	}
	if len(text) > maxTypeChars {
		return tools.Errorf("text is %d characters; type at most %d at a time (or write a file instead)", len(text), maxTypeChars)
	}
	delay := tools.IntArg(args, "delay_ms", 12)
	if delay < 0 || delay > 1000 {
		delay = 12
	}
	if ref := strings.TrimSpace(tools.StringArg(args, "element")); ref != "" {
		// A field written directly takes newlines safely: no key is pressed.
		return t.typeIntoElement(ctx, ref, text, delay)
	}
	if r, ok := controlRune(text); ok {
		return tools.Errorf("text contains %s, which a keystroke sends as a key — Return submits the form it lands in and Tab moves to another field, so the rest of the text would go somewhere else. "+
			"Type each line separately and press_key the key you mean between them.", runeName(r))
	}
	if err := t.ctl.TypeText(ctx, text, delay); err != nil {
		return tools.Errorf("%v", err)
	}
	return tools.Textf("Typed %d characters.", len([]rune(text)))
}

type pressKeyTool struct{ *deps }

func (t *pressKeyTool) Name() string { return "press_key" }
func (t *pressKeyTool) Description() string {
	return `Press a key or key combination in the focused window, e.g. "Return", "ctrl+s", "Escape", "ctrl+shift+t". Modifiers: ctrl, alt, shift, super/cmd. ` +
		`Shortcuts that act on the session rather than the window — switching windows, locking the screen, closing the window in front, opening a terminal — are refused unless force is true.`
}
func (t *pressKeyTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"keys":   map[string]any{"type": "string", "description": `Key or combination, e.g. "ctrl+shift+t"`},
			"repeat": map[string]any{"type": "integer", "description": "How many times (default 1)"},
			"force":  map[string]any{"type": "boolean", "description": "Send a refused session shortcut anyway; only when the user asked for exactly that key"},
		},
		"required": []any{"keys"},
	}
}

func (t *pressKeyTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	keys := strings.TrimSpace(tools.StringArg(args, "keys"))
	if keys == "" {
		return tools.Errorf("keys must not be empty")
	}
	repeat := tools.IntArg(args, "repeat", 1)
	if repeat < 1 {
		repeat = 1
	}
	if repeat > 100 {
		return tools.Errorf("repeat is capped at 100")
	}
	if combo, why := blockedCombo(keys); combo != "" && !tools.BoolArg(args, "force", false) {
		return tools.Errorf("refused %s: it %s, so every key after it would land somewhere else. Nothing was sent. "+
			"To change windows use window_control action=focus; pass force=true only if the user asked for exactly this key.", combo, why)
	}
	if err := t.ctl.PressKey(ctx, keys, repeat); err != nil {
		return tools.Errorf("%v", err)
	}
	return tools.Textf("Pressed %s%s.", keys, plural(repeat))
}

func plural(repeat int) string {
	if repeat > 1 {
		return fmt.Sprintf(" ×%d", repeat)
	}
	return ""
}

// ---- clipboard -------------------------------------------------------------

type clipboardTool struct{ *deps }

func (t *clipboardTool) Name() string { return "clipboard" }
func (t *clipboardTool) Description() string {
	return "Read or write the desktop clipboard (action=get returns its text, action=set replaces it)."
}
func (t *clipboardTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{"type": "string", "enum": []any{"get", "set"}, "description": "get reads the clipboard, set overwrites it with text"},
			"text":   map[string]any{"type": "string", "description": "action=set: the new clipboard contents"},
		},
		"required": []any{"action"},
	}
}

const maxClipboardChars = 100_000

func (t *clipboardTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	switch tools.StringArg(args, "action") {
	case "get":
		text, err := t.ctl.ClipboardGet(ctx)
		if err != nil {
			return tools.Errorf("%v", err)
		}
		if strings.TrimSpace(text) == "" {
			return tools.Text("The clipboard is empty.")
		}
		if len(text) > maxClipboardChars {
			text = text[:maxClipboardChars] + "\n… [truncated]"
		}
		return tools.Text(text)
	case "set":
		text, ok := args["text"].(string)
		if !ok {
			return tools.Errorf("action=set needs text")
		}
		if err := t.ctl.ClipboardSet(ctx, text); err != nil {
			return tools.Errorf("%v", err)
		}
		return tools.Textf("Copied %d characters to the clipboard.", len([]rune(text)))
	}
	return tools.Errorf("action must be get or set")
}

// ---- notify ----------------------------------------------------------------

type notifyTool struct{ *deps }

func (t *notifyTool) Name() string { return "notify" }
func (t *notifyTool) Description() string {
	return "Show a desktop notification. Use it for things the user should see even when the chat window is not in front of them."
}
func (t *notifyTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":   map[string]any{"type": "string", "description": "Headline, kept short; this is what the user reads first"},
			"message": map[string]any{"type": "string", "description": "Body text under the title"},
			"urgency": map[string]any{"type": "string", "enum": []any{"low", "normal", "critical"}, "description": "Default normal; critical notifications stay on screen until dismissed on most desktops"},
		},
		"required": []any{"title"},
	}
}

func (t *notifyTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	title := tools.StringArg(args, "title")
	if title == "" {
		return tools.Errorf("title must not be empty")
	}
	urgency := tools.StringArg(args, "urgency")
	if urgency == "" {
		urgency = "normal"
	}
	if err := t.ctl.Notify(ctx, title, tools.StringArg(args, "message"), urgency); err != nil {
		return tools.Errorf("%v", err)
	}
	return tools.Textf("Notified: %s", title)
}

// ---- open ------------------------------------------------------------------

type openTool struct{ *deps }

func (t *openTool) Name() string { return "open" }
func (t *openTool) Description() string {
	return "Open a file, folder, or URL in the user's default application (xdg-open / open / Start-Process)."
}
func (t *openTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{"type": "string", "description": "Path or URL"},
		},
		"required": []any{"target"},
	}
}

func (t *openTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	target := strings.TrimSpace(tools.StringArg(args, "target"))
	if target == "" {
		return tools.Errorf("target must not be empty")
	}
	// Local paths go through the same guard as file reads; URLs pass straight
	// through (the browser tools cover fetching them).
	if !strings.Contains(target, "://") {
		resolved, err := t.guard.CheckRead(target)
		if err != nil {
			return tools.Errorf("%v", err)
		}
		target = resolved
	}
	if err := t.ctl.Open(ctx, target); err != nil {
		return tools.Errorf("%v", err)
	}
	return tools.Textf("Opened %s.", target)
}

// ---- desktop_info ----------------------------------------------------------

type desktopInfoTool struct {
	tools.ReadOnly
	*deps
}

func (t *desktopInfoTool) Name() string { return "desktop_info" }
func (t *desktopInfoTool) Description() string {
	return "Report the graphical session: backend (x11/wayland/macos/windows), screen size, desktop environment, and which helper programs are installed or missing. Check this when a desktop tool fails."
}
func (t *desktopInfoTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *desktopInfoTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	var b strings.Builder
	fmt.Fprintf(&b, "backend: %s\n", t.ctl.Backend())
	if de := t.env.env("XDG_CURRENT_DESKTOP"); de != "" {
		fmt.Fprintf(&b, "desktop: %s\n", de)
	}
	if display := t.env.env("DISPLAY"); display != "" {
		fmt.Fprintf(&b, "display: %s\n", display)
	}
	if w, h, err := t.ctl.ScreenSize(ctx); err == nil {
		fmt.Fprintf(&b, "screen:  %dx%d\n", w, h)
	} else {
		fmt.Fprintf(&b, "screen:  unknown (%v)\n", err)
	}

	var present, missing []string
	for _, h := range t.ctl.Helpers() {
		if t.env.has(h.Bin) {
			present = append(present, h.Bin)
		} else {
			missing = append(missing, fmt.Sprintf("%s (%s)", h.Bin, h.Purpose))
		}
	}
	sort.Strings(present)
	fmt.Fprintf(&b, "helpers installed: %s\n", orNone(strings.Join(present, ", ")))
	if len(missing) > 0 {
		fmt.Fprintf(&b, "helpers missing:   %s\n", strings.Join(missing, ", "))
		b.WriteString("install them with pkg_install to unlock those actions.\n")
	}
	if n, err := t.ctl.ListWindows(ctx); err == nil {
		fmt.Fprintf(&b, "open windows: %d\n", len(n))
	}
	return tools.Text(strings.TrimRight(b.String(), "\n"))
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
