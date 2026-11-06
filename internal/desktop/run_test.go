package desktop

import (
	"context"
	"errors"
	"image"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/decision"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// fakeWindow is a window desktop_run can drive with no desktop: a list of
// controls that actions change the way the application would.
type fakeWindow struct {
	els     []Element
	readErr error
	// onDo answers an action; nil applies the default effect.
	onDo  func(el Element, act Act) (Outcome, error)
	acted []string
}

func (w *fakeWindow) Name() string { return "fake" }

func (w *fakeWindow) Read(context.Context, *Window) (*Tree, error) {
	if w.readErr != nil {
		return nil, w.readErr
	}
	return &Tree{Backend: "fake", App: "settings", Window: "Account", Positioned: true, Complete: true,
		Elements: append([]Element(nil), w.els...)}, nil
}

func (w *fakeWindow) Refresh(_ context.Context, el Element) (Element, error) {
	for _, e := range w.els {
		if e.Handle == el.Handle {
			return e, nil
		}
	}
	return Element{}, ErrStale
}

func (w *fakeWindow) Do(_ context.Context, el Element, act Act) (Outcome, error) {
	w.acted = append(w.acted, act.Kind+":"+el.Name+":"+act.Text)
	if w.onDo != nil {
		return w.onDo(el, act)
	}
	for i := range w.els {
		e := &w.els[i]
		if e.Handle != el.Handle {
			continue
		}
		switch act.Kind {
		case ActSetText:
			e.Value = act.Text
		case ActPress:
			switch e.Checked {
			case TriOn:
				e.Checked = TriOff
			case TriOff:
				e.Checked = TriOn
			}
		case ActExpand:
			e.Expanded = TriOn
		case ActCollapse:
			e.Expanded = TriOff
		}
		return Outcome{After: *e}, nil
	}
	return Outcome{}, ErrStale
}

func accountWindow() *fakeWindow {
	at := func(y int) image.Rectangle { return image.Rect(10, y, 200, y+20) }
	return &fakeWindow{els: []Element{
		{Role: RoleTextField, Name: "Name", Value: "", Bounds: at(10), Actions: []string{ActSetText}, Handle: "name"},
		{Role: RolePassword, Name: "Password", Secret: true, Bounds: at(40), Actions: []string{ActSetText}, Handle: "pw"},
		{Role: RoleCheckbox, Name: "Remember me", Checked: TriOff, Bounds: at(70), Actions: []string{ActPress}, Handle: "remember"},
		{Role: RoleTreeItem, Name: "Advanced", Expanded: TriOff, Bounds: at(100), Actions: []string{ActExpand, ActCollapse}, Handle: "adv"},
		{Role: RoleButton, Name: "Apply", Bounds: at(130), Actions: []string{ActPress}, Handle: "apply"},
		{Role: RoleButton, Name: "Delete account", Bounds: at(160), Actions: []string{ActPress}, Handle: "delete"},
		{Role: RoleButton, Name: "Help", Disabled: true, Bounds: at(190), Actions: []string{ActPress}, Handle: "help"},
	}}
}

// script answers the decision model from the request itself.
type script struct {
	step   int
	decide func(step int, req *decision.Request) (op, target string, conf float64)
	err    error
	asked  []*decision.Request
}

func (b *script) Name() string { return "script" }

func (b *script) Decide(_ context.Context, req *decision.Request) (*decision.Response, error) {
	b.asked = append(b.asked, req)
	if b.err != nil {
		return nil, b.err
	}
	op, target, conf := b.decide(b.step, req)
	b.step++
	answers := map[string]decision.Answer{}
	fill := func(name string, q decision.Question, choice string) {
		ids := decision.Candidates(q)
		probs := map[string]float64{}
		for _, id := range ids {
			probs[id] = 0.1 / float64(max(len(ids)-1, 1))
		}
		probs[choice] = 0.9
		if len(ids) == 1 {
			probs[choice] = 1
		}
		answers[name] = decision.Answer{Choice: choice, Confidence: conf, Probabilities: probs}
	}
	fill("operation", req.Questions["operation"], op)
	for name, q := range req.Questions {
		if name == "operation" {
			continue
		}
		idx := ""
		for i, raw := range q.Criteria {
			entry, _ := raw.(map[string]any)
			if c, _ := entry["control"].(string); target != "" && strings.HasSuffix(c, "] "+target) {
				idx = i
			}
		}
		if idx == "" {
			idx = decision.Candidates(q)[0]
		}
		fill(name, q, idx)
	}
	return &decision.Response{Answers: answers, Model: "script"}, nil
}

// steps scripts a run as a fixed sequence of decisions.
func steps(seq ...[2]string) *script {
	return &script{decide: func(step int, _ *decision.Request) (string, string, float64) {
		if step >= len(seq) {
			return opDone, "", 0.95
		}
		return seq[step][0], seq[step][1], 0.95
	}}
}

type fakeText struct {
	reply string
	err   error
	asked int
}

func (f *fakeText) Chat(context.Context, *provider.Request) (*provider.Response, error) {
	f.asked++
	if f.err != nil {
		return nil, f.err
	}
	return &provider.Response{Content: f.reply}, nil
}

func runTools(t *testing.T, w Accessor, b decision.Backend, text TextWriter) map[string]tools.Tool {
	t.Helper()
	m := newMachine("linux", "xdotool")
	var opts []Option
	if b != nil {
		var tw TextWriter
		if text != nil {
			tw = text
		}
		opts = append(opts, WithDecisions(decision.New(b, decision.Options{Mode: decision.ModeActive}), tw))
	}
	byName := map[string]tools.Tool{}
	for _, tool := range newToolsWith(m.Env(), NewController(m.Env()), w, tools.NewPathGuard(t.TempDir(), true, false, nil), t.TempDir(), opts...) {
		byName[tool.Name()] = tool
	}
	return byName
}

func runDesktop(t *testing.T, byName map[string]tools.Tool, ctx context.Context, args map[string]any) *tools.Result {
	t.Helper()
	tool, ok := byName["desktop_run"]
	if !ok {
		t.Fatal("desktop_run is not mounted")
	}
	if _, ok := args["goal"]; !ok {
		args["goal"] = "set my name to Ada, tick remember me, then apply"
	}
	if err := tools.ValidateArgs(tool.Parameters(), args); err != nil {
		t.Fatal(err)
	}
	return tool.Execute(ctx, args)
}

func TestDesktopRunFillsChecksAndFinishes(t *testing.T) {
	w := accountWindow()
	b := steps([2]string{opType, "Name"}, [2]string{opCheck, "Remember me"}, [2]string{opExpand, "Advanced"}, [2]string{opClick, "Apply"})
	text := &fakeText{reply: `{"text": "Ada"}`}
	byName := runTools(t, w, b, text)
	res := runDesktop(t, byName, context.Background(), map[string]any{})
	if res.IsError {
		t.Fatalf("run: %s", res.ForLLM)
	}
	for _, want := range []string{
		"DONE", "completion candidate", "Steps (4)",
		`1. TYPE_TEXT textfield "Name" = "Ada" → window changed`,
		`2. CHECK checkbox "Remember me" → window changed`,
		`3. EXPAND treeitem "Advanced" → window changed`,
		`4. CLICK button "Apply" → nothing changed`,
		`Withheld (allow_submit is false): button "Delete account"`,
		`textfield "Name" = "Ada"`, `checkbox "Remember me" [checked]`,
		"frame f1",
	} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("summary lacks %q:\n%s", want, res.ForLLM)
		}
	}
	// The secret field was never offered, and neither was the commit.
	for _, req := range b.asked {
		for name, q := range req.Questions {
			for _, raw := range q.Criteria {
				entry, _ := raw.(map[string]any)
				if c, _ := entry["control"].(string); strings.Contains(c, "Password") || strings.Contains(c, "Delete") || strings.Contains(c, "Help") {
					t.Errorf("%s offered %s", name, c)
				}
			}
		}
	}
	if text.asked != 1 {
		t.Errorf("the text helper was asked %d times", text.asked)
	}
	// The refs it ended on work with the step tools straight away.
	res = run(t, byName["mouse"], map[string]any{"action": "click", "element": "e5"})
	if res.IsError || !strings.Contains(res.ForLLM, `button "Apply"`) {
		t.Errorf("a ref from the run: %s", res.ForLLM)
	}
}

func TestDesktopRunOffersCommitsOnlyWhenAllowed(t *testing.T) {
	w := accountWindow()
	b := steps([2]string{opClick, "Delete account"})
	byName := runTools(t, w, b, nil)
	res := runDesktop(t, byName, context.Background(), map[string]any{"goal": "delete my account", "allow_submit": true})
	if !strings.Contains(res.ForLLM, `CLICK button "Delete account"`) {
		t.Errorf("an allowed commit was not taken:\n%s", res.ForLLM)
	}
	// Without a text helper nothing is offered for typing.
	if _, ok := b.asked[0].Questions[targetQuestion(opType)]; ok {
		t.Error("typing offered with no text helper")
	}
}

func TestDesktopRunStops(t *testing.T) {
	cases := map[string]struct {
		window func() *fakeWindow
		b      *script
		text   *fakeText
		args   map[string]any
		ctx    func() context.Context
		want   string
	}{
		"missing value": {accountWindow, steps([2]string{opType, "Name"}), &fakeText{reply: `{"text": null}`}, nil, nil,
			`the goal does not say what to put in "Name"`},
		"text helper down": {accountWindow, steps([2]string{opType, "Name"}), &fakeText{err: errors.New("offline")}, nil, nil,
			"the text helper failed: offline"},
		"blocked": {accountWindow, steps([2]string{opBlocked, ""}), nil, nil, nil, "BLOCKED"},
		"unsure": {accountWindow, &script{decide: func(int, *decision.Request) (string, string, float64) { return opClick, "Apply", 0.2 }}, nil, nil, nil,
			"not confident enough"},
		"stalled": {accountWindow, &script{decide: func(int, *decision.Request) (string, string, float64) { return opClick, "Apply", 0.95 }}, nil, nil, nil,
			"3 actions in a row changed nothing"},
		"budget": {accountWindow, &script{decide: func(int, *decision.Request) (string, string, float64) { return opWait, "", 0.95 }}, nil,
			map[string]any{"max_steps": 2}, nil, "2 actions taken without reaching the goal"},
		"unavailable": {accountWindow, &script{err: errors.New("model down")}, nil, nil, nil, "did not answer (decision backend unavailable: model down)"},
		"interrupted": {accountWindow, steps(), nil, nil, func() context.Context {
			return tools.WithInterrupt(context.Background(), func() bool { return true })
		}, "the user sent a new message"},
		"cancelled": {accountWindow, steps(), nil, nil, func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, "the turn was interrupted"},
		"nothing": {func() *fakeWindow { return &fakeWindow{} }, steps(), nil, nil, nil, "did nothing"},
		"vanishing": {func() *fakeWindow {
			w := accountWindow()
			w.onDo = func(Element, Act) (Outcome, error) { return Outcome{}, ErrStale }
			return w
		}, &script{decide: func(int, *decision.Request) (string, string, float64) { return opClick, "Apply", 0.95 }}, nil, nil, nil,
			"kept changing under every decision"},
		"unknown delivery": {func() *fakeWindow {
			w := accountWindow()
			w.onDo = func(Element, Act) (Outcome, error) { return Outcome{}, errors.New("timeout") }
			return w
		}, steps([2]string{opClick, "Apply"}), nil, nil, nil, "run screen_view to see what the last action did"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var text TextWriter
			if c.text != nil {
				text = c.text
			}
			byName := runTools(t, c.window(), c.b, text)
			ctx := context.Background()
			if c.ctx != nil {
				ctx = c.ctx()
			}
			args := c.args
			if args == nil {
				args = map[string]any{}
			}
			res := runDesktop(t, byName, ctx, args)
			if !strings.Contains(res.ForLLM, c.want) {
				t.Errorf("want %q in:\n%s", c.want, res.ForLLM)
			}
		})
	}
}

func TestDesktopRunRefusesWhatItCannotDo(t *testing.T) {
	w := accountWindow()
	w.readErr = errors.New("no window in front publishes an accessibility tree")
	byName := runTools(t, w, steps(), nil)
	if res := runDesktop(t, byName, context.Background(), map[string]any{}); !res.IsError || !strings.Contains(res.ForLLM, "could not read the window") {
		t.Errorf("read failure: %s", res.ForLLM)
	}
	if res := runDesktop(t, byName, context.Background(), map[string]any{"goal": "  "}); !res.IsError {
		t.Errorf("an empty goal ran: %s", res.ForLLM)
	}
	if res := runDesktop(t, byName, context.Background(), map[string]any{"window": "nowhere"}); !res.IsError {
		t.Errorf("an unknown window ran: %s", res.ForLLM)
	}
	// Mounted only with a tree to read and a decider that may act.
	if _, ok := runTools(t, accountWindow(), nil, nil)["desktop_run"]; ok {
		t.Error("mounted without a decider")
	}
	if _, ok := runTools(t, nil, steps(), nil)["desktop_run"]; ok {
		t.Error("mounted without an accessibility backend")
	}
	shadow := map[string]tools.Tool{}
	m := newMachine("linux")
	for _, tool := range newToolsWith(m.Env(), NewController(m.Env()), accountWindow(), nil, t.TempDir(),
		WithDecisions(decision.New(steps(), decision.Options{Mode: decision.ModeShadow}), nil)) {
		shadow[tool.Name()] = tool
	}
	if _, ok := shadow["desktop_run"]; ok {
		t.Error("mounted on a shadow decider")
	}
	rt := &runTool{&deps{}}
	if res := rt.Execute(context.Background(), map[string]any{"goal": "x"}); !res.IsError {
		t.Error("ran with no decider")
	}
}

func TestDesktopRunCollapsesAndWaits(t *testing.T) {
	w := accountWindow()
	w.els[3].Expanded = TriOn
	w.els[2].Checked = TriOn
	b := steps([2]string{opCollapse, "Advanced"}, [2]string{opUncheck, "Remember me"}, [2]string{opWait, ""})
	res := runDesktop(t, runTools(t, w, b, nil), context.Background(), map[string]any{})
	for _, want := range []string{`COLLAPSE treeitem "Advanced"`, `UNCHECK checkbox "Remember me"`, "3. WAIT"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("summary lacks %q:\n%s", want, res.ForLLM)
		}
	}
}

func TestParseFieldText(t *testing.T) {
	for reply, want := range map[string]string{
		`{"text": "Ada"}`:                  "Ada",
		"```json\n{\"text\": \"Bo\"}\n```": "Bo",
	} {
		if got, ok, err := parseFieldText(reply); err != nil || !ok || got != want {
			t.Errorf("%q → %q %t %v", reply, got, ok, err)
		}
	}
	for _, reply := range []string{`{"text": null}`, `{"text": "  "}`} {
		if _, ok, err := parseFieldText(reply); ok || err != nil {
			t.Errorf("%q should be no value", reply)
		}
	}
	for _, reply := range []string{"Ada", `{"text": "a", "why": "b"}`, `{"value": "a"}`, `{"text": "` + strings.Repeat("x", 2001) + `"}`} {
		if _, _, err := parseFieldText(reply); err == nil {
			t.Errorf("%q accepted", reply)
		}
	}
}
