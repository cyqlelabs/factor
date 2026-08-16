package desktop

// End-to-end tests for grid vision against a REAL X server: a real screenshot
// helper captures a real screen, the real grid overlay is drawn on it, and a
// real pointer is moved by the real xdotool. Nothing is scripted.
//
// The whole promise of pointing tools is that a cell named on the picture
// lands the pointer on the thing under it, and only a live round trip can
// prove that. The unit tests agree with the code's own arithmetic by
// construction; these read the grid back out of the rendered pixels and check
// the pointer against targets whose true screen positions are known
// independently — so a wrong overlay, a wrong scale, or a wrong translation
// all show up as a pointer in the wrong place.
//
// Requires Xvfb, scrot, xdotool and xlogo; skips anywhere they are missing
// (CI without X, machines without the helpers) and in -short mode.

import (
	"context"
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// Two targets sit on the live screen, both in colors the overlay never draws
// (it uses cyan lines, black outlines and white labels):
//
//   - big, magenta — comfortably larger than a grid cell, so naming its cell
//     is enough to hit it.
//   - small, red — smaller than a cell and off-center inside it, so the
//     coarse pass MISSES it and only the zoom pass can land on it. That gap
//     is the entire reason screen_zoom exists, so a test should show it.
const (
	liveScreenW, liveScreenH = 1024, 768
	bigX, bigY               = 480, 360
	bigW, bigH               = 240, 180
	smallX, smallY           = 700, 250
	smallW, smallH           = 60, 48
)

func bigTarget() image.Rectangle {
	return image.Rect(bigX, bigY, bigX+bigW, bigY+bigH)
}

func smallTarget() image.Rectangle {
	return image.Rect(smallX, smallY, smallX+smallW, smallY+smallH)
}

// startLiveDesktop brings up a private Xvfb with both targets on it and points
// this process's DISPLAY at it, so the production path (DefaultEnv → the real
// helper programs) drives a screen whose contents the test knows exactly.
// Returns once both targets are actually visible in a capture.
func startLiveDesktop(t *testing.T) { startLiveDesktopAt(t, liveScreenW, liveScreenH) }

// startLiveDesktopAt is startLiveDesktop on a screen of the given size, so a
// test can choose one big enough to force the frame sent to the model to be
// downscaled while the pointer still has to land in native pixels.
func startLiveDesktopAt(t *testing.T, screenW, screenH int) {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	for _, bin := range []string{"Xvfb", "scrot", "xdotool", "xlogo"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}

	display := startXvfb(t, screenW, screenH)
	t.Setenv("DISPLAY", display)

	paintTarget(t, bigTarget(), "#ff00ff")
	paintTarget(t, smallTarget(), "#ff0000")
	waitForTargets(t, map[string]colorMatch{
		"big":   {magenta, bigTarget()},
		"small": {red, smallTarget()},
	})
}

// colorMatch pairs a color test with the box that color should occupy.
type colorMatch struct {
	is  func(r, g, b uint8) bool
	box image.Rectangle
}

// paintTarget puts a solid block of one color on screen at an exact position.
// No window manager runs, so xlogo's requested geometry is honored exactly and
// the block sits where the test says it does; painting foreground and
// background the same color fills the window rather than drawing a logo.
func paintTarget(t *testing.T, box image.Rectangle, color string) {
	t.Helper()
	cmd := exec.Command("xlogo", "-geometry",
		fmt.Sprintf("%dx%d+%d+%d", box.Dx(), box.Dy(), box.Min.X, box.Min.Y),
		"-bg", color, "-fg", color)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start xlogo: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
}

// waitForTargets blocks until every named block is fully painted, so a test
// never reads a screen that is still being drawn.
func waitForTargets(t *testing.T, want map[string]colorMatch) {
	t.Helper()
	ctl := NewController(DefaultEnv())
	shot := filepath.Join(t.TempDir(), "probe.png")
	waitFor(t, "the targets to be painted", func() bool {
		if err := ctl.Screenshot(context.Background(), shot, Shot{Mode: "screen"}); err != nil {
			return false
		}
		f, err := os.Open(shot)
		if err != nil {
			return false
		}
		defer f.Close()
		img, _, err := image.Decode(f)
		if err != nil {
			return false
		}
		frame := toRGBA(img)
		for _, m := range want {
			box, ok := findColor(frame, m.is)
			// Whole blocks only, not the first pixels of a mapping window.
			if !ok || box.Dx() < m.box.Dx()-2 || box.Dy() < m.box.Dy()-2 {
				return false
			}
		}
		return true
	})
}

// startXvfb brings up a virtual X server and returns the display it answers
// on. Display numbers are claimed by trying one: a killed server leaves its
// socket and lock behind, and several test binaries may be running at once, so
// the only trustworthy evidence that a number is usable is a server actually
// answering on it. Each candidate that does not is killed and skipped over.
func startXvfb(t *testing.T, screenW, screenH int) string {
	t.Helper()
	var lastErr error
	for n := 90; n < 120; n++ {
		display := fmt.Sprintf(":%d", n)
		xvfb := exec.Command("Xvfb", display, "-screen", "0",
			fmt.Sprintf("%dx%dx24", screenW, screenH))
		if err := xvfb.Start(); err != nil {
			t.Skipf("cannot start Xvfb: %v", err)
		}
		stop := func() {
			_ = xvfb.Process.Kill()
			_, _ = xvfb.Process.Wait()
		}
		if answers(display, screenW, 5*time.Second) {
			t.Cleanup(stop)
			return display
		}
		lastErr = fmt.Errorf("%s never answered", display)
		stop()
	}
	t.Skipf("no usable X display: %v", lastErr)
	return ""
}

// answers reports whether an X server of the expected width is serving the
// display. DISPLAY is passed to the child rather than set on the process, so a
// candidate that fails leaves nothing behind.
func answers(display string, screenW int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		cmd := exec.Command("xdotool", "getdisplaygeometry")
		cmd.Env = append(os.Environ(), "DISPLAY="+display)
		if out, err := cmd.Output(); err == nil &&
			strings.HasPrefix(string(out), strconv.Itoa(screenW)) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func waitFor(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// liveTools builds the real desktop arsenal against the real machine.
func liveTools(t *testing.T) map[string]tools.Tool {
	t.Helper()
	ws := t.TempDir()
	byName := map[string]tools.Tool{}
	for _, tool := range NewTools(DefaultEnv(), tools.NewPathGuard(ws, true, false, nil),
		filepath.Join(ws, "screenshots")) {
		byName[tool.Name()] = tool
	}
	return byName
}

// pointerAt reads the real pointer position back from X.
func pointerAt(t *testing.T) image.Point {
	t.Helper()
	out, err := exec.Command("xdotool", "getmouselocation").Output()
	if err != nil {
		t.Fatalf("getmouselocation: %v", err)
	}
	var p image.Point
	for _, field := range strings.Fields(string(out)) {
		key, value, ok := strings.Cut(field, ":")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			continue
		}
		switch key {
		case "x":
			p.X = n
		case "y":
			p.Y = n
		}
	}
	return p
}

// --- reading the rendered grid back out of the picture -----------------------
//
// These deliberately re-derive geometry from pixels instead of calling the
// grid code: a test that asked layoutFor() where the lines are would agree
// with a wrong overlay just as happily as a right one.

// findColor returns the bounding box of the pixels matching want, and whether
// any were found. The accumulators are plain ints on purpose: image.Rect
// canonicalizes its arguments, so an "inverted" seed rectangle would silently
// come back as the whole image and every box would look like a match.
func findColor(img *image.RGBA, want func(r, g, b uint8) bool) (image.Rectangle, bool) {
	b := img.Bounds()
	minX, minY := b.Max.X, b.Max.Y
	maxX, maxY := b.Min.X, b.Min.Y
	found := false
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			if !want(c.R, c.G, c.B) {
				continue
			}
			found = true
			minX, minY = min(minX, x), min(minY, y)
			maxX, maxY = max(maxX, x+1), max(maxY, y+1)
		}
	}
	if !found {
		return image.Rectangle{}, false
	}
	return image.Rect(minX, minY, maxX, maxY), true
}

func magenta(r, g, b uint8) bool { return r > 200 && g < 60 && b > 200 }
func red(r, g, b uint8) bool     { return r > 200 && g < 60 && b < 60 }

func centerOf(r image.Rectangle) image.Point {
	return image.Pt((r.Min.X+r.Max.X)/2, (r.Min.Y+r.Max.Y)/2)
}

// mustFind locates a target in a frame, failing the test when it is absent.
func mustFind(t *testing.T, img *image.RGBA, want func(r, g, b uint8) bool, what string) image.Rectangle {
	t.Helper()
	box, ok := findColor(img, want)
	if !ok {
		t.Fatalf("the %s target is not visible in the frame", what)
	}
	return box
}

// gridLinesFromPixels finds the drawn grid lines by looking for the overlay's
// bright core running most of the way across the image, and returns the first
// coordinate of each line (x positions, then y positions).
func gridLinesFromPixels(t *testing.T, img *image.RGBA) (xs, ys []int) {
	t.Helper()
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	collect := func(n, across int, at func(i, j int) bool) []int {
		var starts []int
		run := false
		for i := 0; i < n; i++ {
			hits := 0
			for j := 0; j < across; j++ {
				if at(i, j) {
					hits++
				}
			}
			// Half the span is a wide margin: only the labels interrupt a line.
			if hits*2 > across {
				if !run {
					starts = append(starts, i)
					run = true
				}
			} else {
				run = false
			}
		}
		return starts
	}
	xs = collect(w, h, func(x, y int) bool { return img.RGBAAt(x, y) == gridLineInner })
	ys = collect(h, w, func(y, x int) bool { return img.RGBAAt(x, y) == gridLineInner })
	if len(xs) < 2 || len(ys) < 2 {
		t.Fatalf("found %d vertical and %d horizontal grid lines; the overlay is not being drawn",
			len(xs), len(ys))
	}
	return xs, ys
}

// cellContaining names the cell holding p from the line positions read off the
// image plus the documented convention (letters across, numbers down) — the
// same reasoning a model looking at the picture does.
func cellContaining(t *testing.T, xs, ys []int, p image.Point) string {
	t.Helper()
	index := func(starts []int, v int) int {
		i := -1
		for k, s := range starts {
			if v >= s {
				i = k
			}
		}
		if i < 0 {
			t.Fatalf("point %v falls before the first grid line %v", p, starts)
		}
		return i
	}
	return fmt.Sprintf("%s%d", columnLabel(index(xs, p.X)), index(ys, p.Y)+1)
}

func decodePNG(t *testing.T, part provider.ImagePart) *image.RGBA {
	t.Helper()
	return toRGBA(decodeAttached(t, part))
}

// look runs screen_view and returns the annotated frame with its grid lines.
func look(t *testing.T, byName map[string]tools.Tool) (*image.RGBA, []int, []int) {
	t.Helper()
	view := run(t, byName["screen_view"], map[string]any{})
	if view.IsError {
		t.Fatalf("screen_view: %s", view.ForLLM)
	}
	if len(view.Images) != 1 {
		t.Fatalf("screen_view attached %d images, want 1", len(view.Images))
	}
	frame := decodePNG(t, view.Images[0])
	xs, ys := gridLinesFromPixels(t, frame)
	return frame, xs, ys
}

// --- the tests ---------------------------------------------------------------

// TestLiveGridPointing is the headline round trip: look at a real screen, name
// the cell the target is in, click it, land on it.
func TestLiveGridPointing(t *testing.T) {
	startLiveDesktop(t)
	byName := liveTools(t)

	frame, xs, ys := look(t, byName)
	if frame.Bounds().Dx() != liveScreenW || frame.Bounds().Dy() != liveScreenH {
		t.Fatalf("attached frame = %v, want the native %dx%d",
			frame.Bounds(), liveScreenW, liveScreenH)
	}
	// The capture is of the real screen, so the target must appear where X put
	// it — a frame that is stale, cropped or scaled fails here first.
	box := mustFind(t, frame, magenta, "big")
	if !box.In(bigTarget().Inset(-2)) || box.Dx() < bigW-2 || box.Dy() < bigH-2 {
		t.Fatalf("the big target appears at %v, but X placed it at %v", box, bigTarget())
	}

	cell := cellContaining(t, xs, ys, centerOf(box))
	res := run(t, byName["mouse"], map[string]any{"action": "click", "cell": cell})
	if res.IsError {
		t.Fatalf("mouse click cell=%s: %s", cell, res.ForLLM)
	}
	if got := pointerAt(t); !got.In(bigTarget()) {
		t.Fatalf("clicking cell %s put the pointer at %v, outside the target %v",
			cell, got, bigTarget())
	}
}

// TestLiveGridZoomHitsWhatTheCoarseGridMisses is the reason the second pass
// exists: a target smaller than a cell and off-center within it is not
// reachable by naming the cell, and is reachable after zooming into it.
func TestLiveGridZoomHitsWhatTheCoarseGridMisses(t *testing.T) {
	startLiveDesktop(t)
	byName := liveTools(t)

	frame, xs, ys := look(t, byName)
	box := mustFind(t, frame, red, "small")
	if !box.In(smallTarget().Inset(-2)) {
		t.Fatalf("the small target appears at %v, but X placed it at %v", box, smallTarget())
	}
	cell := cellContaining(t, xs, ys, centerOf(box))

	// Coarse pass: the cell's center is not on this target.
	if res := run(t, byName["mouse"], map[string]any{"action": "move", "cell": cell}); res.IsError {
		t.Fatalf("mouse move cell=%s: %s", cell, res.ForLLM)
	}
	coarse := pointerAt(t)
	if coarse.In(smallTarget()) {
		t.Skipf("the coarse cell %s already lands on the small target at %v; "+
			"this test needs a target the first pass misses", cell, coarse)
	}

	// Zoom pass: the same target, named on the finer grid, is reachable.
	zoom := run(t, byName["screen_zoom"], map[string]any{"cell": cell})
	if zoom.IsError {
		t.Fatalf("screen_zoom cell=%s: %s", cell, zoom.ForLLM)
	}
	zoomed := decodePNG(t, zoom.Images[0])
	zbox := mustFind(t, zoomed, red, "small (zoomed)")
	// Magnification is real: the target covers more pixels than it did before.
	if zbox.Dx() <= box.Dx() {
		t.Errorf("zoomed target is %dpx wide, no larger than the %dpx original", zbox.Dx(), box.Dx())
	}
	zxs, zys := gridLinesFromPixels(t, zoomed)
	fineCell := cellContaining(t, zxs, zys, centerOf(zbox))

	if res := run(t, byName["mouse"], map[string]any{"action": "click", "cell": fineCell}); res.IsError {
		t.Fatalf("mouse click cell=%s on the zoomed view: %s", fineCell, res.ForLLM)
	}
	fine := pointerAt(t)
	if !fine.In(smallTarget()) {
		t.Fatalf("after zooming, clicking cell %s put the pointer at %v, still outside the target %v",
			fineCell, fine, smallTarget())
	}
}

// TestLiveGridRegionZoom covers the other way in: zooming a pixel region
// straight from a window's geometry, with no coarse cell involved.
func TestLiveGridRegionZoom(t *testing.T) {
	startLiveDesktop(t)
	byName := liveTools(t)
	look(t, byName) // a frame must exist before it can be zoomed

	zoom := run(t, byName["screen_zoom"], map[string]any{
		"x": smallX, "y": smallY, "width": smallW, "height": smallH,
	})
	if zoom.IsError {
		t.Fatalf("screen_zoom region: %s", zoom.ForLLM)
	}
	zoomed := decodePNG(t, zoom.Images[0])
	box := mustFind(t, zoomed, red, "small (region zoom)")
	// The crop is exactly the target, so the target must fill it.
	if box.Dx() < zoomed.Bounds().Dx()*3/4 || box.Dy() < zoomed.Bounds().Dy()*3/4 {
		t.Fatalf("the crop is %v but the target covers only %v; the region is off",
			zoomed.Bounds(), box)
	}

	zxs, zys := gridLinesFromPixels(t, zoomed)
	cell := cellContaining(t, zxs, zys, centerOf(box))
	if res := run(t, byName["mouse"], map[string]any{"action": "click", "cell": cell}); res.IsError {
		t.Fatalf("mouse click cell=%s: %s", cell, res.ForLLM)
	}
	if got := pointerAt(t); !got.In(smallTarget()) {
		t.Errorf("region-zoom click landed at %v, outside the target %v", got, smallTarget())
	}
}

// TestLiveGridPointsAcrossADownscaledFrame covers the translation that only
// happens on a big screen: the frame the model looks at is shrunk to fit the
// image budget, so every cell it names — on the overview and on the zoom crop
// taken from it — has to be scaled back up before the pointer moves.
func TestLiveGridPointsAcrossADownscaledFrame(t *testing.T) {
	const wideW, wideH = 1920, 1200
	startLiveDesktopAt(t, wideW, wideH)

	// A target far from the origin, where the gap between frame pixels and
	// screen pixels is widest: near (0,0) the two nearly agree, so a click
	// that forgot to scale would still land and the bug would hide.
	far := image.Rect(1170, 725, 1470, 955)
	isGreen := func(r, g, b uint8) bool { return r < 60 && g > 200 && b < 60 }
	paintTarget(t, far, "#00ff00")
	waitForTargets(t, map[string]colorMatch{"far": {isGreen, far}})

	byName := liveTools(t)
	frame, xs, ys := look(t, byName)
	if frame.Bounds().Dx() != maxViewDim {
		t.Fatalf("frame is %v on a %dx%d screen; this test needs the downscaled path",
			frame.Bounds(), wideW, wideH)
	}
	box := mustFind(t, frame, isGreen, "far")
	// The target's true screen box is unchanged; in the frame it appears
	// smaller, which is exactly the discrepancy the translation has to undo.
	if box.Dx() >= far.Dx() {
		t.Fatalf("target spans %dpx in a downscaled frame, want less than its native %dpx",
			box.Dx(), far.Dx())
	}
	if box.Min.X >= far.Min.X {
		t.Fatalf("target starts at x=%d in the frame and x=%d on screen; "+
			"the frame does not look downscaled", box.Min.X, far.Min.X)
	}

	// Coarse pass: naming a cell on the shrunken frame must move the pointer
	// in full-size screen pixels.
	cell := cellContaining(t, xs, ys, centerOf(box))
	if res := run(t, byName["mouse"], map[string]any{"action": "click", "cell": cell}); res.IsError {
		t.Fatalf("mouse click cell=%s: %s", cell, res.ForLLM)
	}
	coarse := pointerAt(t)
	if !coarse.In(far) {
		t.Fatalf("on a downscaled frame, clicking cell %s landed at %v, outside the target %v",
			cell, coarse, far)
	}

	// Zoom pass: the crop is cut from the same shrunken frame, so its cells
	// carry the scale too.
	zoom := run(t, byName["screen_zoom"], map[string]any{"cell": cell})
	if zoom.IsError {
		t.Fatalf("screen_zoom cell=%s: %s", cell, zoom.ForLLM)
	}
	zoomed := decodePNG(t, zoom.Images[0])
	zbox := mustFind(t, zoomed, isGreen, "far (zoomed)")
	zxs, zys := gridLinesFromPixels(t, zoomed)
	fineCell := cellContaining(t, zxs, zys, centerOf(zbox))

	if res := run(t, byName["mouse"], map[string]any{"action": "click", "cell": fineCell}); res.IsError {
		t.Fatalf("mouse click cell=%s: %s", fineCell, res.ForLLM)
	}
	if got := pointerAt(t); !got.In(far) {
		t.Fatalf("zooming a downscaled frame then clicking cell %s landed at %v, outside the target %v",
			fineCell, got, far)
	}
}

// TestLiveGridSurvivesAScreenChange guards the stalest-frame failure mode:
// after the screen changes, a fresh look must ground clicks in the NEW screen.
func TestLiveGridSurvivesAScreenChange(t *testing.T) {
	startLiveDesktop(t)
	byName := liveTools(t)

	frame, xs, ys := look(t, byName)
	box := mustFind(t, frame, magenta, "big")
	cell := cellContaining(t, xs, ys, centerOf(box))
	if res := run(t, byName["mouse"], map[string]any{"action": "move", "cell": cell}); res.IsError {
		t.Fatalf("mouse move cell=%s: %s", cell, res.ForLLM)
	}

	// Move the target, then look again: the same visual target now lives in a
	// different cell, and the pointer must follow the picture, not the memory.
	moved := image.Rect(120, 100, 120+bigW, 100+bigH)
	cmd := exec.Command("xlogo", "-geometry",
		fmt.Sprintf("%dx%d+%d+%d", moved.Dx(), moved.Dy(), moved.Min.X, moved.Min.Y),
		"-bg", "#00ff00", "-fg", "#00ff00")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start xlogo: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	isGreen := func(r, g, b uint8) bool { return r < 60 && g > 200 && b < 60 }
	waitFor(t, "the moved target", func() bool {
		f, _, _ := look(t, byName)
		box, ok := findColor(f, isGreen)
		return ok && box.Dx() >= bigW-2
	})

	frame, xs, ys = look(t, byName)
	greenBox := mustFind(t, frame, isGreen, "green")
	greenCell := cellContaining(t, xs, ys, centerOf(greenBox))
	if greenCell == cell {
		t.Fatalf("both targets landed in cell %s; the test needs them in different cells", cell)
	}
	if res := run(t, byName["mouse"], map[string]any{"action": "click", "cell": greenCell}); res.IsError {
		t.Fatalf("mouse click cell=%s: %s", greenCell, res.ForLLM)
	}
	if got := pointerAt(t); !got.In(moved) {
		t.Fatalf("after the screen changed, clicking cell %s landed at %v, outside the new target %v",
			greenCell, got, moved)
	}
}
