package desktop

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/tools"
)

// fakeAccessor scripts an accessibility backend: the tree a read returns, and
// what each action answers.
type fakeAccessor struct {
	tree    *Tree
	readErr error
	reads   []*Window
	// refresh answers Refresh; nil returns the element unchanged.
	refresh func(Element) (Element, error)
	// do answers Do per action kind; nil is a plain success.
	do    map[string]func(Element, Act) (Outcome, error)
	calls []string
}

func (f *fakeAccessor) Name() string { return "fake" }

func (f *fakeAccessor) Read(_ context.Context, target *Window) (*Tree, error) {
	f.reads = append(f.reads, target)
	if f.readErr != nil {
		return nil, f.readErr
	}
	// A fresh copy each time: finish rewrites the slice it is handed.
	t := *f.tree
	t.Elements = append([]Element(nil), f.tree.Elements...)
	return &t, nil
}

func (f *fakeAccessor) Refresh(_ context.Context, el Element) (Element, error) {
	f.calls = append(f.calls, "refresh "+el.Name)
	if f.refresh != nil {
		return f.refresh(el)
	}
	return el, nil
}

func (f *fakeAccessor) Do(_ context.Context, el Element, act Act) (Outcome, error) {
	f.calls = append(f.calls, act.Kind+" "+el.Name+" "+act.Text)
	if fn := f.do[act.Kind]; fn != nil {
		return fn(el, act)
	}
	return Outcome{After: el}, nil
}

// formTree is a small dialog: a labelled field, a password, a checkbox, two
// buttons and a disabled one, plus a label that names the field.
func formTree() *Tree {
	return &Tree{
		Backend: "fake", App: "editor", Window: "Save As", Positioned: true, Complete: true, WindowID: "0x42",
		Elements: []Element{
			{Role: RoleLabel, Name: "File name:", Bounds: image.Rect(20, 20, 100, 40)},
			{Role: RoleTextField, Value: "notes.txt", Bounds: image.Rect(110, 15, 400, 45), Actions: []string{ActSetText, ActFocus}, Focused: true, Handle: "h1"},
			{Role: RolePassword, Name: "Password", Value: "hunter2", Secret: true, Bounds: image.Rect(110, 60, 400, 90), Actions: []string{ActSetText}, Handle: "h2"},
			{Role: RoleCheckbox, Name: "Keep", Checked: TriOff, Bounds: image.Rect(110, 100, 300, 120), Actions: []string{ActPress}, Handle: "h3"},
			{Role: RoleButton, Name: "Save", Default: true, Bounds: image.Rect(300, 200, 400, 240), Actions: []string{ActPress}, Handle: "h4"},
			{Role: RoleButton, Name: "Cancel", Bounds: image.Rect(180, 200, 280, 240), Actions: []string{ActPress}, Handle: "h5"},
			{Role: RoleButton, Name: "Help", Disabled: true, Bounds: image.Rect(20, 200, 100, 240), Handle: "h6"},
			{Role: "", Name: "panel", Bounds: image.Rect(0, 0, 500, 300)},
		},
	}
}

// labels lists a finished tree as "e1:button \"OK\" …".
func labels(tree *Tree) string {
	var out []string
	for _, e := range tree.Elements {
		out = append(out, e.Ref+":"+e.Label())
	}
	return strings.Join(out, " ")
}

func a11yTools(t *testing.T, m *fakeMachine, acc Accessor) map[string]tools.Tool {
	t.Helper()
	ws := t.TempDir()
	env := m.Env()
	byName := map[string]tools.Tool{}
	for _, tool := range newToolsWith(env, NewController(env), acc, tools.NewPathGuard(ws, true, false, nil), filepath.Join(ws, "shots")) {
		byName[tool.Name()] = tool
	}
	return byName
}

func viewTree(t *testing.T, byName map[string]tools.Tool) *tools.Result {
	t.Helper()
	res := run(t, byName["screen_view"], map[string]any{})
	if res.IsError {
		t.Fatalf("screen_view: %s", res.ForLLM)
	}
	return res
}

func TestScreenViewListsAndTagsTheTree(t *testing.T) {
	m := shotMachine(t, 640, 480)
	acc := &fakeAccessor{tree: formTree()}
	byName := a11yTools(t, m, acc)
	res := viewTree(t, byName)
	for _, want := range []string{
		`Accessibility tree of "Save As" (editor): 6 controls.`,
		`e1 C1 textfield "File name" = "notes.txt" [focused]`,
		`password "Password" = (hidden)`,
		`checkbox "Keep" [unchecked]`,
		`button "Save" [default]`,
		`button "Help" [disabled]`,
		"frame f1",
	} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("listing lacks %q:\n%s", want, res.ForLLM)
		}
	}
	if strings.Contains(res.ForLLM, "hunter2") {
		t.Error("a secret field's contents reached the listing")
	}
	if strings.Contains(res.ForLLM, "panel") {
		t.Error("a container was listed as a control")
	}
	// Row order: the field row first, the buttons row left to right.
	help, cancel, save := strings.Index(res.ForLLM, `"Help"`), strings.Index(res.ForLLM, `"Cancel"`), strings.Index(res.ForLLM, `"Save" [`)
	if help >= cancel || cancel >= save {
		t.Errorf("buttons are not in reading order:\n%s", res.ForLLM)
	}
	img := decodeAttached(t, res.Images[0])
	// The Save button's outline is drawn where the tree put it.
	if c := color.RGBAModel.Convert(img.At(350, 201)).(color.RGBA); c != tagFill {
		t.Errorf("no outline at Save's top edge: %v", c)
	}
}

func TestScreenViewSaysWhenTheTreeIsThinOrMissing(t *testing.T) {
	thin := &Tree{Backend: "fake", App: "game", Window: "Play", Positioned: true, Complete: false, Nodes: 900, Elements: []Element{
		{Role: RoleButton, Name: "Menu", Bounds: image.Rect(0, 0, 50, 20), Actions: []string{ActPress}},
		{Role: RoleButton, Bounds: image.Rect(60, 0, 110, 20), Actions: []string{ActPress}},
	}}
	res := viewTree(t, a11yTools(t, shotMachine(t, 320, 240), &fakeAccessor{tree: thin}))
	for _, want := range []string{"thin — 1 named of 2 controls", "point at anything else with the grid", "stopped at its time limit after 900 nodes", "button (unnamed)"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("thin listing lacks %q:\n%s", want, res.ForLLM)
		}
	}

	res = viewTree(t, a11yTools(t, shotMachine(t, 320, 240), &fakeAccessor{readErr: errors.New("no window in front publishes an accessibility tree")}))
	if !strings.Contains(res.ForLLM, "No accessibility tree for the window in front: no window in front publishes") {
		t.Errorf("a failed read is not explained:\n%s", res.ForLLM)
	}

	empty := &Tree{Backend: "fake", Window: "Canvas", Positioned: true, Complete: true}
	res = viewTree(t, a11yTools(t, shotMachine(t, 320, 240), &fakeAccessor{tree: empty}))
	if !strings.Contains(res.ForLLM, `No accessibility tree for "Canvas": it lists no controls`) {
		t.Errorf("an empty tree is not explained:\n%s", res.ForLLM)
	}

	way := formTree()
	way.Positioned = false
	res = viewTree(t, a11yTools(t, shotMachine(t, 640, 480), &fakeAccessor{tree: way}))
	if !strings.Contains(res.ForLLM, "listed without positions") || !strings.Contains(res.ForLLM, `e1 -- textfield`) {
		t.Errorf("an unpositioned tree is not said to be one:\n%s", res.ForLLM)
	}
}

func TestScreenViewReadsTheWindowAskedFor(t *testing.T) {
	m := shotMachine(t, 320, 240)
	m.installed["wmctrl"] = true
	m.outputs["wmctrl -l -G -p -x"] = wmctrlOutput
	acc := &fakeAccessor{tree: formTree()}
	byName := a11yTools(t, m, acc)
	if _, ok := byName["screen_view"].Parameters()["properties"].(map[string]any)["window"]; !ok {
		t.Fatal("screen_view offers no window argument with a tree to read")
	}
	run(t, byName["screen_view"], map[string]any{"window": "Visual Studio"})
	if len(acc.reads) != 1 || acc.reads[0] == nil || acc.reads[0].ID != "0x03c00003" {
		t.Fatalf("read the wrong window: %+v", acc.reads)
	}
	res := run(t, byName["screen_view"], map[string]any{"window": "no such window"})
	if res.IsError || !strings.Contains(res.ForLLM, `no window matches "no such window"`) {
		t.Fatalf("an unknown window should explain itself, not fail the look: %s", res.ForLLM)
	}
}

func TestMouseElementPressesThroughTheApplication(t *testing.T) {
	m := shotMachine(t, 640, 480)
	acc := &fakeAccessor{tree: formTree(), do: map[string]func(Element, Act) (Outcome, error){
		ActPress: func(el Element, _ Act) (Outcome, error) {
			el.Checked = TriOn
			return Outcome{After: el}, nil
		},
	}}
	byName := a11yTools(t, m, acc)
	viewTree(t, byName)
	m.calls = nil
	res := run(t, byName["mouse"], map[string]any{"action": "click", "element": "e3"})
	if res.IsError || !strings.Contains(res.ForLLM, "pointer was not moved") || !strings.Contains(res.ForLLM, "It is now checked.") {
		t.Fatalf("press: %s", res.ForLLM)
	}
	if len(m.calls) != 0 {
		t.Errorf("a semantic press ran helpers: %v", m.calls)
	}
	if acc.calls[len(acc.calls)-1] != "press Keep " {
		t.Errorf("pressed %v", acc.calls)
	}

	// Pressing OK closes its dialog: gone is success.
	acc.do[ActPress] = func(el Element, _ Act) (Outcome, error) { return Outcome{After: el, Gone: true}, nil }
	res = run(t, byName["mouse"], map[string]any{"action": "click", "element": "f1:e5"})
	if res.IsError || !strings.Contains(res.ForLLM, "It is gone now") {
		t.Fatalf("gone: %s", res.ForLLM)
	}
}

func TestMouseElementRefusesWhatChanged(t *testing.T) {
	m := shotMachine(t, 640, 480)
	acc := &fakeAccessor{tree: formTree(), do: map[string]func(Element, Act) (Outcome, error){
		ActPress: func(Element, Act) (Outcome, error) {
			return Outcome{}, fmt.Errorf("%w: its address now holds button \"Other\"", ErrStale)
		},
	}}
	byName := a11yTools(t, m, acc)
	viewTree(t, byName)
	m.calls = nil
	res := run(t, byName["mouse"], map[string]any{"action": "click", "element": "e5"})
	if !res.IsError || !strings.Contains(res.ForLLM, "no longer there (its address now holds button \"Other\")") || !strings.Contains(res.ForLLM, "nothing was sent") {
		t.Fatalf("stale: %s", res.ForLLM)
	}
	if len(m.calls) != 0 {
		t.Errorf("a stale control still moved the pointer: %v", m.calls)
	}

	for args, want := range map[string]string{
		"e9":       "not one of the controls listed on f1",
		"f7:e1":    "f7 is not the latest look",
		"fx:e1":    "does not name a frame",
		"e4":       "is disabled, so nothing was sent",
		"e1|D4":    "one target, not several",
		"e1|x=1":   "one target, not several",
		"bogus:e1": "does not name a frame",
	} {
		a := map[string]any{"action": "click"}
		ref, extra, _ := strings.Cut(args, "|")
		a["element"] = ref
		switch extra {
		case "D4":
			a["cell"] = "D4"
		case "x=1":
			a["x"], a["y"] = 1, 1
		}
		if res := run(t, byName["mouse"], a); !res.IsError || !strings.Contains(res.ForLLM, want) {
			t.Errorf("%s: %s", args, res.ForLLM)
		}
	}

	// An action whose delivery is unknown is said to be so.
	acc.do[ActPress] = func(Element, Act) (Outcome, error) { return Outcome{}, errors.New("timeout") }
	if res := run(t, byName["mouse"], map[string]any{"action": "click", "element": "e6"}); !res.IsError || !strings.Contains(res.ForLLM, "before trying again") {
		t.Errorf("unknown delivery: %s", res.ForLLM)
	}
}

func TestMouseElementFallsBackToThePointer(t *testing.T) {
	m := shotMachine(t, 640, 480)
	m.installed["wmctrl"] = true
	acc := &fakeAccessor{tree: formTree(), do: map[string]func(Element, Act) (Outcome, error){
		ActPress: func(Element, Act) (Outcome, error) { return Outcome{}, fmt.Errorf("%w: no", ErrRefused) },
	}}
	byName := a11yTools(t, m, acc)
	viewTree(t, byName)
	m.calls = nil
	res := run(t, byName["mouse"], map[string]any{"action": "click", "element": "e5"})
	if res.IsError || !strings.Contains(res.ForLLM, "at 230,220") || !strings.Contains(res.ForLLM, "pointer did it") {
		t.Fatalf("fallback: %s", res.ForLLM)
	}
	if !m.ranWith("wmctrl -i -a 0x42") || !m.ranWith("xdotool mousemove 230 220") {
		t.Errorf("the window was not raised or the pointer not aimed: %v", m.calls)
	}
	// Gestures the application has no action for always use the pointer.
	for _, action := range []string{"double_click", "right_click", "middle_click", "scroll_down", "move"} {
		if res := run(t, byName["mouse"], map[string]any{"action": action, "element": "e6"}); res.IsError {
			t.Errorf("%s: %s", action, res.ForLLM)
		}
	}
	if res := run(t, byName["mouse"], map[string]any{"action": "move", "element": "e6"}); !strings.Contains(res.ForLLM, "Moved the pointer onto e6") {
		t.Errorf("move: %s", res.ForLLM)
	}

	// The pointer path checks the control first, and needs a position.
	acc.refresh = func(Element) (Element, error) { return Element{}, ErrStale }
	if res := run(t, byName["mouse"], map[string]any{"action": "double_click", "element": "e6"}); !res.IsError || !strings.Contains(res.ForLLM, "no longer there") {
		t.Errorf("stale before the pointer: %s", res.ForLLM)
	}
	acc.refresh = func(Element) (Element, error) { return Element{}, errors.New("bus gone") }
	if res := run(t, byName["mouse"], map[string]any{"action": "double_click", "element": "e6"}); !res.IsError || !strings.Contains(res.ForLLM, "could not confirm") {
		t.Errorf("unconfirmed: %s", res.ForLLM)
	}
	acc.refresh = nil
	acc.tree.Positioned = false
	viewTree(t, byName)
	if res := run(t, byName["mouse"], map[string]any{"action": "double_click", "element": "e6"}); !res.IsError || !strings.Contains(res.ForLLM, "no position on screen") {
		t.Errorf("unpositioned: %s", res.ForLLM)
	}
}

func TestMouseElementWithoutATree(t *testing.T) {
	byName := a11yTools(t, shotMachine(t, 320, 240), &fakeAccessor{readErr: errors.New("nope")})
	viewTree(t, byName)
	if res := run(t, byName["mouse"], map[string]any{"action": "click", "element": "e1"}); !res.IsError || !strings.Contains(res.ForLLM, "no accessibility tree to take e1 from") {
		t.Errorf("%s", res.ForLLM)
	}
	// Without a backend the arguments are not even offered.
	plain, _ := newTools(t, shotMachine(t, 320, 240))
	if _, ok := plain["mouse"].Parameters()["properties"].(map[string]any)["element"]; ok {
		t.Error("element= offered with no accessibility backend")
	}
	if strings.Contains(plain["type_text"].Description(), "element=") {
		t.Error("type_text describes element= with no backend")
	}
}

func TestTypeTextElementWritesAndReadsBack(t *testing.T) {
	m := shotMachine(t, 640, 480)
	acc := &fakeAccessor{tree: formTree(), do: map[string]func(Element, Act) (Outcome, error){
		ActSetText: func(el Element, a Act) (Outcome, error) {
			el.Value = a.Text
			return Outcome{After: el}, nil
		},
	}}
	byName := a11yTools(t, m, acc)
	viewTree(t, byName)
	m.calls = nil
	res := run(t, byName["type_text"], map[string]any{"text": "line one\nline two", "element": "e1"})
	if res.IsError || !strings.Contains(res.ForLLM, "no key was pressed") || !strings.Contains(res.ForLLM, "holds exactly that") {
		t.Fatalf("set: %s", res.ForLLM)
	}
	if len(m.calls) != 0 {
		t.Errorf("a direct write ran helpers: %v", m.calls)
	}

	res = run(t, byName["type_text"], map[string]any{"text": "s3cret", "element": "e2"})
	if res.IsError || !strings.Contains(res.ForLLM, "secret field") || strings.Contains(res.ForLLM, "s3cret") {
		t.Fatalf("secret: %s", res.ForLLM)
	}

	acc.do[ActSetText] = func(el Element, a Act) (Outcome, error) {
		el.Value = strings.ToUpper(a.Text)
		return Outcome{After: el}, nil
	}
	if res := run(t, byName["type_text"], map[string]any{"text": "abc", "element": "e1"}); !strings.Contains(res.ForLLM, `now holds "ABC"`) {
		t.Errorf("a changed value is not reported: %s", res.ForLLM)
	}
	acc.do[ActSetText] = func(el Element, a Act) (Outcome, error) { return Outcome{Gone: true}, nil }
	if res := run(t, byName["type_text"], map[string]any{"text": "abc", "element": "e1"}); !strings.Contains(res.ForLLM, "could not be read back") {
		t.Errorf("gone: %s", res.ForLLM)
	}
	acc.do[ActSetText] = func(Element, Act) (Outcome, error) { return Outcome{}, ErrStale }
	if res := run(t, byName["type_text"], map[string]any{"text": "abc", "element": "e1"}); !res.IsError || !strings.Contains(res.ForLLM, "no longer there") {
		t.Errorf("stale: %s", res.ForLLM)
	}
	acc.do[ActSetText] = func(Element, Act) (Outcome, error) { return Outcome{}, errors.New("timeout") }
	if res := run(t, byName["type_text"], map[string]any{"text": "abc", "element": "e1"}); !res.IsError || !strings.Contains(res.ForLLM, "before trying again") {
		t.Errorf("unknown: %s", res.ForLLM)
	}
	if res := run(t, byName["type_text"], map[string]any{"text": "abc", "element": "e4"}); !res.IsError || !strings.Contains(res.ForLLM, "disabled") {
		t.Errorf("disabled: %s", res.ForLLM)
	}
}

func TestTypeTextElementFallsBackToKeystrokes(t *testing.T) {
	m := shotMachine(t, 640, 480)
	acc := &fakeAccessor{tree: formTree(), do: map[string]func(Element, Act) (Outcome, error){
		ActSetText: func(Element, Act) (Outcome, error) { return Outcome{}, ErrRefused },
	}}
	acc.refresh = func(el Element) (Element, error) {
		el.Value = "typed"
		return el, nil
	}
	byName := a11yTools(t, m, acc)
	viewTree(t, byName)
	m.calls = nil
	res := run(t, byName["type_text"], map[string]any{"text": "typed", "element": "e1"})
	if res.IsError || !strings.Contains(res.ForLLM, "by keystroke") || !strings.Contains(res.ForLLM, "holds exactly that") {
		t.Fatalf("keystrokes: %s", res.ForLLM)
	}
	if !m.ranWith("xdotool key --clearmodifiers -- ctrl+a") || !m.ranWith("xdotool type") || m.lastStdin() != "typed" {
		t.Errorf("did not select and type: %v", m.calls)
	}
	if acc.calls[1] != "focus File name " {
		t.Errorf("the field was not focused first: %v", acc.calls)
	}
	if res := run(t, byName["type_text"], map[string]any{"text": "a\nb", "element": "e1"}); !res.IsError || !strings.Contains(res.ForLLM, "a newline") {
		t.Errorf("newline by keystroke: %s", res.ForLLM)
	}
	// A field that cannot be focused directly is clicked.
	acc.do[ActFocus] = func(Element, Act) (Outcome, error) { return Outcome{}, ErrRefused }
	m.calls = nil
	if res := run(t, byName["type_text"], map[string]any{"text": "x", "element": "e1"}); res.IsError || !m.ranWith("xdotool mousemove 255 30") {
		t.Errorf("focus by click: %s %v", res.ForLLM, m.calls)
	}
}

func TestTreeFinishCoversCapsAndNames(t *testing.T) {
	tree := &Tree{Positioned: true, Layers: []image.Rectangle{image.Rect(0, 0, 800, 600), image.Rect(100, 100, 300, 300)}}
	tree.Elements = []Element{
		{Role: RoleButton, Name: "Under", Bounds: image.Rect(150, 150, 200, 180), Actions: []string{ActPress}},
		{Role: RoleButton, Name: "Above", Layer: 1, Bounds: image.Rect(150, 150, 200, 180), Actions: []string{ActPress}},
		{Role: RoleButton, Name: "Away", Bounds: image.Rect(2000, 10, 2050, 30), Actions: []string{ActPress}},
		{Role: RoleListItem, Name: "Static row", Bounds: image.Rect(10, 400, 100, 420)},
		{Role: RoleListItem, Name: "Pick me", Selected: true, Bounds: image.Rect(10, 430, 100, 450)},
		{Role: RoleLabel, Name: "Email", Bounds: image.Rect(400, 10, 500, 30)},
		{Role: RoleTextField, Bounds: image.Rect(400, 35, 600, 60)},
		{Role: RoleSpin, Bounds: image.Rect(700, 35, 750, 60)},
	}
	tree.finish(image.Rect(0, 0, 1024, 768))
	var names []string
	for _, e := range tree.Elements {
		names = append(names, e.Ref+"="+e.Name)
	}
	got := strings.Join(names, " ")
	if got != "e1=Above e2=Email e3= e4=Pick me" {
		t.Fatalf("finished to %q", got)
	}

	// A long listing keeps the focused control.
	tree = &Tree{Positioned: true}
	for i := 0; i < maxListed+5; i++ {
		tree.Elements = append(tree.Elements, Element{Role: RoleButton, Name: fmt.Sprint("b", i), Bounds: image.Rect(0, i*20, 10, i*20+10)})
	}
	tree.Elements[maxListed+3].Focused = true
	tree.finish(image.Rect(0, 0, 100, 5000))
	if len(tree.Elements) != maxListed || tree.Withheld != 5 || !tree.Elements[maxListed-1].Focused {
		t.Fatalf("cap kept %d, withheld %d, last focused %t", len(tree.Elements), tree.Withheld, tree.Elements[maxListed-1].Focused)
	}
}

func TestSameElementAndHelpers(t *testing.T) {
	button := Element{Role: RoleButton, Name: "Save"}
	for _, c := range []struct {
		now  Element
		same bool
	}{
		{Element{Role: RoleButton, Name: "Save", Bounds: image.Rect(5, 5, 9, 9)}, true},
		{Element{Role: RoleButton, Name: "Open"}, false},
		{Element{Role: RoleLink, Name: "Save"}, false},
	} {
		if sameElement(button, c.now) != c.same {
			t.Errorf("sameElement(%v, %v) != %t", button, c.now, c.same)
		}
	}
	if !sameElement(Element{Role: RoleButton, Name: "Play", ID: "toggle"}, Element{Role: RoleButton, Name: "Pause", ID: "toggle"}) {
		t.Error("a toolkit id is identity across a label change")
	}
	if !sameElement(Element{Role: RoleTextField, Name: "Email"}, Element{Role: RoleTextField}) {
		t.Error("a field named from its label is the same field unnamed")
	}
	if got := clipValue(strings.Repeat("ab ", 40)); len([]rune(got)) != 61 || !strings.HasSuffix(got, "…") {
		t.Errorf("clipValue = %q", got)
	}
	e := Element{Role: RoleTreeItem, Expanded: TriOff, ReadOnly: true, Checked: TriOn}
	if got := strings.Join(e.states(), ","); got != "checked,collapsed,read-only" {
		t.Errorf("states = %s", got)
	}
	if (&Tree{}).Quality() != QualityNone || (*Tree)(nil).Quality() != QualityNone {
		t.Error("an empty tree has a quality")
	}
	if _, ok := (*Tree)(nil).Find("e1"); ok {
		t.Error("found in a nil tree")
	}
	if treeSummary(nil, nil) != "" {
		t.Error("nil tree summarized")
	}
	if readBudget(&fakeAccessor{}) != readDeadline || readBudget(newUIA(Env{})) <= readDeadline {
		t.Error("read budgets")
	}
}

// Each conversation keeps its own picture: a zoom in one does not move the
// clicks of another onto the zoomed grid.
func TestVisionIsPerSession(t *testing.T) {
	m := shotMachine(t, 640, 480)
	byName, _ := newTools(t, m)
	ctxA := tools.WithToolContext(context.Background(), tools.ToolContext{SessionKey: "telegram:1"})
	ctxB := tools.WithToolContext(context.Background(), tools.ToolContext{SessionKey: "voice:local"})
	for _, ctx := range []context.Context{ctxA, ctxB} {
		if res := byName["screen_view"].Execute(ctx, map[string]any{}); res.IsError {
			t.Fatal(res.ForLLM)
		}
	}
	if res := byName["screen_zoom"].Execute(ctxA, map[string]any{"cell": "A1"}); res.IsError {
		t.Fatal(res.ForLLM)
	}
	m.calls = nil
	byName["mouse"].Execute(ctxB, map[string]any{"action": "click", "cell": "D4"})
	if !m.ranWith("xdotool mousemove 336 336") {
		t.Errorf("session B clicked on session A's zoom: %v", m.calls)
	}
	// A qualified cell resolves on its own frame's grid even with a zoom up:
	// f1 is A's full view, f3 its zoom.
	m.calls = nil
	byName["mouse"].Execute(ctxA, map[string]any{"action": "click", "cell": "f1:D4"})
	if !m.ranWith("xdotool mousemove 336 336") {
		t.Errorf("f1:D4 did not resolve on the full grid: %v", m.calls)
	}
	if res := byName["mouse"].Execute(ctxA, map[string]any{"action": "click", "cell": "f2:D4"}); !res.IsError || !strings.Contains(res.ForLLM, "not the latest look") {
		t.Errorf("another session's frame was accepted: %s", res.ForLLM)
	}

	store := newVisionStore()
	first := store.forCtx(tools.WithToolContext(context.Background(), tools.ToolContext{SessionKey: "s0"}))
	for i := 1; i <= maxVisionSessions; i++ {
		store.forCtx(tools.WithToolContext(context.Background(), tools.ToolContext{SessionKey: fmt.Sprint("s", i)}))
	}
	if again := store.forCtx(tools.WithToolContext(context.Background(), tools.ToolContext{SessionKey: "s0"})); again == first {
		t.Error("the least recently used session was not evicted")
	}
}

func TestZoomTagsTheTree(t *testing.T) {
	m := shotMachine(t, 640, 480)
	byName := a11yTools(t, m, &fakeAccessor{tree: formTree()})
	viewTree(t, byName)
	res := run(t, byName["screen_zoom"], map[string]any{"x": 280, "y": 180, "width": 160, "height": 80})
	if res.IsError || !strings.Contains(res.ForLLM, "Tagged here from f1's accessibility tree: e6") {
		t.Fatalf("zoom: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "(frame f2)") {
		t.Errorf("the zoom is not numbered: %s", res.ForLLM)
	}
	// e6 (Save) named on the zoom is still the same control.
	if res := run(t, byName["mouse"], map[string]any{"action": "click", "element": "f2:e6"}); res.IsError {
		t.Errorf("a ref named on the zoom: %s", res.ForLLM)
	}
}
