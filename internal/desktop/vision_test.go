package desktop

import (
	"bytes"
	"encoding/base64"
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
