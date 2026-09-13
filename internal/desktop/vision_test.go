package desktop

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/provider"
)

// shotMachine scripts an X11 box whose scrot writes a real PNG of the given
// size, so the vision tools run their whole pipeline against actual pixels.
func shotMachine(t *testing.T, w, h int) *fakeMachine {
	t.Helper()
	m := newMachine("linux", "scrot", "xdotool")
	m.onRun = func(argv []string) {
		if argv[0] != "scrot" {
			return
		}
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.SetRGBA(x, y, color.RGBA{40, 80, 120, 255})
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(argv[len(argv)-1], buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func decodeAttached(t *testing.T, part provider.ImagePart) image.Image {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(part.Data)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestScreenViewAttachesAnnotatedFrame(t *testing.T) {
	m := shotMachine(t, 640, 480)
	byName, _ := newTools(t, m)
	res := run(t, byName["screen_view"], map[string]any{})
	if res.IsError {
		t.Fatalf("screen_view failed: %s", res.ForLLM)
	}
	if len(res.Images) != 1 || res.Images[0].MediaType != "image/png" {
		t.Fatalf("images = %+v", res.Images)
	}
	for _, want := range []string{"640x480", "A1-G5", "screen_zoom", "mouse cell="} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("ForLLM missing %q: %s", want, res.ForLLM)
		}
	}

	decoded := decodeAttached(t, res.Images[0])
	if decoded.Bounds().Dx() != 640 || decoded.Bounds().Dy() != 480 {
		t.Fatalf("attached image = %v, small frames must not be rescaled", decoded.Bounds())
	}
	// The grid is really drawn: bright line core at x=96 (cell size 96).
	if got := color.RGBAModel.Convert(decoded.At(96, 5)); got != gridLineInner {
		t.Errorf("grid line pixel = %v, want %v", got, gridLineInner)
	}
}

func TestScreenViewWithoutHelperExplains(t *testing.T) {
	m := newMachine("linux") // nothing installed
	byName, _ := newTools(t, m)
	res := run(t, byName["screen_view"], map[string]any{})
	if !res.IsError || !strings.Contains(res.ForLLM, "install") {
		t.Errorf("want actionable missing-helper error, got %q", res.ForLLM)
	}
}

func TestMouseCellClicksResolvedPixels(t *testing.T) {
	m := shotMachine(t, 640, 480)
	byName, _ := newTools(t, m)
	if res := run(t, byName["screen_view"], map[string]any{}); res.IsError {
		t.Fatal(res.ForLLM)
	}

	// Cell B2 on a 96px grid centers at (144, 144); scale is 1.
	res := run(t, byName["mouse"], map[string]any{"action": "click", "cell": "B2"})
	if res.IsError {
		t.Fatalf("mouse cell click failed: %s", res.ForLLM)
	}
	if !m.ranWith("mousemove 144 144") {
		t.Errorf("expected mousemove 144 144, calls: %v", m.calls)
	}
	if !strings.Contains(res.ForLLM, "cell B2 → 144,144") {
		t.Errorf("reply should state the resolution: %q", res.ForLLM)
	}
}

func TestMouseCellRequiresViewFirst(t *testing.T) {
	m := shotMachine(t, 640, 480)
	byName, _ := newTools(t, m)
	res := run(t, byName["mouse"], map[string]any{"action": "click", "cell": "B2"})
	if !res.IsError || !strings.Contains(res.ForLLM, "screen_view first") {
		t.Errorf("want screen_view-first error, got %q", res.ForLLM)
	}
	res = run(t, byName["mouse"], map[string]any{"action": "click", "cell": "B2", "x": 5, "y": 5})
	if !res.IsError || !strings.Contains(res.ForLLM, "not both") {
		t.Errorf("cell+xy should conflict, got %q", res.ForLLM)
	}
}

func TestScreenZoomCellThenFineClick(t *testing.T) {
	m := shotMachine(t, 640, 480)
	byName, _ := newTools(t, m)
	if res := run(t, byName["screen_view"], map[string]any{}); res.IsError {
		t.Fatal(res.ForLLM)
	}

	res := run(t, byName["screen_zoom"], map[string]any{"cell": "b2"})
	if res.IsError {
		t.Fatalf("screen_zoom failed: %s", res.ForLLM)
	}
	if len(res.Images) != 1 {
		t.Fatalf("zoom attached %d images", len(res.Images))
	}
	if !strings.Contains(res.ForLLM, "cell B2") || !strings.Contains(res.ForLLM, "×2.0") {
		t.Errorf("zoom summary = %q", res.ForLLM)
	}

	// Cell B2 spans (96,96)-(192,192); margin 24 → crop (72,72)-(216,216),
	// upscaled ×2 to 288x288. Sub-grid floors to 60px cells (5 cols/rows).
	// Zoom cell A1 centers at (30,30) on the crop → native (72+15, 72+15).
	res = run(t, byName["mouse"], map[string]any{"action": "click", "cell": "A1"})
	if res.IsError {
		t.Fatalf("fine click failed: %s", res.ForLLM)
	}
	if !m.ranWith("mousemove 87 87") {
		t.Errorf("expected mousemove 87 87 after zoom, calls: %v", m.calls)
	}
	if !strings.Contains(res.ForLLM, "zoomed cell A1") {
		t.Errorf("reply should note zoomed resolution: %q", res.ForLLM)
	}

	// A fresh screen_view drops the zoom: the same cell resolves coarse again.
	if res := run(t, byName["screen_view"], map[string]any{}); res.IsError {
		t.Fatal(res.ForLLM)
	}
	run(t, byName["mouse"], map[string]any{"action": "click", "cell": "A1"})
	if !m.ranWith("mousemove 48 48") {
		t.Errorf("expected coarse mousemove 48 48 after re-view, calls: %v", m.calls)
	}
}

func TestScreenZoomRegionAndErrors(t *testing.T) {
	m := shotMachine(t, 640, 480)
	byName, _ := newTools(t, m)

	res := run(t, byName["screen_zoom"], map[string]any{"cell": "A1"})
	if !res.IsError || !strings.Contains(res.ForLLM, "screen_view first") {
		t.Errorf("zoom before view should error, got %q", res.ForLLM)
	}

	if res := run(t, byName["screen_view"], map[string]any{}); res.IsError {
		t.Fatal(res.ForLLM)
	}
	res = run(t, byName["screen_zoom"], map[string]any{"x": 100, "y": 100, "width": 200, "height": 150})
	if res.IsError {
		t.Fatalf("region zoom failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "region 200x150 at 100,100") {
		t.Errorf("region summary = %q", res.ForLLM)
	}

	res = run(t, byName["screen_zoom"], map[string]any{})
	if !res.IsError || !strings.Contains(res.ForLLM, "pass cell") {
		t.Errorf("argless zoom should instruct, got %q", res.ForLLM)
	}
	res = run(t, byName["screen_zoom"], map[string]any{"cell": "Z99"})
	if !res.IsError {
		t.Error("out-of-grid zoom cell should error")
	}
}

func TestScreenViewDownscalesLargeScreens(t *testing.T) {
	m := shotMachine(t, 3200, 1200)
	byName, _ := newTools(t, m)
	res := run(t, byName["screen_view"], map[string]any{})
	if res.IsError {
		t.Fatal(res.ForLLM)
	}
	decoded := decodeAttached(t, res.Images[0])
	if got := decoded.Bounds().Dx(); got != maxViewDim {
		t.Errorf("attached width = %d, want %d", got, maxViewDim)
	}
	if !strings.Contains(res.ForLLM, "3200x1200") {
		t.Errorf("native size must be reported: %q", res.ForLLM)
	}

	// Cells resolve back to NATIVE pixels: view is 1568x588, cell 117
	// (588/5=117), so A1 centers at view (58,58) → native ≈ (119,119).
	res = run(t, byName["mouse"], map[string]any{"action": "move", "cell": "A1"})
	if res.IsError {
		t.Fatal(res.ForLLM)
	}
	var moved []string
	for _, c := range m.calls {
		if len(c.argv) == 4 && c.argv[0] == "xdotool" && c.argv[1] == "mousemove" {
			moved = c.argv[2:]
		}
	}
	if moved == nil {
		t.Fatal("no mousemove recorded")
	}
	x, _ := strconv.Atoi(moved[0])
	y, _ := strconv.Atoi(moved[1])
	if x < 110 || x > 128 || y < 110 || y > 128 {
		t.Errorf("A1 native center = %d,%d, want ≈119,119", x, y)
	}
}

// screencapture writes the backing store, so a Retina frame is twice the
// desktop in each direction while cliclick takes points. A cell resolved off
// that frame and handed over unconverted lands at twice its coordinates.
func TestResolveCellConvertsRetinaPixelsToPoints(t *testing.T) {
	raw := image.NewRGBA(image.Rect(0, 0, 2880, 1800))
	grid := layoutFor(2880, 1800, gridDivisions)

	var v visionState
	v.setView(raw, 1, 2, grid)
	retina, _, err := v.resolveCell("A1")
	if err != nil {
		t.Fatal(err)
	}

	var plain visionState
	plain.setView(raw, 1, 1, grid)
	points, _, err := plain.resolveCell("A1")
	if err != nil {
		t.Fatal(err)
	}
	if retina.X*2 != points.X || retina.Y*2 != points.Y {
		t.Fatalf("A1 resolved to %v on Retina and %v otherwise; want half", retina, points)
	}
	if retina.X <= 0 || retina.Y <= 0 {
		t.Fatalf("A1 resolved to %v", retina)
	}
}

// A zoomed cell goes through a different branch and needs the same conversion.
func TestResolveZoomedCellConvertsToPoints(t *testing.T) {
	raw := image.NewRGBA(image.Rect(0, 0, 2880, 1800))
	grid := layoutFor(2880, 1800, gridDivisions)

	var v visionState
	v.setView(raw, 1, 2, grid)
	_, zoom, err := buildZoom(raw, image.Rect(400, 400, 600, 600))
	if err != nil {
		t.Fatal(err)
	}
	v.setZoom(zoom)
	got, what, err := v.resolveCell("A1")
	if err != nil {
		t.Fatal(err)
	}
	if what != "zoomed cell" {
		t.Fatalf("resolved against %q", what)
	}
	// The crop starts at 400 px, which is point 200 on a 2x display.
	if got.X < 190 || got.X > 260 {
		t.Fatalf("zoomed A1 resolved to %v, which is not inside the crop in points", got)
	}
}

func TestSetViewTreatsAnUnknownScaleAsOne(t *testing.T) {
	raw := image.NewRGBA(image.Rect(0, 0, 100, 100))
	var v visionState
	v.setView(raw, 1, 0, layoutFor(100, 100, gridDivisions))
	if v.pointScale != 1 {
		t.Fatalf("pointScale = %v, want 1", v.pointScale)
	}
}

// Only macOS captures in units the pointer does not use. Reading a disagreeing
// screen size as a scale anywhere else would halve every click on a machine
// whose xrandr reports one monitor of several.
func TestCaptureScaleIsAskedOnDarwinOnly(t *testing.T) {
	ctx := context.Background()
	retina := image.NewRGBA(image.Rect(0, 0, 2880, 1800))

	mac := &sizedController{w: 1440, h: 900}
	if got := captureScale(ctx, mac, Env{GOOS: "darwin"}, retina); got != 2 {
		t.Errorf("Retina scale = %v, want 2", got)
	}
	if !mac.asked {
		t.Error("the screen size was never read")
	}

	plain := &sizedController{w: 2880, h: 1800}
	if got := captureScale(ctx, plain, Env{GOOS: "darwin"}, retina); got != 1 {
		t.Errorf("non-Retina scale = %v, want 1", got)
	}

	lying := &sizedController{w: 1440, h: 900}
	if got := captureScale(ctx, lying, Env{GOOS: "linux"}, retina); got != 1 {
		t.Errorf("linux scale = %v, want 1", got)
	}
	if lying.asked {
		t.Error("linux was asked for a scale it cannot have")
	}

	broken := &sizedController{err: errors.New("no screen")}
	if got := captureScale(ctx, broken, Env{GOOS: "darwin"}, retina); got != 1 {
		t.Errorf("scale with no answer = %v, want 1", got)
	}
	odd := &sizedController{w: 400, h: 300} // a 7.2x ratio is not a display
	if got := captureScale(ctx, odd, Env{GOOS: "darwin"}, retina); got != 1 {
		t.Errorf("implausible scale = %v, want 1", got)
	}
}

// sizedController answers ScreenSize and nothing else.
type sizedController struct {
	Controller
	w, h  int
	err   error
	asked bool
}

func (c *sizedController) ScreenSize(context.Context) (int, int, error) {
	c.asked = true
	return c.w, c.h, c.err
}
