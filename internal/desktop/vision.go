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
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
	"github.com/cyqlelabs/factor/internal/trace"
)

// visionState is one session's perception state: the last native-resolution
// frame plus the geometry needed to translate grid cells (on the possibly
// downscaled view, or on a zoomed crop) back to screen pixels, and the tree
// read beside it.
//
// It is per session, not per desktop. A cell is only meaningful against the
// picture the model was looking at when it named it, and two conversations
// driving one screen — a Telegram turn and a spoken one — each have their
// own picture: shared, a zoom in one moved every click in the other onto the
// zoomed grid.
type visionState struct {
	mu sync.Mutex
	// frame numbers the full-screen image; zoomFrame the zoomed crop of it
	// currently attached, if any. A cell or ref may be qualified with either
	// ("f3:D4") to say which picture it was read off.
	frame     int
	zoomFrame int
	tree      *Tree
	raw       *image.RGBA // capture-resolution frame from the last screen_view
	viewScale float64     // view px per capture px (≤ 1)
	// pointScale is capture px per unit of the pointer's own coordinate
	// space. It is 1 wherever a screenshot is measured in the same units the
	// mouse is moved in, and 2 on a Retina Mac, where screencapture writes the
	// backing store — twice the desktop in each direction — while cliclick and
	// AppleScript take points. Without it every click computed from a frame
	// landed at twice its coordinates, which put three quarters of the screen
	// out of reach and every hit in the wrong place.
	pointScale float64
	grid       gridLayout // over the view image
	zoom       *zoomView  // nil until screen_zoom; cleared by screen_view
}

func (v *visionState) setView(raw *image.RGBA, scale, pointScale float64, grid gridLayout) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if pointScale <= 0 {
		pointScale = 1
	}
	v.raw, v.viewScale, v.pointScale, v.grid, v.zoom = raw, scale, pointScale, grid, nil
	v.zoomFrame, v.tree = 0, nil
}

// setFrame numbers the view just set and attaches the tree read beside it.
func (v *visionState) setFrame(frame int, tree *Tree) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.frame, v.tree = frame, tree
	if tree != nil {
		tree.Frame = frame
	}
}

// setTree makes a tree read without a picture the session's latest look:
// desktop_run ends on one, and its refs must work with element= straight
// away. The old picture goes with it — a cell named on it would point into a
// window that has changed since — so a cell needs a fresh screen_view.
func (v *visionState) setTree(frame int, tree *Tree) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.frame, v.tree, v.zoomFrame, v.zoom, v.raw = frame, tree, 0, nil, nil
	tree.Frame = frame
}

func (v *visionState) setZoom(z zoomView) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.zoom = &z
}

func (v *visionState) setZoomFrame(frame int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.zoomFrame = frame
}

// snapshot returns the current frame and geometry for a zoom pass.
func (v *visionState) snapshot() (*image.RGBA, float64, gridLayout, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.raw, v.viewScale, v.grid, v.raw != nil
}

// current reports the latest frame, its tree, and the scales that map the
// tree's rectangles onto the view.
func (v *visionState) current() (frame int, tree *Tree, viewScale, pointScale float64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.frame, v.tree, v.viewScale, v.pointScale
}

// qualifier splits "f3:D4" into the frame it names and the rest. A bare name
// has frame 0, which means "whatever is attached now".
func qualifier(name string) (int, string, error) {
	name = strings.TrimSpace(name)
	head, rest, ok := strings.Cut(name, ":")
	if !ok {
		return 0, name, nil
	}
	head = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(head)), "f")
	n := atoi(head)
	if n <= 0 || head == "" {
		return 0, "", fmt.Errorf("%q does not name a frame (expected something like f3:D4)", name)
	}
	return n, strings.TrimSpace(rest), nil
}

// staleFrame is the refusal for a name read off a picture that is no longer
// the latest one.
func (v *visionState) staleFrame(n int) error {
	return fmt.Errorf("f%d is not the latest look (that is f%d) — the screen may have changed since, so nothing was done; run screen_view and name a cell or element on the new picture", n, v.latestFrame())
}

func (v *visionState) latestFrame() int {
	if v.zoomFrame > v.frame {
		return v.zoomFrame
	}
	return v.frame
}

// element finds a listed control by ref, refusing one from an older frame.
func (v *visionState) element(ref string) (Element, *Tree, error) {
	n, bare, err := qualifier(ref)
	if err != nil {
		return Element{}, nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.tree == nil || len(v.tree.Elements) == 0 {
		return Element{}, nil, fmt.Errorf("no accessibility tree to take %s from — screen_view lists the controls it can see; point with cell= otherwise", ref)
	}
	if n != 0 && n != v.frame && n != v.zoomFrame {
		return Element{}, nil, v.staleFrame(n)
	}
	el, ok := v.tree.Find(strings.ToLower(bare))
	if !ok {
		return Element{}, nil, fmt.Errorf("%s is not one of the controls listed on f%d (e1-e%d)", bare, v.frame, len(v.tree.Elements))
	}
	return el, v.tree, nil
}

// toView maps a rectangle in pointer units onto the full-screen view image.
func (v *visionState) toView(r image.Rectangle) image.Rectangle {
	k := v.pointScale * v.viewScale
	if k <= 0 {
		k = 1
	}
	return image.Rect(int(float64(r.Min.X)*k), int(float64(r.Min.Y)*k), int(float64(r.Max.X)*k), int(float64(r.Max.Y)*k))
}

// cellOf names the grid cell an element's center falls in on the full view.
func (v *visionState) cellOf(t *Tree, e Element) string {
	if !t.Positioned || !e.HasBounds() {
		return "--"
	}
	r := v.toView(e.Bounds)
	c := image.Point{X: (r.Min.X + r.Max.X) / 2, Y: (r.Min.Y + r.Max.Y) / 2}
	if c.X < 0 || c.Y < 0 || c.X >= v.grid.W || c.Y >= v.grid.H || v.grid.CellSize <= 0 {
		return "off-screen"
	}
	return fmt.Sprintf("%s%d", columnLabel(c.X/v.grid.CellSize), c.Y/v.grid.CellSize+1)
}

// resolveCell maps a cell named on the most recent attached image — the
// zoomed crop when one is active, the full-screen grid otherwise — to the
// native screen pixel at its center. A cell qualified with a frame resolves
// on that frame's own grid, whichever is attached.
func (v *visionState) resolveCell(coord string) (image.Point, string, error) {
	n, coord, err := qualifier(coord)
	if err != nil {
		return image.Point{}, "", err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.raw == nil {
		return image.Point{}, "", fmt.Errorf("no grid view yet — run screen_view first")
	}
	if n != 0 && n != v.frame && n != v.zoomFrame {
		return image.Point{}, "", v.staleFrame(n)
	}
	// A bare cell names whatever is attached now; a qualified one names its
	// own frame, so f3:D4 resolves on the full grid with a zoom of f3 up.
	if v.zoom != nil && (n == 0 || n == v.zoomFrame) {
		p, err := v.zoom.cellCenterNative(coord)
		if err != nil {
			return image.Point{}, "", fmt.Errorf("%v (on the current zoomed view; screen_view resets to the full screen)", err)
		}
		return v.toPointer(p), "zoomed cell", nil
	}
	r, err := v.grid.cellRectByName(coord)
	if err != nil {
		return image.Point{}, "", err
	}
	cx := float64(r.Min.X+r.Max.X) / 2
	cy := float64(r.Min.Y+r.Max.Y) / 2
	return v.toPointer(image.Point{X: int(cx / v.viewScale), Y: int(cy / v.viewScale)}), "cell", nil
}

// toPointer converts a pixel in the captured frame to the coordinate the
// pointer is moved in. Caller holds the lock.
func (v *visionState) toPointer(p image.Point) image.Point {
	if v.pointScale <= 1 {
		return p
	}
	return image.Point{X: int(float64(p.X) / v.pointScale), Y: int(float64(p.Y) / v.pointScale)}
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

// visionStore holds one visionState per session. Frames are numbered across
// all of them, so a qualified name from one conversation can never be taken
// for a picture in another.
type visionStore struct {
	mu     sync.Mutex
	frames int
	byKey  map[string]*visionState
	order  []string // least recently used first
}

// maxVisionSessions bounds how many conversations keep a picture. Each holds a
// full-resolution frame, and a session that has not looked at the screen in
// this long is not about to click on what it saw.
const maxVisionSessions = 8

func newVisionStore() *visionStore { return &visionStore{byKey: map[string]*visionState{}} }

// forCtx returns the state of the session the tool call belongs to.
func (s *visionStore) forCtx(ctx context.Context) *visionState {
	key := tools.ToolContextFrom(ctx).SessionKey
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.order {
		if k == key {
			s.order = append(append(s.order[:i:i], s.order[i+1:]...), key)
			return s.byKey[key]
		}
	}
	v := &visionState{}
	s.byKey[key] = v
	s.order = append(s.order, key)
	if len(s.order) > maxVisionSessions {
		delete(s.byKey, s.order[0])
		s.order = s.order[1:]
	}
	return v
}

// nextFrame hands out the next frame number.
func (s *visionStore) nextFrame() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames++
	return s.frames
}

// ---- screen_view -----------------------------------------------------------

type screenViewTool struct{ *deps }

func (t *screenViewTool) Name() string { return "screen_view" }
func (t *screenViewTool) Description() string {
	return "Look at the screen: capture it and attach the image with a battleship coordinate grid (columns A,B,C…, rows 1,2,3…) overlaid. " +
		"Where the application publishes an accessibility tree, its controls are also outlined and tagged on the image (e1, e2…) and listed with their names and states: act on those with element= — exact, checked against the live window, and usually without moving the pointer. " +
		"For anything else, point with the grid: small or dense targets deserve a screen_zoom on their cell first (finer grid, better precision); then click with mouse cell=... . " +
		"Run it again after acting to see the result. Needs a vision-capable model. " +
		"This is for native applications. For anything in a web page use the browser tools instead — they read the page's own elements, cost a fraction of a screenshot, and are not disturbed by the user moving the mouse or changing windows."
}
func (t *screenViewTool) Parameters() map[string]any {
	props := map[string]any{}
	if t.access != nil {
		props["window"] = map[string]any{"type": "string", "description": "Read the accessibility tree of this window instead of the one in front: " + windowArgDoc}
	}
	return map[string]any{"type": "object", "properties": props}
}

func (t *screenViewTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	trees := t.readTree(ctx, tools.StringArg(args, "window"))
	raw, err := captureFrame(ctx, t.ctl)
	if err != nil {
		<-trees // the read must not outlive the call that asked for it
		return tools.Errorf("screen_view: %v", err)
	}
	view, scale := fitForModel(raw)
	grid := layoutFor(view.Bounds().Dx(), view.Bounds().Dy(), gridDivisions)
	drawGrid(view, grid)
	pointScale := captureScale(ctx, t.ctl, t.env, raw)
	state := t.vision.forCtx(ctx)
	state.setView(raw, scale, pointScale, grid)

	tree := <-trees
	frame := t.vision.nextFrame()
	state.setFrame(frame, tree)
	var summary string
	if tree != nil {
		screen := image.Rect(0, 0, int(float64(raw.Bounds().Dx())/pointScale), int(float64(raw.Bounds().Dy())/pointScale))
		tree.finish(screen)
		var items []overlayItem
		if tree.Positioned {
			for _, e := range tree.Elements {
				if e.HasBounds() {
					items = append(items, overlayItem{rect: state.toView(e.Bounds), tag: e.Ref})
				}
			}
		}
		drawElements(view, items)
		summary = treeSummary(tree, func(e Element) string { return state.cellOf(tree, e) })
		noteTree(ctx, tree)
	}
	part, err := pngPart(view)
	if err != nil {
		return tools.Errorf("screen_view: encode: %v", err)
	}

	// Reported in the pointer's units rather than the capture's: they are the
	// same number everywhere but a Retina Mac, and there a screen announced as
	// 2880x1800 invites coordinates half the desktop past its right edge.
	screenW := int(float64(raw.Bounds().Dx()) / pointScale)
	screenH := int(float64(raw.Bounds().Dy()) / pointScale)
	cellNative := int(float64(grid.CellSize) / scale / pointScale)
	return &tools.Result{
		ForLLM: fmt.Sprintf(
			"Screen is %dx%d px. Attached: the current screen with grid cells A1-%s (each ≈%dpx square on screen). "+
				"Pick the cell containing your target, then screen_zoom cell=... for precision, then mouse cell=... to click.%s%s",
			screenW, screenH, grid.lastCell(), cellNative, emptyScreenHint(t.env), summary),
		ForUser: "👁 screen view",
		Images:  []provider.ImagePart{part},
	}
}

// readTree starts reading the accessibility tree beside the capture, so the
// read costs no time the screenshot was not already taking. The channel
// yields nil when there is no backend, and a tree with a Note when the read
// failed: a failed read is something to say, not a reason to fail the look.
func (t *screenViewTool) readTree(ctx context.Context, window string) <-chan *Tree {
	out := make(chan *Tree, 1)
	if t.access == nil {
		out <- nil
		return out
	}
	go func() {
		var target *Window
		if window != "" {
			w, err := t.resolveWindow(ctx, window)
			if err != nil {
				out <- &Tree{Backend: t.access.Name(), Note: err.Error()}
				return
			}
			target = &w
		}
		rctx, cancel := context.WithTimeout(ctx, readBudget(t.access))
		defer cancel()
		started := time.Now()
		tree, err := t.access.Read(rctx, target)
		if err != nil {
			out <- &Tree{Backend: t.access.Name(), Note: err.Error(), Took: time.Since(started)}
			return
		}
		tree.Took = time.Since(started)
		out <- tree
	}()
	return out
}

// noteTree records on the turn's trace how good the tree was, per
// application: which programs on this machine publish a tree worth leaning
// on is something only the user's own traces can say.
func noteTree(ctx context.Context, t *Tree) {
	app := t.App
	if app == "" {
		app = "?"
	}
	trace.TurnFrom(ctx).Event(trace.EventAccessibility, fmt.Sprintf("backend=%s app=%s quality=%s controls=%d nodes=%d complete=%t ms=%d",
		t.Backend, app, t.Quality(), len(t.Elements), t.Nodes, t.Complete, t.Took.Milliseconds()))
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
	state := t.vision.forCtx(ctx)
	raw, viewScale, grid, ok := state.snapshot()
	if !ok {
		return tools.Errorf("no frame to zoom — run screen_view first")
	}

	var rect image.Rectangle
	var what string
	if cell := strings.TrimSpace(tools.StringArg(args, "cell")); cell != "" {
		_, bare, err := qualifier(cell)
		if err != nil {
			return tools.Errorf("%v", err)
		}
		r, err := grid.cellRectByName(bare)
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
		what = "cell " + strings.ToUpper(bare)
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
	frame, tree, _, pointScale := state.current()
	tags := zoomTags(zoomed, z, tree, pointScale)
	part, err := pngPart(zoomed)
	if err != nil {
		return tools.Errorf("screen_zoom: encode: %v", err)
	}
	state.setZoom(z)
	zoomFrame := t.vision.nextFrame()
	state.setZoomFrame(zoomFrame)

	note := ""
	if tags != "" {
		note = " Tagged here from f" + fmt.Sprint(frame) + "'s accessibility tree: " + tags + " — element= acts on them directly."
	}
	return &tools.Result{
		ForLLM: fmt.Sprintf(
			"Attached: %s magnified ×%.1f (screen area %d,%d %dx%d px) under a finer grid, cells A1-%s (frame f%d). "+
				"mouse cell=... now targets this zoomed view; a new screen_view returns to the full screen.%s",
			what, z.Scale, z.Crop.Min.X, z.Crop.Min.Y, z.Crop.Dx(), z.Crop.Dy(), z.Grid.lastCell(), zoomFrame, note),
		ForUser: "🔎 zoom: " + what,
		Images:  []provider.ImagePart{part},
	}
}

// zoomTags draws the tree's controls that fall inside a zoomed crop onto it
// and names them, so a small control the zoom was asked for is tagged at the
// size it is now drawn.
func zoomTags(img *image.RGBA, z zoomView, tree *Tree, pointScale float64) string {
	if tree == nil || !tree.Positioned {
		return ""
	}
	if pointScale <= 0 {
		pointScale = 1
	}
	var items []overlayItem
	var refs []string
	for _, e := range tree.Elements {
		if !e.HasBounds() {
			continue
		}
		native := image.Rect(int(float64(e.Bounds.Min.X)*pointScale), int(float64(e.Bounds.Min.Y)*pointScale),
			int(float64(e.Bounds.Max.X)*pointScale), int(float64(e.Bounds.Max.Y)*pointScale))
		if !native.Overlaps(z.Crop) {
			continue
		}
		r := native.Sub(z.Crop.Min)
		r = image.Rect(int(float64(r.Min.X)*z.Scale), int(float64(r.Min.Y)*z.Scale), int(float64(r.Max.X)*z.Scale), int(float64(r.Max.Y)*z.Scale))
		items = append(items, overlayItem{rect: r, tag: e.Ref})
		refs = append(refs, e.Ref)
	}
	drawElements(img, items)
	return strings.Join(refs, ", ")
}

// captureScale reports how many pixels of a screenshot cover one unit of the
// coordinate space the pointer moves in.
//
// They are the same thing on every platform but macOS, where screencapture
// writes the backing store — on a Retina display twice the desktop in each
// direction — while cliclick and System Events take points. Nothing in a
// frame says which it is, so the capture is measured against the screen size
// the controller reports, which is in the pointer's own units.
//
// It is only asked on darwin. Elsewhere the two are equal by construction, and
// a screen size that disagrees with the capture there means something else
// entirely — an xrandr reporting one monitor of several, say — which this must
// not read as a scale factor and silently halve every click by.
func captureScale(ctx context.Context, ctl Controller, env Env, raw *image.RGBA) float64 {
	if env.GOOS != "darwin" {
		return 1
	}
	w, _, err := ctl.ScreenSize(ctx)
	if err != nil || w <= 0 {
		return 1
	}
	scale := float64(raw.Bounds().Dx()) / float64(w)
	// Apple has shipped 1x and 2x, and rounds to whole factors; anything else
	// is a screen that moved between the two calls or a size read wrongly, and
	// a fraction of a pixel is not worth moving every click by.
	if scale < 1.5 {
		return 1
	}
	if scale > 3 {
		return 1
	}
	return 2
}

// emptyScreenHint is what to make of a macOS capture with nothing on it.
//
// Since macOS 10.15 a process without Screen Recording permission does not
// fail: screencapture exits 0 and writes a picture of the wallpaper with every
// window removed. Nothing in the result says so, so the model reads a desktop
// with six applications open as an empty one and reports that to the user with
// a screenshot to back it up. The permission is the first thing to suspect,
// and only this platform can produce the symptom.
func emptyScreenHint(env Env) string {
	if env.GOOS != "darwin" {
		return ""
	}
	return " If the screen appears to have no windows on it, macOS is withholding Screen Recording " +
		"permission rather than showing you an empty desktop: ask the user to grant it in " +
		"System Settings → Privacy & Security → Screen Recording."
}
