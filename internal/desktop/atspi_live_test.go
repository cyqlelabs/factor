//go:build linux

package desktop

// End-to-end tests for the accessibility layer against a REAL AT-SPI stack: a
// private Xvfb, a private session bus, the real accessibility bus launcher it
// activates, and a real GTK window with known controls at known places.
//
// The tree's word is never what a test checks itself against. Where a control
// is comes from the pixels of a capture — the two large buttons are solid
// colours — and whether an action landed comes from the fixture's own log of
// what GTK did. A backend that read the wrong node, mapped coordinates wrongly
// or reported a press it never delivered cannot agree with either.
//
// Needs Xvfb, scrot, xdotool, dbus-daemon, at-spi2-core and a Python with
// GTK 3 bindings; skips anywhere they are missing and in -short mode, and
// fails instead under FACTOR_REQUIRE_LIVE_DESKTOP.

import (
	"context"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

const (
	fixtureX, fixtureY = 60, 40
)

// a11yDesktop is one live session: where the fixture logs what it did.
type a11yDesktop struct {
	log string
}

// gtkPython finds an interpreter that can import GTK 3. The system Python is
// the one python3-gi was built for, and on a machine with several installed
// it is not always the one called python3.
func gtkPython() string {
	for _, bin := range []string{"python3", "/usr/bin/python3", "python3.12", "python3.11", "python3.13", "python3.10"} {
		path, err := exec.LookPath(bin)
		if err != nil {
			continue
		}
		cmd := exec.Command(path, "-I", "-c", "import gi; gi.require_version('Gtk', '3.0'); from gi.repository import Gtk")
		cmd.Env = append(os.Environ(), "DISPLAY=")
		if cmd.Run() == nil {
			return path
		}
	}
	return ""
}

// startA11yDesktop brings up the whole stack and the fixture, and returns
// once the fixture's controls are readable on the accessibility bus.
func startA11yDesktop(t *testing.T) *a11yDesktop {
	t.Helper()
	if testing.Short() {
		liveSkip(t, "short mode")
	}
	for _, bin := range []string{"Xvfb", "scrot", "xdotool", "dbus-daemon"} {
		if _, err := exec.LookPath(bin); err != nil {
			liveSkip(t, "%s is not installed", bin)
		}
	}
	if !hasLauncher() {
		liveSkip(t, "at-spi2-core is not installed")
	}
	python := gtkPython()
	if python == "" {
		liveSkip(t, "no Python with GTK 3 bindings (python3-gi, gir1.2-gtk-3.0)")
	}

	display := startXvfb(t, liveScreenW, liveScreenH)
	t.Setenv("DISPLAY", display)
	runtime := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	t.Setenv("AT_SPI_BUS_ADDRESS", "")
	t.Setenv("NO_AT_BRIDGE", "")
	t.Setenv("GTK_A11Y", "")
	bus := "unix:path=" + filepath.Join(runtime, "bus")
	daemon := exec.Command("dbus-daemon", "--session", "--nofork", "--nopidfile", "--address="+bus)
	daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := daemon.Start(); err != nil {
		liveSkip(t, "cannot start a session bus: %v", err)
	}
	t.Cleanup(func() {
		// The bus launcher and the accessibility bus are activated by the
		// session bus and live in its process group.
		_ = syscall.Kill(-daemon.Process.Pid, syscall.SIGKILL)
		_, _ = daemon.Process.Wait()
	})
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", bus)
	waitFor(t, "the session bus", func() bool {
		_, err := os.Stat(filepath.Join(runtime, "bus"))
		return err == nil
	})

	d := &a11yDesktop{log: filepath.Join(t.TempDir(), "fixture.log")}
	fixture := exec.Command(python, "-I", filepath.Join("testdata", "a11y_fixture.py"), d.log,
		itoa(fixtureX), itoa(fixtureY))
	fixture.Stderr = os.Stderr
	if err := fixture.Start(); err != nil {
		liveSkip(t, "cannot start the GTK fixture: %v", err)
	}
	t.Cleanup(func() {
		_ = fixture.Process.Kill()
		_, _ = fixture.Process.Wait()
	})
	waitFor(t, "the fixture to draw", func() bool { return d.saw("ready") })

	acc := NewAccessor(DefaultEnv(), NewController(DefaultEnv()))
	waitFor(t, "the fixture on the accessibility bus", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		tree, err := acc.Read(ctx, nil)
		if err != nil {
			return false
		}
		for _, e := range tree.Elements {
			if e.Name == "Save" {
				return true
			}
		}
		return false
	})
	return d
}

func hasLauncher() bool {
	for _, p := range []string{"/usr/libexec/at-spi-bus-launcher", "/usr/lib/at-spi2-core/at-spi-bus-launcher", "/usr/lib/at-spi-bus-launcher"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// saw reports whether the fixture logged a line.
func (d *a11yDesktop) saw(line string) bool {
	data, err := os.ReadFile(d.log)
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(data), "\n") {
		if l == line {
			return true
		}
	}
	return false
}

// count reports how many times the fixture logged a line.
func (d *a11yDesktop) count(line string) int {
	data, _ := os.ReadFile(d.log)
	n := 0
	for _, l := range strings.Split(string(data), "\n") {
		if l == line {
			n++
		}
	}
	return n
}

func green(r, g, b uint8) bool { return r < 40 && g > 170 && b < 40 }
func blue(r, g, b uint8) bool  { return r < 40 && g < 40 && b > 200 }

// readFixture reads the fixture's tree through the production path.
func readFixture(t *testing.T) *Tree {
	t.Helper()
	acc := NewAccessor(DefaultEnv(), NewController(DefaultEnv()))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tree, err := acc.Read(ctx, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	tree.finish(image.Rect(0, 0, liveScreenW, liveScreenH))
	return tree
}

func find(t *testing.T, tree *Tree, role, name string) Element {
	t.Helper()
	for _, e := range tree.Elements {
		if e.Role == role && e.Name == name {
			return e
		}
	}
	var got []string
	for _, e := range tree.Elements {
		got = append(got, e.Label())
	}
	t.Fatalf("no %s %q in the tree: %v", role, name, got)
	return Element{}
}

func TestLiveATSPIReadsTheFixture(t *testing.T) {
	startA11yDesktop(t)
	tree := readFixture(t)
	if tree.Quality() != QualityRich || !tree.Complete || !tree.Positioned {
		t.Fatalf("quality %s complete %t positioned %t: %+v", tree.Quality(), tree.Complete, tree.Positioned, tree.Elements)
	}
	if tree.Window != "Factor Fixture" {
		t.Errorf("window = %q", tree.Window)
	}
	name := find(t, tree, RoleTextField, "Name")
	if name.Value != "hello" || !name.Can(ActSetText) {
		t.Errorf("name field = %+v", name)
	}
	secret := find(t, tree, RolePassword, "Password")
	if secret.Value != "" || !secret.Secret {
		t.Errorf("a password field's contents were read: %+v", secret)
	}
	if box := find(t, tree, RoleCheckbox, "Remember me"); box.Checked != TriOff || !box.Can(ActPress) {
		t.Errorf("checkbox = %+v", box)
	}
	if off := find(t, tree, RoleButton, "Unavailable"); !off.Disabled {
		t.Errorf("an insensitive button reads as enabled: %+v", off)
	}

	// Where the tree says Save is must be where Save is drawn.
	frame := capture(t)
	for name, color := range map[string]func(r, g, b uint8) bool{"Save": green, "Cancel": blue} {
		drawn := mustFind(t, frame, color, name)
		listed := find(t, tree, RoleButton, name).Bounds
		if overlap := listed.Intersect(drawn); overlap.Dx()*overlap.Dy() < drawn.Dx()*drawn.Dy()*8/10 {
			t.Errorf("%s: the tree puts it at %v, the screen draws it at %v", name, listed, drawn)
		}
	}
}

// capture takes a real screenshot of the live screen.
func capture(t *testing.T) *image.RGBA {
	t.Helper()
	frame, err := captureFrame(context.Background(), NewController(DefaultEnv()))
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	return frame
}

// liveA11yTools builds the production tool set with the tree switched on.
func liveA11yTools(t *testing.T) map[string]tools.Tool {
	t.Helper()
	ws := t.TempDir()
	byName := map[string]tools.Tool{}
	for _, tool := range NewTools(DefaultEnv(), tools.NewPathGuard(ws, true, false, nil), filepath.Join(ws, "screenshots")) {
		byName[tool.Name()] = tool
	}
	return byName
}

// refFor finds the ref a screen_view listing gave a control.
func refFor(t *testing.T, listing, what string) string {
	t.Helper()
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && strings.HasPrefix(fields[0], "e") && strings.Contains(line, " "+what) {
			return fields[0]
		}
	}
	t.Fatalf("%s is not listed:\n%s", what, listing)
	return ""
}

func yellow(r, g, b uint8) bool { return r > 230 && g > 190 && g < 235 && b < 40 }

// yellowAround counts overlay pixels in a thin ring around a box.
func yellowAround(img *image.RGBA, box image.Rectangle) int {
	n := 0
	ring := box.Inset(-4)
	for y := ring.Min.Y; y < ring.Max.Y; y++ {
		for x := ring.Min.X; x < ring.Max.X; x++ {
			if (image.Point{X: x, Y: y}).In(box.Inset(4)) {
				continue
			}
			c := img.RGBAAt(x, y)
			if yellow(c.R, c.G, c.B) {
				n++
			}
		}
	}
	return n
}

func TestLiveScreenViewTagsAndActsThroughTheTree(t *testing.T) {
	d := startA11yDesktop(t)
	byName := liveA11yTools(t)
	ctx := context.Background()

	saveDrawn := mustFind(t, capture(t), green, "Save")
	cancelDrawn := mustFind(t, capture(t), blue, "Cancel")
	look := byName["screen_view"].Execute(ctx, map[string]any{})
	if look.IsError || len(look.Images) != 1 {
		t.Fatalf("screen_view: %s", look.ForLLM)
	}
	if !strings.Contains(look.ForLLM, `Accessibility tree of "Factor Fixture"`) {
		t.Fatalf("no tree in the look:\n%s", look.ForLLM)
	}
	t.Log(look.ForLLM)
	view := decodePNG(t, look.Images[0])
	if n := yellowAround(view, saveDrawn); n < 100 {
		t.Errorf("the Save button drawn at %v carries no outline in the attached image (%d overlay pixels around it)", saveDrawn, n)
	}
	save := refFor(t, look.ForLLM, `button "Save"`)
	cancel := refFor(t, look.ForLLM, `button "Cancel"`)
	remember := refFor(t, look.ForLLM, `checkbox "Remember me"`)
	name := refFor(t, look.ForLLM, `textfield "Name"`)
	if !strings.Contains(look.ForLLM, `password "Password" = (hidden)`) {
		t.Errorf("the password field is not listed as hidden:\n%s", look.ForLLM)
	}

	// A press goes through the application: GTK reports the click, and the
	// pointer is exactly where it was.
	before := pointerAt(t)
	res := byName["mouse"].Execute(ctx, map[string]any{"action": "click", "element": save})
	if res.IsError || !strings.Contains(res.ForLLM, "pointer was not moved") {
		t.Fatalf("press: %s", res.ForLLM)
	}
	waitFor(t, "GTK to report the click", func() bool { return d.saw("clicked Save") })
	if after := pointerAt(t); after != before {
		t.Errorf("a semantic press moved the pointer from %v to %v", before, after)
	}

	res = byName["mouse"].Execute(ctx, map[string]any{"action": "click", "element": remember})
	if res.IsError || !strings.Contains(res.ForLLM, "now checked") {
		t.Fatalf("checkbox: %s", res.ForLLM)
	}
	waitFor(t, "GTK to report the toggle", func() bool { return d.saw("check True") })

	res = byName["type_text"].Execute(ctx, map[string]any{"text": "Factor", "element": name})
	if res.IsError || !strings.Contains(res.ForLLM, "holds exactly that") {
		t.Fatalf("type: %s", res.ForLLM)
	}
	waitFor(t, "GTK to report the new text", func() bool { return d.saw("entry Factor") })

	// What the application cannot do itself, the pointer does — at the
	// control as the screen draws it.
	res = byName["mouse"].Execute(ctx, map[string]any{"action": "double_click", "element": cancel})
	if res.IsError {
		t.Fatalf("double click: %s", res.ForLLM)
	}
	if p := pointerAt(t); !p.In(cancelDrawn) {
		t.Errorf("the pointer landed at %v, outside Cancel drawn at %v", p, cancelDrawn)
	}
	waitFor(t, "GTK to report both clicks", func() bool { return d.count("clicked Cancel") >= 2 })

	// A ref read off an older look is refused before anything is sent.
	byName["screen_view"].Execute(ctx, map[string]any{})
	stale := byName["mouse"].Execute(ctx, map[string]any{"action": "click", "element": "f1:" + save})
	if !stale.IsError || !strings.Contains(stale.ForLLM, "not the latest look") {
		t.Errorf("a ref from an older frame was not refused: %s", stale.ForLLM)
	}
	if d.count("clicked Save") != 1 {
		t.Errorf("Save was clicked %d times", d.count("clicked Save"))
	}
}
