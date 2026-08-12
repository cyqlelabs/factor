package desktop

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// These cover the macOS and Windows controllers on any host: every operation
// ends in a script or argv the fake machine records, so the escaping, the
// AppleScript/PowerShell bodies and the output parsing are all assertable
// without the corresponding OS.

func macCtl(t *testing.T, installed ...string) (*fakeMachine, Controller) {
	t.Helper()
	m := newMachine("darwin", installed...)
	return m, NewController(m.Env())
}

func winCtl(t *testing.T, installed ...string) (*fakeMachine, Controller) {
	t.Helper()
	m := newMachine("windows", installed...)
	return m, NewController(m.Env())
}

func TestMacBackendAndHelpers(t *testing.T) {
	_, c := macCtl(t)
	if c.Backend() != "macos" {
		t.Errorf("backend = %s", c.Backend())
	}
	bins := map[string]bool{}
	for _, h := range c.Helpers() {
		bins[h.Bin] = true
		if h.Purpose == "" {
			t.Errorf("helper %s has no purpose", h.Bin)
		}
	}
	for _, want := range []string{"osascript", "screencapture", "pbcopy", "cliclick"} {
		if !bins[want] {
			t.Errorf("helper %q is not declared", want)
		}
	}
}

func TestMacActiveWindow(t *testing.T) {
	m, c := macCtl(t, "osascript")
	m.outputs["osascript"] = "640:1\tTerminal\n"
	w, err := c.ActiveWindow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if w.ID != "640:1" || w.App != "Terminal" || w.Title != "Terminal" {
		t.Errorf("window = %+v", w)
	}

	// a reply that is not "id<tab>app" is reported, not silently accepted
	m.outputs["osascript"] = "\n"
	if _, err := c.ActiveWindow(context.Background()); err == nil {
		t.Error("garbage active-window reply accepted")
	}
}

func TestMacWindowOperationsBuildTheirScripts(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		call func(Controller) error
		want string
	}{
		{"focus", func(c Controller) error { return c.Focus(ctx, Window{ID: "12:3"}) }, `set frontmost of p to true`},
		{"close", func(c Controller) error { return c.CloseWindow(ctx, Window{ID: "12:3"}) }, `AXCloseButton`},
		{"minimize", func(c Controller) error { return c.SetState(ctx, Window{ID: "12:3"}, "minimize") }, `"AXMinimized" of w to true`},
		{"restore", func(c Controller) error { return c.SetState(ctx, Window{ID: "12:3"}, "restore") }, `"AXMinimized" of w to false`},
		{"fullscreen", func(c Controller) error { return c.SetState(ctx, Window{ID: "12:3"}, "fullscreen") }, `"AXFullScreen" of w to true`},
		{"unfullscreen", func(c Controller) error { return c.SetState(ctx, Window{ID: "12:3"}, "unfullscreen") }, `"AXFullScreen" of w to false`},
		{"move", func(c Controller) error {
			return c.MoveResize(ctx, Window{ID: "12:3"}, Geometry{X: 5, Y: 6, HasPos: true})
		}, `set position of w to {5, 6}`},
		{"resize", func(c Controller) error {
			return c.MoveResize(ctx, Window{ID: "12:3"}, Geometry{W: 80, H: 90, HasSize: true})
		}, `set size of w to {80, 90}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, c := macCtl(t, "osascript")
			if err := tc.call(c); err != nil {
				t.Fatal(err)
			}
			script := m.lastCall()[2]
			if !strings.Contains(script, tc.want) {
				t.Errorf("script = %s", script)
			}
			// every window script addresses the right process and window
			if !strings.Contains(script, "unix id is 12") || !strings.Contains(script, "window 3 of p") {
				t.Errorf("script does not target 12:3: %s", script)
			}
		})
	}
}

func TestMacMaximizeUsesTheScreenSize(t *testing.T) {
	m, c := macCtl(t, "osascript")
	m.outputs["osascript"] = "0, 0, 1512, 945"
	if err := c.SetState(context.Background(), Window{ID: "12:3"}, "maximize"); err != nil {
		t.Fatal(err)
	}
	if script := m.lastCall()[2]; !strings.Contains(script, "set size of w to {1512, 945}") {
		t.Errorf("script = %s", script)
	}

	// a screen-size failure aborts the maximize instead of resizing to 0x0
	m.errs["osascript"] = errors.New("no Finder")
	if err := c.SetState(context.Background(), Window{ID: "12:3"}, "maximize"); err == nil {
		t.Error("maximize survived an unreadable screen size")
	}
	if err := c.SetState(context.Background(), Window{ID: "12:3"}, "levitate"); err == nil {
		t.Error("unknown window state accepted")
	}
}

func TestMacScreenSize(t *testing.T) {
	m, c := macCtl(t, "osascript")
	m.outputs["osascript"] = "0, 0, 2560, 1440\n"
	w, h, err := c.ScreenSize(context.Background())
	if err != nil || w != 2560 || h != 1440 {
		t.Fatalf("screen = %d x %d, %v", w, h, err)
	}

	m.outputs["osascript"] = "wide and tall"
	if _, _, err := c.ScreenSize(context.Background()); err == nil {
		t.Error("unparseable desktop bounds accepted")
	}
}

func TestMacMouse(t *testing.T) {
	m, c := macCtl(t, "cliclick")
	ctx := context.Background()
	if err := c.MoveMouse(ctx, 100, 200); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "cliclick m:100,200" {
		t.Errorf("move ran %q", got)
	}

	if err := c.Click(ctx, "left", 1, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "cliclick c:." {
		t.Errorf("click ran %q", got)
	}
	if err := c.Click(ctx, "left", 2, &Point{X: 4, Y: 5}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "cliclick dc:4,5" {
		t.Errorf("double click ran %q", got)
	}
	if err := c.Click(ctx, "right", 1, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "cliclick rc:." {
		t.Errorf("right click ran %q", got)
	}
	if err := c.Click(ctx, "sideways", 1, nil); err == nil {
		t.Error("unknown button accepted")
	}
}

func TestMacMissingHelpersAreActionable(t *testing.T) {
	_, c := macCtl(t) // nothing installed
	ctx := context.Background()
	cases := map[string]error{
		"osascript":     firstErr(func() error { _, err := c.ListWindows(ctx); return err }),
		"screencapture": c.Screenshot(ctx, "/tmp/x.png", Shot{Mode: "screen"}),
		"cliclick":      c.MoveMouse(ctx, 1, 1),
		"pbcopy":        c.ClipboardSet(ctx, "x"),
		"pbpaste":       firstErr(func() error { _, err := c.ClipboardGet(ctx); return err }),
		"open":          c.Open(ctx, "https://example.com"),
	}
	for bin, err := range cases {
		if err == nil || !strings.Contains(err.Error(), bin) {
			t.Errorf("missing %s reported as %v", bin, err)
		}
		if err != nil && !strings.Contains(err.Error(), "pkg_install") {
			t.Errorf("missing %s does not say how to fix it: %v", bin, err)
		}
	}
}

func TestMacClipboardGetAndOpen(t *testing.T) {
	m, c := macCtl(t, "pbpaste", "open")
	ctx := context.Background()
	m.outputs["pbpaste"] = "copied"
	got, err := c.ClipboardGet(ctx)
	if err != nil || got != "copied" {
		t.Fatalf("clipboard = %q, %v", got, err)
	}
	if err := c.Open(ctx, "/tmp/file.pdf"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "open /tmp/file.pdf" {
		t.Errorf("open ran %q", got)
	}
}

// The accessibility hint is the difference between a user granting the
// permission and filing a bug, so it must survive to the caller.
func TestMacAccessibilityHint(t *testing.T) {
	m, c := macCtl(t, "osascript")
	m.errs["osascript"] = errors.New("osascript: not allowed assistive access")
	_, err := c.ListWindows(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Accessibility") {
		t.Fatalf("error = %v", err)
	}
}

func TestMacScreenshotWindowFocusesFirst(t *testing.T) {
	m, c := macCtl(t, "osascript", "screencapture")
	err := c.Screenshot(context.Background(), "/tmp/w.png", Shot{
		Mode:   "window",
		Window: Window{ID: "9:1", X: 10, Y: 20, W: 300, H: 400, HasGeom: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 2 || m.calls[0].argv[0] != "osascript" {
		t.Fatalf("calls = %+v", m.calls)
	}
	if got := strings.Join(m.lastCall(), " "); got != "screencapture -x -R 10,20,300,400 /tmp/w.png" {
		t.Errorf("capture ran %q", got)
	}

	// an unfocusable window aborts before capturing a wrong screen area
	m.calls = nil
	if err := c.Screenshot(context.Background(), "/tmp/w.png", Shot{Mode: "window", Window: Window{ID: "bogus"}}); err == nil {
		t.Error("screenshot proceeded with an unusable window id")
	}
}

func TestWindowsBackendAndHelpers(t *testing.T) {
	_, c := winCtl(t)
	if c.Backend() != "windows" {
		t.Errorf("backend = %s", c.Backend())
	}
	if h := c.Helpers(); len(h) != 1 || h[0].Bin != "powershell" {
		t.Errorf("helpers = %+v", h)
	}
}

func TestWindowsFallsBackToPwsh(t *testing.T) {
	m, c := winCtl(t, "pwsh")
	if _, err := c.ListWindows(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.lastCall()[0] != "pwsh" {
		t.Errorf("ran %q, want pwsh", m.lastCall()[0])
	}
}

func TestWindowsActiveWindow(t *testing.T) {
	m, c := winCtl(t, "powershell")
	m.outputs["powershell"] = "  74562  \r\n"
	w, err := c.ActiveWindow(context.Background())
	if err != nil || w.ID != "74562" {
		t.Fatalf("window = %+v, %v", w, err)
	}

	// "0" means no foreground window — not a window with id zero
	m.outputs["powershell"] = "0\r\n"
	if _, err := c.ActiveWindow(context.Background()); err == nil {
		t.Error("a null handle was accepted as a window")
	}
}

func TestWindowsWindowOperationsBuildTheirScripts(t *testing.T) {
	ctx := context.Background()
	w := Window{ID: "4242"}
	cases := []struct {
		name string
		call func(Controller) error
		want string
	}{
		{"focus", func(c Controller) error { return c.Focus(ctx, w) }, "SetForegroundWindow("},
		{"close", func(c Controller) error { return c.CloseWindow(ctx, w) }, "PostMessage("},
		{"minimize", func(c Controller) error { return c.SetState(ctx, w, "minimize") }, "ShowWindow([IntPtr]::new([int64]'4242'), 6)"},
		{"maximize", func(c Controller) error { return c.SetState(ctx, w, "maximize") }, "ShowWindow([IntPtr]::new([int64]'4242'), 3)"},
		{"restore", func(c Controller) error { return c.SetState(ctx, w, "restore") }, "ShowWindow([IntPtr]::new([int64]'4242'), 9)"},
		{"mouse", func(c Controller) error { return c.MoveMouse(ctx, 7, 8) }, "SetCursorPos(7, 8)"},
		{"open", func(c Controller) error { return c.Open(ctx, "C:\\tmp\\a.txt") }, `Start-Process 'C:\tmp\a.txt'`},
		{"clipboard", func(c Controller) error { return c.ClipboardSet(ctx, "it's") }, `Set-Clipboard -Value 'it''s'`},
		{"notify", func(c Controller) error { return c.Notify(ctx, "T", "B", "normal") }, "ShowBalloonTip(5000, 'T', 'B'"},
		{"type", func(c Controller) error { return c.TypeText(ctx, "50% off", 0) }, "SendWait('50{%} off')"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, c := winCtl(t, "powershell")
			if err := tc.call(c); err != nil {
				t.Fatal(err)
			}
			argv := m.lastCall()
			if !strings.Contains(argv[len(argv)-1], tc.want) {
				t.Errorf("script = %s", argv[len(argv)-1])
			}
		})
	}

	_, c := winCtl(t, "powershell")
	if err := c.SetState(ctx, w, "levitate"); err == nil {
		t.Error("unknown window state accepted")
	}
}

func TestWindowsClick(t *testing.T) {
	m, c := winCtl(t, "powershell")
	ctx := context.Background()
	if err := c.Click(ctx, "right", 1, &Point{X: 3, Y: 4}); err != nil {
		t.Fatal(err)
	}
	script := m.lastCall()[len(m.lastCall())-1]
	if !strings.Contains(script, "SetCursorPos(3, 4)") || !strings.Contains(script, "mouse_event(0x0008") {
		t.Errorf("script = %s", script)
	}

	if err := c.Click(ctx, "left", 2, nil); err != nil {
		t.Fatal(err)
	}
	script = m.lastCall()[len(m.lastCall())-1]
	if n := strings.Count(script, "mouse_event(0x0002"); n != 2 {
		t.Errorf("double click pressed %d times: %s", n, script)
	}
	if !strings.Contains(script, "Start-Sleep -Milliseconds 80") {
		t.Error("consecutive clicks are not spaced out")
	}
	if err := c.Click(ctx, "sideways", 1, nil); err == nil {
		t.Error("unknown button accepted")
	}
}

func TestWindowsScreenshotModes(t *testing.T) {
	ctx := context.Background()
	m, c := winCtl(t, "powershell")
	if err := c.Screenshot(ctx, `C:\shots\a.png`, Shot{Mode: "screen"}); err != nil {
		t.Fatal(err)
	}
	script := m.lastCall()[len(m.lastCall())-1]
	if !strings.Contains(script, "PrimaryScreen.Bounds") || !strings.Contains(script, `'C:\shots\a.png'`) {
		t.Errorf("script = %s", script)
	}

	if err := c.Screenshot(ctx, "a.png", Shot{Mode: "region", Region: Geometry{X: 1, Y: 2, W: 3, H: 4}}); err != nil {
		t.Fatal(err)
	}
	if script := m.lastCall()[len(m.lastCall())-1]; !strings.Contains(script, "Drawing.Rectangle(1, 2, 3, 4)") {
		t.Errorf("script = %s", script)
	}

	// a window shot without geometry cannot be aimed, and says so
	err := c.Screenshot(ctx, "a.png", Shot{Mode: "window", Window: Window{ID: "1"}})
	if err == nil || !strings.Contains(err.Error(), "window_list") {
		t.Errorf("error = %v", err)
	}
}

func TestWindowsKeysAndClipboardRoundTrip(t *testing.T) {
	m, c := winCtl(t, "powershell")
	ctx := context.Background()

	if err := c.PressKey(ctx, "Return", 3); err != nil {
		t.Fatal(err)
	}
	script := m.lastCall()[len(m.lastCall())-1]
	if !strings.Contains(script, "-lt 3;") || !strings.Contains(script, "SendWait('{ENTER}')") {
		t.Errorf("script = %s", script)
	}
	if err := c.PressKey(ctx, "notakey", 1); err == nil {
		t.Error("unknown key accepted")
	}

	m.outputs["powershell"] = "pasted\r\n"
	got, err := c.ClipboardGet(ctx)
	if err != nil || strings.TrimSpace(got) != "pasted" {
		t.Errorf("clipboard = %q, %v", got, err)
	}
}

func TestWindowsScreenSize(t *testing.T) {
	m, c := winCtl(t, "powershell")
	m.outputs["powershell"] = "1920 1080\r\n"
	w, h, err := c.ScreenSize(context.Background())
	if err != nil || w != 1920 || h != 1080 {
		t.Fatalf("screen = %d x %d, %v", w, h, err)
	}

	m.outputs["powershell"] = "huge"
	if _, _, err := c.ScreenSize(context.Background()); err == nil {
		t.Error("unparseable screen bounds accepted")
	}
	m.errs["powershell"] = errors.New("boom")
	if _, _, err := c.ScreenSize(context.Background()); err == nil {
		t.Error("a failed query reported a screen size")
	}
}

func firstErr(fn func() error) error { return fn() }
