package desktop

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The Linux controller picks whichever helper is installed, so the same
// operation lands on a different program on a GNOME box, a tiling-WM box and a
// Wayland session. These tests pin the preference order and the argv each
// branch produces.

func linuxCtl(t *testing.T, installed ...string) (*fakeMachine, Controller) {
	t.Helper()
	m := newMachine("linux", installed...)
	return m, NewController(m.Env())
}

func waylandCtl(t *testing.T, installed ...string) (*fakeMachine, Controller) {
	t.Helper()
	m := newMachine("linux", installed...)
	m.env = map[string]string{"WAYLAND_DISPLAY": "wayland-0"}
	return m, NewController(m.Env())
}

func TestLinuxBackendNameAndHelperSet(t *testing.T) {
	_, x11 := linuxCtl(t)
	if x11.Backend() != "x11" {
		t.Errorf("backend = %s", x11.Backend())
	}
	if bins := helperBins(x11); !bins["xdotool"] || !bins["wmctrl"] {
		t.Errorf("x11 helpers = %v", bins)
	}

	_, wl := waylandCtl(t)
	if wl.Backend() != "wayland" {
		t.Errorf("backend = %s", wl.Backend())
	}
	bins := helperBins(wl)
	if !bins["grim"] || !bins["wl-copy"] || bins["xdotool"] {
		t.Errorf("wayland helpers = %v", bins)
	}
}

func helperBins(c Controller) map[string]bool {
	bins := map[string]bool{}
	for _, h := range c.Helpers() {
		bins[h.Bin] = true
	}
	return bins
}

func TestLinuxFocusAndClosePreferWmctrl(t *testing.T) {
	ctx := context.Background()
	w := Window{ID: "0x42"}

	m, c := linuxCtl(t, "wmctrl", "xdotool")
	if err := c.Focus(ctx, w); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "wmctrl -i -a 0x42" {
		t.Errorf("focus ran %q", got)
	}
	if err := c.CloseWindow(ctx, w); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "wmctrl -i -c 0x42" {
		t.Errorf("close ran %q", got)
	}

	// without wmctrl the same calls go through xdotool
	m, c = linuxCtl(t, "xdotool")
	if err := c.Focus(ctx, w); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "xdotool windowactivate --sync 0x42" {
		t.Errorf("focus ran %q", got)
	}
	if err := c.CloseWindow(ctx, w); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "xdotool windowclose 0x42" {
		t.Errorf("close ran %q", got)
	}

	_, bare := linuxCtl(t)
	if err := bare.Focus(ctx, w); err == nil {
		t.Error("focus succeeded with no helper installed")
	}
	if err := bare.CloseWindow(ctx, w); err == nil {
		t.Error("close succeeded with no helper installed")
	}
}

func TestLinuxSetStateRaisesAfterRestore(t *testing.T) {
	ctx := context.Background()
	w := Window{ID: "0x42"}

	m, c := linuxCtl(t, "wmctrl", "xdotool")
	if err := c.SetState(ctx, w, "restore"); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 2 {
		t.Fatalf("restore ran %d commands: %+v", len(m.calls), m.calls)
	}
	if got := strings.Join(m.calls[0].argv, " "); got != "wmctrl -i -r 0x42 -b remove,maximized_vert,maximized_horz" {
		t.Errorf("first call = %q", got)
	}
	if got := strings.Join(m.lastCall(), " "); got != "xdotool windowactivate 0x42" {
		t.Errorf("restore did not raise the window: %q", got)
	}

	m.errs["wmctrl"] = errors.New("no such window")
	if err := c.SetState(ctx, w, "maximize"); err == nil {
		t.Error("a failed wmctrl state change was reported as success")
	}

	// minimize is xdotool's job, and needs it
	m, c = linuxCtl(t, "wmctrl", "xdotool")
	if err := c.SetState(ctx, w, "minimize"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "xdotool windowminimize 0x42" {
		t.Errorf("minimize ran %q", got)
	}
	_, noXdotool := linuxCtl(t, "wmctrl")
	if err := noXdotool.SetState(ctx, w, "minimize"); err == nil {
		t.Error("minimize succeeded without xdotool")
	}
	_, noWmctrl := linuxCtl(t, "xdotool")
	if err := noWmctrl.SetState(ctx, w, "fullscreen"); err == nil {
		t.Error("fullscreen succeeded without wmctrl")
	}
}

func TestLinuxMoveResizeBranches(t *testing.T) {
	ctx := context.Background()
	w := Window{ID: "0x42"}

	// wmctrl encodes "leave this alone" as -1
	m, c := linuxCtl(t, "wmctrl")
	if err := c.MoveResize(ctx, w, Geometry{W: 800, H: 600, HasSize: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "wmctrl -i -r 0x42 -e 0,-1,-1,800,600" {
		t.Errorf("resize ran %q", got)
	}
	if err := c.MoveResize(ctx, w, Geometry{X: 10, Y: 20, HasPos: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "wmctrl -i -r 0x42 -e 0,10,20,-1,-1" {
		t.Errorf("move ran %q", got)
	}

	// xdotool needs one call per half
	m, c = linuxCtl(t, "xdotool")
	if err := c.MoveResize(ctx, w, Geometry{X: 1, Y: 2, W: 3, H: 4, HasPos: true, HasSize: true}); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 2 {
		t.Fatalf("move+resize ran %d commands", len(m.calls))
	}
	if got := strings.Join(m.calls[0].argv, " "); got != "xdotool windowmove 0x42 1 2" {
		t.Errorf("move ran %q", got)
	}
	if got := strings.Join(m.calls[1].argv, " "); got != "xdotool windowsize 0x42 3 4" {
		t.Errorf("resize ran %q", got)
	}

	m.errs["xdotool windowmove"] = errors.New("nope")
	if err := c.MoveResize(ctx, w, Geometry{X: 1, Y: 2, HasPos: true}); err == nil {
		t.Error("a failed move was reported as success")
	}
	m.errs = map[string]error{"xdotool windowsize": errors.New("nope")}
	if err := c.MoveResize(ctx, w, Geometry{W: 3, H: 4, HasSize: true}); err == nil {
		t.Error("a failed resize was reported as success")
	}

	_, bare := linuxCtl(t)
	if err := bare.MoveResize(ctx, w, Geometry{}); err == nil {
		t.Error("move succeeded with no helper installed")
	}
}

func TestLinuxScreenshotHelperPreference(t *testing.T) {
	ctx := context.Background()
	win := Window{ID: "0x42"}

	cases := []struct {
		name      string
		installed []string
		shot      Shot
		want      string
	}{
		{"maim screen", []string{"maim"}, Shot{Mode: "screen"}, "maim --hidecursor /s.png"},
		{"maim window", []string{"maim"}, Shot{Mode: "window", Window: win}, "maim --hidecursor -i 0x42 /s.png"},
		{"maim region", []string{"maim"}, Shot{Mode: "region", Region: Geometry{X: 1, Y: 2, W: 3, H: 4}}, "maim --hidecursor -g 3x4+1+2 /s.png"},
		{"scrot region", []string{"scrot"}, Shot{Mode: "region", Region: Geometry{X: 1, Y: 2, W: 3, H: 4}}, "scrot --overwrite -a 1,2,3,4 /s.png"},
		{"grim region", []string{"grim"}, Shot{Mode: "region", Region: Geometry{X: 1, Y: 2, W: 3, H: 4}}, "grim -g 1,2 3x4 /s.png"},
		{"grim screen", []string{"grim"}, Shot{Mode: "screen"}, "grim /s.png"},
		{"import window", []string{"import"}, Shot{Mode: "window", Window: win}, "import -silent -window 0x42 /s.png"},
		{"import screen", []string{"import"}, Shot{Mode: "screen"}, "import -silent -window root /s.png"},
		{"import region", []string{"import"}, Shot{Mode: "region", Region: Geometry{X: 1, Y: 2, W: 3, H: 4}}, "import -silent -window root -crop 3x4+1+2 /s.png"},
		{"gnome screen", []string{"gnome-screenshot"}, Shot{Mode: "screen"}, "gnome-screenshot -f /s.png"},
		{"gnome window", []string{"gnome-screenshot"}, Shot{Mode: "window", Window: win}, "gnome-screenshot -f /s.png -w"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, c := linuxCtl(t, tc.installed...)
			if err := c.Screenshot(ctx, "/s.png", tc.shot); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(m.lastCall(), " "); got != tc.want {
				t.Errorf("ran %q, want %q", got, tc.want)
			}
		})
	}

	// scrot cannot aim at a window, so it focuses one first
	m, c := linuxCtl(t, "scrot", "wmctrl")
	if err := c.Screenshot(ctx, "/s.png", Shot{Mode: "window", Window: win}); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 2 || m.calls[0].argv[0] != "wmctrl" {
		t.Fatalf("calls = %+v", m.calls)
	}
	if got := strings.Join(m.lastCall(), " "); got != "scrot --overwrite -u /s.png" {
		t.Errorf("capture ran %q", got)
	}
	// ...and gives up if it cannot
	_, scrotOnly := linuxCtl(t, "scrot")
	if err := scrotOnly.Screenshot(ctx, "/s.png", Shot{Mode: "window", Window: win}); err == nil {
		t.Error("scrot captured a window it could not focus")
	}

	// grim has no window mode at all, and says so
	_, grim := waylandCtl(t, "grim")
	err := grim.Screenshot(ctx, "/s.png", Shot{Mode: "window", Window: win})
	if err == nil || !strings.Contains(err.Error(), "not supported on wayland/grim") {
		t.Errorf("error = %v", err)
	}
}

func TestLinuxClipboardAndTypingPreferWaylandNatives(t *testing.T) {
	ctx := context.Background()

	m, c := waylandCtl(t, "wl-copy", "wl-paste", "wtype")
	if err := c.ClipboardSet(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if m.calls[0].argv[0] != "wl-copy" || m.calls[0].stdin != "hello" {
		t.Errorf("clipboard set ran %+v", m.calls[0])
	}
	if err := c.TypeText(ctx, "-dash-leading", 0); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "wtype -- -dash-leading" {
		t.Errorf("type ran %q", got)
	}
	if err := c.PressKey(ctx, "ctrl+s", 1); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "wtype -k ctrl -k s" {
		t.Errorf("key ran %q", got)
	}

	// xsel is the last-resort X11 clipboard
	m, c = linuxCtl(t, "xsel")
	if err := c.ClipboardSet(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "xsel --clipboard --input" {
		t.Errorf("clipboard set ran %q", got)
	}
	m.outputs["xsel"] = "hi"
	if got, err := c.ClipboardGet(ctx); err != nil || got != "hi" {
		t.Errorf("clipboard get = %q, %v", got, err)
	}

	_, bare := linuxCtl(t)
	if err := bare.ClipboardSet(ctx, "x"); err == nil {
		t.Error("clipboard write succeeded with no helper installed")
	}
	if err := bare.TypeText(ctx, "x", 0); err == nil {
		t.Error("typing succeeded with no helper installed")
	}
	if err := bare.PressKey(ctx, "a", 1); err == nil {
		t.Error("key press succeeded with no helper installed")
	}
}

func TestLinuxScreenSizeSources(t *testing.T) {
	ctx := context.Background()

	m, c := linuxCtl(t, "xdotool")
	m.outputs["xdotool"] = "1920 1080\n"
	if w, h, err := c.ScreenSize(ctx); err != nil || w != 1920 || h != 1080 {
		t.Fatalf("screen = %d x %d, %v", w, h, err)
	}
	m.outputs["xdotool"] = "huge"
	if _, _, err := c.ScreenSize(ctx); err == nil {
		t.Error("unparseable xdotool output accepted")
	}
	m.errs["xdotool"] = errors.New("no display")
	if _, _, err := c.ScreenSize(ctx); err == nil {
		t.Error("a failed query reported a screen size")
	}

	m, c = linuxCtl(t, "xrandr")
	m.errs["xrandr"] = errors.New("no display")
	if _, _, err := c.ScreenSize(ctx); err == nil {
		t.Error("a failed xrandr reported a screen size")
	}
	m.errs = map[string]error{}
	m.outputs["xrandr"] = "Screen 0: minimum 320 x 200\n"
	if _, _, err := c.ScreenSize(ctx); err == nil {
		t.Error("xrandr output with no connected display was accepted")
	}

	_, bare := linuxCtl(t)
	if _, _, err := bare.ScreenSize(ctx); err == nil {
		t.Error("screen size succeeded with no helper installed")
	}
}

func TestLinuxOpenFallbacks(t *testing.T) {
	ctx := context.Background()
	m, c := linuxCtl(t, "gio")
	if err := c.Open(ctx, "https://example.com"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "gio open https://example.com" {
		t.Errorf("open ran %q", got)
	}

	m, c = linuxCtl(t, "exo-open")
	if err := c.Open(ctx, "/tmp/a.pdf"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.lastCall(), " "); got != "exo-open /tmp/a.pdf" {
		t.Errorf("open ran %q", got)
	}
}

func TestLinuxActiveWindowAndListErrors(t *testing.T) {
	ctx := context.Background()

	m, c := linuxCtl(t, "xdotool")
	m.errs["xdotool getactivewindow"] = errors.New("no active window")
	if _, err := c.ActiveWindow(ctx); err == nil {
		t.Error("a failed query returned a window")
	}

	m.errs = map[string]error{"wmctrl": errors.New("cannot open display")}
	m.installed["wmctrl"] = true
	if _, err := c.ListWindows(ctx); err == nil {
		t.Error("a failed wmctrl returned a window list")
	}

	m, c = linuxCtl(t, "xdotool")
	m.errs["xdotool search"] = errors.New("no display")
	if _, err := c.ListWindows(ctx); err == nil {
		t.Error("a failed search returned a window list")
	}
}
