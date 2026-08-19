package desktop

// The grid-vision tools: screen_view captures the desktop and overlays the
// battleship grid, screen_zoom magnifies one cell (or any pixel region) under
// a finer sub-grid, and the mouse tool's cell argument resolves either grid's
// cells back to native screen pixels. Together they give a vision model a
// reliable two-pass pointing loop — coarse cell, zoom, fine cell, click —
// with no OCR, no ML models, and no helpers beyond the screenshot program
// already required.

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/jpeg" // some screenshot helpers ignore the .png extension
	"image/png"
	"os"
	"strings"
	"sync"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// visionState is the shared perception state: the last native-resolution
// frame plus the geometry needed to translate grid cells (on the possibly
// downscaled view, or on a zoomed crop) back to screen pixels. One desktop,
// one state — concurrent sessions share it, hence the lock.
type visionState struct {
	mu        sync.Mutex
	raw       *image.RGBA // native-resolution frame from the last screen_view
	viewScale float64     // view px per native px (≤ 1)
	grid      gridLayout  // over the view image
	zoom      *zoomView   // nil until screen_zoom; cleared by screen_view
}

func (v *visionState) setView(raw *image.RGBA, scale float64, grid gridLayout) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.raw, v.viewScale, v.grid, v.zoom = raw, scale, grid, nil
}

func (v *visionState) setZoom(z zoomView) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.zoom = &z
}

// snapshot returns the current frame and geometry for a zoom pass.
func (v *visionState) snapshot() (*image.RGBA, float64, gridLayout, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.raw, v.viewScale, v.grid, v.raw != nil
}

// resolveCell maps a cell named on the most recent attached image — the
// zoomed crop when one is active, the full-screen grid otherwise — to the
// native screen pixel at its center.
func (v *visionState) resolveCell(coord string) (image.Point, string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.raw == nil {
		return image.Point{}, "", fmt.Errorf("no grid view yet — run screen_view first")
	}
	if v.zoom != nil {
		p, err := v.zoom.cellCenterNative(coord)
		if err != nil {
			return image.Point{}, "", fmt.Errorf("%v (on the current zoomed view; screen_view resets to the full screen)", err)
		}
		return p, "zoomed cell", nil
	}
	r, err := v.grid.cellRectByName(coord)
	if err != nil {
		return image.Point{}, "", err
	}
	cx := float64(r.Min.X+r.Max.X) / 2
	cy := float64(r.Min.Y+r.Max.Y) / 2
	return image.Point{X: int(cx / v.viewScale), Y: int(cy / v.viewScale)}, "cell", nil
}

// captureFrame takes a full-screen screenshot through the controller into a
// temp file and decodes it. The file never touches the workspace.
func captureFrame(ctx context.Context, ctl Controller) (*image.RGBA, error) {
	tmp, err := os.CreateTemp("", "factor-view-*.png")
	if err != nil {
		return nil, err
	}
	path := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(path) }()

	if err := ctl.Screenshot(ctx, path, Shot{Mode: "screen"}); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("the screenshot helper reported success but wrote nothing: %w", err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode screenshot: %w", err)
	}
	return toRGBA(img), nil
}

func pngPart(img *image.RGBA) (provider.ImagePart, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return provider.ImagePart{}, err
	}
	return provider.ImagePart{
		MediaType: "image/png",
		Data:      base64.StdEncoding.EncodeToString(buf.Bytes()),
	}, nil
}

// ---- screen_view -----------------------------------------------------------

type screenViewTool struct{ *deps }

func (t *screenViewTool) Name() string { return "screen_view" }
func (t *screenViewTool) Description() string {
	return "Look at the screen: capture it and attach the image with a battleship coordinate grid (columns A,B,C…, rows 1,2,3…) overlaid. " +
		"To act on something you see: small or dense targets deserve a screen_zoom on their cell first (finer grid, better precision); then click with mouse cell=... . " +
		"Run it again after acting to see the result. Needs a vision-capable model. " +
		"This is for native applications. For anything in a web page use the browser tools instead — they read the page's own elements, cost a fraction of a screenshot, and are not disturbed by the user moving the mouse or changing windows."
}
func (t *screenViewTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *screenViewTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	raw, err := captureFrame(ctx, t.ctl)
	if err != nil {
		return tools.Errorf("screen_view: %v", err)
	}
	view, scale := fitForModel(raw)
	grid := layoutFor(view.Bounds().Dx(), view.Bounds().Dy(), gridDivisions)
	drawGrid(view, grid)
	part, err := pngPart(view)
	if err != nil {
		return tools.Errorf("screen_view: encode: %v", err)
	}
	t.vision.setView(raw, scale, grid)

	cellNative := int(float64(grid.CellSize) / scale)
	return &tools.Result{
		ForLLM: fmt.Sprintf(
			"Screen is %dx%d px. Attached: the current screen with grid cells A1-%s (each ≈%dpx square on screen). "+
				"Pick the cell containing your target, then screen_zoom cell=... for precision, then mouse cell=... to click.",
			raw.Bounds().Dx(), raw.Bounds().Dy(), grid.lastCell(), cellNative),
		ForUser: "👁 screen view",
		Images:  []provider.ImagePart{part},
	}
}

// ---- screen_zoom -----------------------------------------------------------

type screenZoomTool struct{ *deps }

func (t *screenZoomTool) Name() string { return "screen_zoom" }
func (t *screenZoomTool) Description() string {
	return "Magnify part of the last screen_view: pass cell (a grid cell like D4) or a pixel region (x, y, width, height — e.g. a window's geometry from window_list). " +
		"Attaches the enlarged crop under a finer grid; mouse cell=... then resolves on THIS zoomed view until the next screen_view. " +
		"Zooms the already-captured frame — run screen_view first for a fresh look."
}
func (t *screenZoomTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cell":   map[string]any{"type": "string", "description": "Grid cell from the last screen_view, e.g. D4"},
			"x":      map[string]any{"type": "integer", "description": "Region: left edge in screen pixels (with y/width/height, instead of cell)"},
			"y":      map[string]any{"type": "integer", "description": "Region: top edge in screen pixels"},
			"width":  map[string]any{"type": "integer", "description": "Region: width in pixels"},
			"height": map[string]any{"type": "integer", "description": "Region: height in pixels"},
		},
	}
}

func (t *screenZoomTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	raw, viewScale, grid, ok := t.vision.snapshot()
	if !ok {
		return tools.Errorf("no frame to zoom — run screen_view first")
	}

	var rect image.Rectangle
	var what string
	if cell := strings.TrimSpace(tools.StringArg(args, "cell")); cell != "" {
		r, err := grid.cellRectByName(cell)
		if err != nil {
			return tools.Errorf("%v", err)
		}
		// Include a margin so a target straddling the cell edge stays whole,
		// and translate from view pixels to native pixels.
		margin := grid.CellSize / 4
		rect = image.Rect(
			int(float64(r.Min.X-margin)/viewScale), int(float64(r.Min.Y-margin)/viewScale),
			int(float64(r.Max.X+margin)/viewScale), int(float64(r.Max.Y+margin)/viewScale),
		)
		what = "cell " + strings.ToUpper(cell)
	} else if w, h := tools.IntArg(args, "width", 0), tools.IntArg(args, "height", 0); w > 0 && h > 0 {
		x, y := tools.IntArg(args, "x", 0), tools.IntArg(args, "y", 0)
		rect = image.Rect(x, y, x+w, y+h)
		what = fmt.Sprintf("region %dx%d at %d,%d", w, h, x, y)
	} else {
		return tools.Errorf("pass cell (like D4) or a region (x, y, width, height)")
	}

	zoomed, z, err := buildZoom(raw, rect)
	if err != nil {
		return tools.Errorf("screen_zoom: %v", err)
	}
	part, err := pngPart(zoomed)
	if err != nil {
		return tools.Errorf("screen_zoom: encode: %v", err)
	}
	t.vision.setZoom(z)

	return &tools.Result{
		ForLLM: fmt.Sprintf(
			"Attached: %s magnified ×%.1f (screen area %d,%d %dx%d px) under a finer grid, cells A1-%s. "+
				"mouse cell=... now targets this zoomed view; a new screen_view returns to the full screen.",
			what, z.Scale, z.Crop.Min.X, z.Crop.Min.Y, z.Crop.Dx(), z.Crop.Dy(), z.Grid.lastCell()),
		ForUser: "🔎 zoom: " + what,
		Images:  []provider.ImagePart{part},
	}
}
