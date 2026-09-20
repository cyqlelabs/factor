//go:build !nobrowser

package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/decision"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// fakePage is a page the executor can drive with no browser: a read, a
// probe that moves when the page does, and actions scripted per test. It is
// what lets every stop path of the loop run in CI, where Chromium is not.
type fakePage struct {
	page     *pageRead
	version  int
	readErr  error
	unstable bool // the probe never answers the same twice
	onClick  func(ref string) (changed bool, err error)
	onFill   func(ref, text string) error
	onScroll func(to string) (grew bool)
	onBack   func() error
	actions  []string
}

func (f *fakePage) touch() { f.version++ }

func (f *fakePage) readPage(context.Context, string, int) (*pageRead, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	copied := *f.page
	return &copied, nil
}

func (f *fakePage) probe(context.Context) (pageProbe, bool) {
	if f.unstable {
		f.version++
	}
	return pageProbe{URL: f.page.URL, Ready: "complete", Nodes: f.version, Text: len(f.page.Text)}, true
}

func (f *fakePage) click(_ context.Context, target string) (*pageRead, bool, error) {
	f.actions = append(f.actions, "click:"+target)
	changed, err := false, error(nil)
	if f.onClick != nil {
		changed, err = f.onClick(target)
	}
	if err != nil {
		return nil, false, err
	}
	if changed {
		f.touch()
	}
	copied := *f.page
	return &copied, changed, nil
}

func (f *fakePage) fill(_ context.Context, target, text string, _ bool) (string, error) {
	f.actions = append(f.actions, "fill:"+target+"="+text)
	if f.onFill != nil {
		if err := f.onFill(target, text); err != nil {
			return "", err
		}
	}
	f.touch()
	return "Filled.", nil
}

func (f *fakePage) scroll(_ context.Context, to, _ string) (string, *pageRead, error) {
	f.actions = append(f.actions, "scroll:"+to)
	grew := ""
	if f.onScroll != nil && f.onScroll(to) {
		grew = "Scrolling loaded more.\n"
		f.touch()
	}
	copied := *f.page
	return grew, &copied, nil
}

func (f *fakePage) back(context.Context) (*pageRead, error) {
	f.actions = append(f.actions, "back")
	if f.onBack != nil {
		if err := f.onBack(); err != nil {
			return nil, err
		}
	}
	f.touch()
	copied := *f.page
	return &copied, nil
}

func (f *fakePage) awaitChange(context.Context, pageProbe, time.Duration) (pageProbe, bool) {
	f.actions = append(f.actions, "wait")
	return pageProbe{}, false
}

// searchForm is the flights page as a read.
func searchForm() *pageRead {
	return &pageRead{Title: "Flight search", URL: "http://x/", Text: "Book a flight", Elements: []pageElement{
		{Ref: "e1", Tag: "input", Type: "text", Label: "Where from?", Value: "Zurich"},
		{Ref: "e2", Tag: "input", Type: "text", Label: "Where to?"},
		{Ref: "e3", Tag: "button", Label: "Search"},
		{Ref: "e4", Tag: "button", Label: "Noop"},
		{Ref: "e5", Tag: "button", Label: "Book now"},
	}}
}

// stepBackend scripts the decision model per request from the request itself.
type stepBackend struct {
	step   int
	decide func(step int, req *decision.Request) (op, target string, conf float64)
	err    error
}

func (b *stepBackend) Decide(_ context.Context, req *decision.Request) (*decision.Response, error) {
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
			if el, _ := entry["element"].(string); strings.Contains(el, target) {
				idx = i
			}
		}
		if idx == "" {
			idx = decision.Candidates(q)[0]
		}
		fill(name, q, idx)
	}
	return &decision.Response{Answers: answers, Model: "jev-fake", Usage: decision.Usage{InputTokens: 10}}, nil
}

func (b *stepBackend) Name() string { return "step" }

func fakeRun(t *testing.T, page *fakePage, b decision.Backend, text TextWriter) *runTool {
	t.Helper()
	d := decision.New(b, decision.Options{Mode: decision.ModeActive})
	return &runTool{s: NewSession(config.BrowserConfig{Engine: "chromium"}, t.TempDir(), nil), drive: page, decider: d, text: text}
}

func runGoal(t *testing.T, tool *runTool, ctx context.Context, args map[string]any) string {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	if _, ok := args["goal"]; !ok {
		args["goal"] = "find flights"
	}
	res := tool.Execute(ctx, args)
	if res.IsError {
		t.Fatalf("run errored: %s", res.ForLLM)
	}
	return res.ForLLM
}

func TestFakeRunTypesSearchesAndFinishes(t *testing.T) {
	page := &fakePage{page: searchForm()}
	page.onFill = func(ref, text string) error {
		page.page.Elements[1].Value = text
		return nil
	}
	page.onClick = func(ref string) (bool, error) {
		if ref == "e3" {
			page.page.Text = "Flights to London: 3 options"
			return true, nil
		}
		return false, nil
	}
	b := &stepBackend{decide: func(_ int, req *decision.Request) (string, string, float64) {
		state := req.State.(map[string]any)
		text := state["page"].(map[string]any)["text"].(string)
		if strings.Contains(text, "Flights to London") {
			return opDone, "", 0.95
		}
		if page.page.Elements[1].Value == "" {
			return opType, "Where to?", 0.9
		}
		return opClick, "Search", 0.9
	}}
	out := runGoal(t, fakeRun(t, page, b, scriptedText{reply: `{"text": "London"}`}), context.Background(), nil)
	for _, want := range []string{"DONE", `1. TYPE_TEXT "Where to?" = "London" → page changed`, `2. CLICK "Search" → page changed`,
		`Withheld (allow_submit is false): "Book now"`, "Flights to London"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Join(page.actions, " ") != "fill:e2=London click:e3" {
		t.Errorf("actions = %v", page.actions)
	}
}

func TestFakeRunStopPaths(t *testing.T) {
	always := func(op, target string, conf float64) *stepBackend {
		return &stepBackend{decide: func(int, *decision.Request) (string, string, float64) { return op, target, conf }}
	}
	cases := map[string]struct {
		page  func() *fakePage
		b     decision.Backend
		text  TextWriter
		args  map[string]any
		ctx   func() context.Context
		want  []string
		steps string
	}{
		"blocked": {page: func() *fakePage { return &fakePage{page: searchForm()} }, b: always(opBlocked, "", 0.9),
			want: []string{"BLOCKED", "Steps: none taken"}},
		"stalled": {page: func() *fakePage { return &fakePage{page: searchForm()} }, b: always(opClick, "Noop", 0.9),
			want: []string{"stopped changing", `3. CLICK "Noop" → nothing changed`}, steps: "click:e4 click:e4 click:e4"},
		"unsure": {page: func() *fakePage { return &fakePage{page: searchForm()} }, b: always(opClick, "Search", 0.2),
			want: []string{"not confident", "Steps: none taken"}},
		"budget": {page: func() *fakePage {
			p := &fakePage{page: searchForm()}
			p.onClick = func(string) (bool, error) { return true, nil }
			return p
		}, b: always(opClick, "Search", 0.9), args: map[string]any{"max_steps": 2},
			want: []string{"2 actions taken", "Steps (2)"}},
		"paused": {page: func() *fakePage { return &fakePage{page: searchForm()} }, b: always(opClick, "Search", 0.9),
			ctx:  func() context.Context { return tools.WithInterrupt(context.Background(), func() bool { return true }) },
			want: []string{"paused", "new message"}},
		"cancelled": {page: func() *fakePage { return &fakePage{page: searchForm()} }, b: always(opClick, "Search", 0.9),
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			want: []string{"paused", "interrupted"}},
		"unavailable": {page: func() *fakePage { return &fakePage{page: searchForm()} }, b: &stepBackend{err: errors.New("refused")},
			want: []string{"did not answer", "Page now:"}},
		"missing value": {page: func() *fakePage { return &fakePage{page: searchForm()} }, b: always(opType, "Where to?", 0.9),
			text: scriptedText{reply: `{"text": null}`}, want: []string{`does not say what to enter in "Where to?"`}},
		"text helper down": {page: func() *fakePage { return &fakePage{page: searchForm()} }, b: always(opType, "Where to?", 0.9),
			text: failingText{}, want: []string{"the text helper failed"}},
		"unstable page": {page: func() *fakePage { return &fakePage{page: searchForm(), unstable: true} }, b: always(opClick, "Search", 0.9),
			want: []string{"kept changing"}},
		"action fails then done": {page: func() *fakePage {
			p := &fakePage{page: searchForm()}
			p.onClick = func(string) (bool, error) { return false, errors.New("no element matches") }
			return p
		}, b: &stepBackend{decide: func(step int, _ *decision.Request) (string, string, float64) {
			if step == 0 {
				return opClick, "Search", 0.9
			}
			return opDone, "", 0.9
		}}, want: []string{"DONE", `1. CLICK "Search" → failed: no element matches`}},
		"wait scroll back": {page: func() *fakePage {
			p := &fakePage{page: searchForm()}
			p.onScroll = func(to string) bool { return to == "down" }
			return p
		}, b: &stepBackend{decide: func(step int, _ *decision.Request) (string, string, float64) {
			switch step {
			case 0:
				return opWait, "", 0.9
			case 1:
				return opScrollDn, "", 0.9
			case 2:
				return opScrollUp, "", 0.9
			case 3:
				return opBack, "", 0.9
			}
			return opDone, "", 0.9
		}}, want: []string{"1. WAIT", "2. SCROLL_DOWN → page changed", "3. SCROLL_UP → nothing changed", "4. BACK → page changed", "DONE"},
			steps: "wait scroll:down scroll:up back"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			page := c.page()
			ctx := context.Background()
			if c.ctx != nil {
				ctx = c.ctx()
			}
			out := runGoal(t, fakeRun(t, page, c.b, c.text), ctx, c.args)
			for _, want := range c.want {
				if !strings.Contains(out, want) {
					t.Errorf("lacks %q:\n%s", want, out)
				}
			}
			if c.steps != "" && strings.Join(page.actions, " ") != c.steps {
				t.Errorf("actions = %v, want %q", page.actions, c.steps)
			}
		})
	}
}

type failingText struct{}

func (failingText) Chat(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, errors.New("light chain down")
}

func TestFakeRunReadFailures(t *testing.T) {
	page := &fakePage{page: searchForm(), readErr: errors.New("tab gone")}
	res := fakeRun(t, page, &stepBackend{}, nil).Execute(context.Background(), map[string]any{"goal": "g"})
	if !res.IsError || !strings.Contains(res.ForLLM, "could not read the page") {
		t.Errorf("first read failure: %+v", res)
	}
	// A read that fails mid-run ends it with the reason and no page.
	page = &fakePage{page: searchForm()}
	page.onClick = func(string) (bool, error) {
		page.readErr = errors.New("tab closed")
		return false, errors.New("click failed")
	}
	b := &stepBackend{decide: func(int, *decision.Request) (string, string, float64) { return opClick, "Search", 0.9 }}
	out := runGoal(t, fakeRun(t, page, b, nil), context.Background(), nil)
	if !strings.Contains(out, "tab closed") || strings.Contains(out, "Page now:") {
		t.Errorf("mid-run read failure:\n%s", out)
	}
	// DONE decided over a page that then moved is re-decided, not accepted.
	moved := &fakePage{page: searchForm()}
	calls := 0
	b = &stepBackend{decide: func(step int, _ *decision.Request) (string, string, float64) {
		calls++
		if step == 0 {
			moved.touch() // the page changes under the decision
		}
		return opDone, "", 0.9
	}}
	out = runGoal(t, fakeRun(t, moved, b, nil), context.Background(), nil)
	if !strings.Contains(out, "DONE") || calls != 2 {
		t.Errorf("stale DONE: calls=%d\n%s", calls, out)
	}
}
