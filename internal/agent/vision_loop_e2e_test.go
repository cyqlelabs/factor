package agent

// Full-stack end-to-end for grid vision, with no display and no network
// beyond loopback, so it runs everywhere the unit tests do — including the CI
// boxes where the live X tests skip.
//
// Everything between the user's sentence and the pointer is real: the real
// agent loop, the real tool registry, the real desktop tools, and a real
// provider speaking the real OpenAI wire format over a local HTTP server. Only
// the two ends are simulated — the machine (a scripted screen and a pointer
// that remembers where it was put) and the model.
//
// The model is the interesting part: it does not know any coordinates. It
// decodes the PNG that actually arrived on the wire, finds the target by
// color, works out which cell holds it by reading the drawn grid lines, and
// names that cell. So the assertion at the end — the pointer sits on the
// target — can only hold if the overlay, the wire encoding, the loop's image
// handling and the coordinate translation are all right together.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/desktop"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/session"
	"github.com/cyqlelabs/factor/internal/skills"
	"github.com/cyqlelabs/factor/internal/tools"
)

// The scripted screen is wider than the image budget on purpose: the model
// sees a shrunken frame, so a click that ignored the scale would miss.
const (
	e2eScreenW, e2eScreenH = 2560, 1440
	e2eTargetX, e2eTargetY = 1800, 980
	e2eTargetW, e2eTargetH = 120, 90
)

func e2eTarget() image.Rectangle {
	return image.Rect(e2eTargetX, e2eTargetY, e2eTargetX+e2eTargetW, e2eTargetY+e2eTargetH)
}

// fakeScreen is the machine under the desktop tools: it renders a screenshot
// on demand and remembers where the pointer was moved.
type fakeScreen struct {
	mu      sync.Mutex
	pointer image.Point
	moves   int
	clicks  int
}

// render draws the screen: a dark background with one solid magenta block.
func (s *fakeScreen) render() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, e2eScreenW, e2eScreenH))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{28, 30, 34, 255}), image.Point{}, draw.Src)
	draw.Draw(img, e2eTarget(), image.NewUniform(color.RGBA{255, 0, 255, 255}), image.Point{}, draw.Src)
	return img
}

// env exposes the scripted machine as the desktop package's environment seam,
// so the real controller and the real tools run against it unchanged.
func (s *fakeScreen) env() desktop.Env {
	return desktop.Env{
		GOOS:   "linux",
		Getenv: func(k string) string { return map[string]string{"DISPLAY": ":0"}[k] },
		Has:    func(bin string) bool { return bin == "scrot" || bin == "xdotool" },
		Run: func(_ context.Context, _ string, argv ...string) (string, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			switch {
			case argv[0] == "scrot":
				var buf bytes.Buffer
				if err := png.Encode(&buf, s.render()); err != nil {
					return "", err
				}
				return "", os.WriteFile(argv[len(argv)-1], buf.Bytes(), 0o644)
			case argv[0] == "xdotool" && argv[1] == "mousemove":
				s.moves++
				s.pointer = image.Pt(atoiTest(argv[2]), atoiTest(argv[3]))
				return "", nil
			case argv[0] == "xdotool" && argv[1] == "click":
				s.clicks++
				return "", nil
			case argv[0] == "xdotool" && argv[1] == "getdisplaygeometry":
				return fmt.Sprintf("%d %d\n", e2eScreenW, e2eScreenH), nil
			}
			return "", nil
		},
	}
}

func (s *fakeScreen) pointerAt() image.Point {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pointer
}

func atoiTest(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// --- the model ---------------------------------------------------------------

// visionModel is a fake LLM that can only see. Given a picture it finds the
// magenta block, reads the grid lines off the image, and names the cell — the
// same job a real vision model does, done deterministically.
type visionModel struct {
	t *testing.T
	// straightToClick makes the model click from the overview instead of
	// zooming first — the shorter route a model takes at a target big enough
	// to hit without refining, and the one that leans entirely on the
	// overview frame's scale being undone correctly.
	straightToClick bool
	mu              sync.Mutex
	steps           []string // the tools it asked for, in order
}

// findMagenta returns the bounding box of the block as drawn. Plain int
// accumulators: image.Rect canonicalizes, so an inverted seed rectangle would
// come back as the whole image and hide a miss.
func findMagenta(img image.Image) (image.Rectangle, bool) {
	b := img.Bounds()
	minX, minY, maxX, maxY := b.Max.X, b.Max.Y, b.Min.X, b.Min.Y
	found := false
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r>>8 > 200 && g>>8 < 60 && bl>>8 > 200 {
				found = true
				minX, minY = min(minX, x), min(minY, y)
				maxX, maxY = max(maxX, x+1), max(maxY, y+1)
			}
		}
	}
	if !found {
		return image.Rectangle{}, false
	}
	return image.Rect(minX, minY, maxX, maxY), true
}

// cellUnder names the grid cell containing p by finding the overlay's bright
// lines in the picture — never by asking the code where it drew them.
func cellUnder(t *testing.T, img image.Image, p image.Point) string {
	t.Helper()
	b := img.Bounds()
	isLine := func(x, y int) bool {
		r, g, bl, _ := img.At(x, y).RGBA()
		return r>>8 < 40 && g>>8 > 200 && bl>>8 > 200 // the cyan core
	}
	scan := func(n, across int, at func(i, j int) bool) []int {
		var starts []int
		run := false
		for i := 0; i < n; i++ {
			hits := 0
			for j := 0; j < across; j++ {
				if at(i, j) {
					hits++
				}
			}
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
	xs := scan(b.Dx(), b.Dy(), func(x, y int) bool { return isLine(b.Min.X+x, b.Min.Y+y) })
	ys := scan(b.Dy(), b.Dx(), func(y, x int) bool { return isLine(b.Min.X+x, b.Min.Y+y) })
	if len(xs) < 2 || len(ys) < 2 {
		t.Fatalf("no grid in the attached image (%d vertical, %d horizontal lines)", len(xs), len(ys))
	}
	index := func(starts []int, v int) int {
		i := 0
		for k, s := range starts {
			if v >= s {
				i = k
			}
		}
		return i
	}
	col, row := index(xs, p.X), index(ys, p.Y)
	label, n := "", col+1
	for n > 0 {
		n--
		label = string(rune('A'+n%26)) + label
		n /= 26
	}
	return fmt.Sprintf("%s%d", label, row+1)
}

// lastImage pulls the newest attached image out of an OpenAI-format request,
// which is where the loop must have put the tool's picture for a real model
// to ever see it.
func lastImage(t *testing.T, body map[string]any) (image.Image, bool) {
	t.Helper()
	messages, _ := body["messages"].([]any)
	for i := len(messages) - 1; i >= 0; i-- {
		msg, _ := messages[i].(map[string]any)
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, raw := range parts {
			part, _ := raw.(map[string]any)
			if part["type"] != "image_url" {
				continue
			}
			url, _ := part["image_url"].(map[string]any)["url"].(string)
			_, encoded, found := strings.Cut(url, ";base64,")
			if !found {
				t.Fatalf("image_url is not a base64 data URI: %.60q", url)
			}
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatalf("attached image is not valid base64: %v", err)
			}
			img, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("attached image is not a decodable picture: %v", err)
			}
			return img, true
		}
	}
	return nil, false
}

// serve is the model's turn: look at what arrived, decide the next tool call.
// It walks the same path a real run does — look, zoom for precision, click,
// then answer.
func (m *visionModel) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		m.t.Errorf("decode request: %v", err)
		return
	}
	img, hasImage := lastImage(m.t, body)

	m.mu.Lock()
	seen := len(m.steps)
	m.mu.Unlock()

	var name string
	args := map[string]any{}
	switch {
	case !hasImage:
		// Nothing seen yet: look at the screen.
		name = "screen_view"
	case seen == 1:
		// The overview arrived: find the target, then either click it
		// directly or zoom its cell for precision first.
		box, ok := findMagenta(img)
		if !ok {
			m.t.Errorf("the target is not visible in the overview the agent sent")
			return
		}
		cell := cellUnder(m.t, img, image.Pt((box.Min.X+box.Max.X)/2, (box.Min.Y+box.Max.Y)/2))
		if m.straightToClick {
			name, args["action"], args["cell"] = "mouse", "click", cell
			break
		}
		name, args["cell"] = "screen_zoom", cell
	case seen == 2 && !m.straightToClick:
		// The zoom arrived: click the sub-cell holding the target.
		box, ok := findMagenta(img)
		if !ok {
			m.t.Errorf("the target is not visible in the zoomed crop the agent sent")
			return
		}
		name = "mouse"
		args["action"] = "click"
		args["cell"] = cellUnder(m.t, img, image.Pt((box.Min.X+box.Max.X)/2, (box.Min.Y+box.Max.Y)/2))
	default:
		writeJSON(w, map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"role": "assistant", "content": "clicked it"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		})
		return
	}

	m.mu.Lock()
	m.steps = append(m.steps, name)
	id := fmt.Sprintf("call_%d", len(m.steps))
	m.mu.Unlock()

	encoded, err := json.Marshal(args)
	if err != nil {
		m.t.Errorf("marshal args: %v", err)
		return
	}
	writeJSON(w, map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": string(encoded)}},
			}},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
	})
}

func writeJSON(w http.ResponseWriter, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// --- the test ----------------------------------------------------------------

// visionRig wires the real loop, registry, desktop tools and provider around
// the scripted screen and the fake model.
type visionRig struct {
	loop   *Loop
	store  *session.Store
	screen *fakeScreen
	model  *visionModel
	dir    string
}

func newVisionRig(t *testing.T, straightToClick bool) *visionRig {
	t.Helper()
	screen := &fakeScreen{}
	model := &visionModel{t: t, straightToClick: straightToClick}
	srv := httptest.NewServer(http.HandlerFunc(model.serve))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.Agent.Workspace = t.TempDir()
	cfg.Agent.MaxToolIterations = 8
	if err := config.EnsureWorkspace(cfg.Agent.Workspace); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(cfg.Agent.Workspace, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry(cfg.Tools.IsToolEnabled, nil)
	registry.Register(desktop.NewTools(screen.env(),
		tools.NewPathGuard(cfg.Agent.Workspace, true, false, nil),
		filepath.Join(cfg.Agent.Workspace, "screenshots"))...)
	builder := NewContextBuilder(cfg, skills.NewLoader(filepath.Join(cfg.Agent.Workspace, "skills")), nil)

	return &visionRig{
		loop: NewLoop(cfg, bus.New(), provider.NewOpenAI(srv.URL, "k", "m"),
			registry, store, builder, (*memory.Ambient)(nil)),
		store:  store,
		screen: screen,
		model:  model,
		dir:    cfg.Agent.Workspace,
	}
}

func (r *visionRig) steps() []string {
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	return append([]string(nil), r.model.steps...)
}

// TestVisionLoopClicksWhatTheModelSees runs a whole turn: the user asks, the
// model looks, zooms, and clicks, and the pointer ends up on the target.
func TestVisionLoopClicksWhatTheModelSees(t *testing.T) {
	rig := newVisionRig(t, false)

	reply, err := rig.loop.ProcessDirect(context.Background(), "click the magenta block", "cli:vision")
	if err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	if reply != "clicked it" {
		t.Errorf("reply = %q", reply)
	}

	// The model asked for exactly the loop the tools describe.
	if want := []string{"screen_view", "screen_zoom", "mouse"}; !equalStrings(rig.steps(), want) {
		t.Fatalf("tool sequence = %v, want %v", rig.steps(), want)
	}

	// The payoff: a model that only ever named cells moved the real pointer
	// onto the target, across a screen too big to send at native size.
	if got := rig.screen.pointerAt(); !got.In(e2eTarget()) {
		t.Fatalf("pointer ended at %v, outside the target %v", got, e2eTarget())
	}
	if rig.screen.clicks != 1 {
		t.Errorf("clicks = %d, want exactly 1", rig.screen.clicks)
	}
}

// TestVisionLoopClicksStraightFromTheOverview takes the shorter route: no zoom,
// so the click rests entirely on undoing the overview frame's downscale. A
// target far from the origin makes that undoing matter — near (0,0) frame and
// screen pixels nearly agree, and a missing scale would pass unnoticed.
func TestVisionLoopClicksStraightFromTheOverview(t *testing.T) {
	rig := newVisionRig(t, true)

	if _, err := rig.loop.ProcessDirect(context.Background(), "click the magenta block", "cli:direct"); err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	if want := []string{"screen_view", "mouse"}; !equalStrings(rig.steps(), want) {
		t.Fatalf("tool sequence = %v, want %v", rig.steps(), want)
	}
	if got := rig.screen.pointerAt(); !got.In(e2eTarget()) {
		t.Fatalf("pointer ended at %v, outside the target %v at %dx%d native",
			got, e2eTarget(), e2eScreenW, e2eScreenH)
	}
}

// TestVisionLoopKeepsHistoryFreeOfImages checks the other half of the
// contract: pictures reach the model, but never the session file, which is
// what keeps a long desktop session cheap to store and replay.
func TestVisionLoopKeepsHistoryFreeOfImages(t *testing.T) {
	rig := newVisionRig(t, false)
	store, cfgDir := rig.store, rig.dir

	if _, err := rig.loop.ProcessDirect(context.Background(), "click the magenta block", "cli:store"); err != nil {
		t.Fatal(err)
	}

	history, err := store.History("cli:store")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range history {
		if len(m.Images) != 0 {
			t.Fatalf("session history kept image bytes: %+v", m)
		}
	}
	// Two pictures were shown, so two placeholders should stand in for them.
	placeholders := 0
	for _, m := range history {
		if strings.Contains(m.Content, "has been dropped to save space") {
			placeholders++
		}
	}
	if placeholders != 2 {
		t.Errorf("history has %d image placeholders, want 2", placeholders)
	}
	raw, err := os.ReadFile(filepath.Join(cfgDir, "sessions", "cli_store.jsonl"))
	if err != nil {
		// The file name is an implementation detail; skip rather than fail.
		t.Logf("session file not readable (%v); the in-memory check above stands", err)
		return
	}
	if len(raw) > 8000 {
		t.Errorf("session file is %d bytes; image payloads are leaking into it", len(raw))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
