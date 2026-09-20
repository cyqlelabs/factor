//go:build !nobrowser

package browser

import (
	"context"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/decision"
	"github.com/cyqlelabs/factor/internal/provider"
)

func formPage() *pageRead {
	return &pageRead{Title: "Form", URL: "http://x/", Text: "Book a flight", Elements: []pageElement{
		{Ref: "e1", Tag: "input", Type: "text", Label: "Where from?", Value: "Zurich", Selector: "#from"},
		{Ref: "e2", Tag: "input", Type: "text", Label: "Where to?", Selector: "#to"},
		{Ref: "e3", Tag: "input", Type: "password", Label: "Password", Selector: "#pw"},
		{Ref: "e4", Tag: "button", Label: "Search", Selector: "#search"},
		{Ref: "e5", Tag: "button", Label: "Book now", Selector: "#book"},
		{Ref: "e6", Tag: "input", Type: "submit", Label: "Go", Selector: "#go"},
		{Ref: "e7", Tag: "select", Label: "Class", Value: "Economy", Selector: "#class"},
		{Ref: "e8", Tag: "input", Type: "checkbox", Label: "Non-stop", Selector: "#ns"},
		{Ref: "e9", Tag: "a", Label: "Results", Href: "/results", Selector: "#r"},
		{Ref: "e10", Tag: "input", Type: "file", Label: "Upload", Selector: "#up"},
	}}
}

func labels(group map[string]*candidate) []string {
	var out []string
	for _, c := range group {
		out = append(out, c.Label)
	}
	return out
}

func has(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// The candidate set is the whole of what the decision may choose from, so
// what is left out of it is the policy: passwords never, committing controls
// only when allowed, fields only when something can write their text.
func TestActionSpaceAppliesThePolicy(t *testing.T) {
	space := buildActionSpace(formPage(), false, true)
	clicks := labels(space.targets[opClick])
	types := labels(space.targets[opType])
	for _, want := range []string{"Search", "Non-stop", "Results"} {
		if !has(clicks, want) {
			t.Errorf("CLICK targets miss %q: %v", want, clicks)
		}
	}
	for _, withheld := range []string{"Book now", "Go"} {
		if has(clicks, withheld) {
			t.Errorf("a committing control %q was offered: %v", withheld, clicks)
		}
		if !has(space.withheld, withheld) {
			t.Errorf("withheld list does not name %q: %v", withheld, space.withheld)
		}
	}
	for _, want := range []string{"Where from?", "Where to?", "Class"} {
		if !has(types, want) {
			t.Errorf("TYPE_TEXT targets miss %q: %v", want, types)
		}
	}
	for _, never := range []string{"Password", "Upload"} {
		if has(types, never) || has(clicks, never) {
			t.Errorf("%q must never be a candidate", never)
		}
	}
	// Indices are dense and each points back at a ref the session knows.
	for i, c := range space.elements {
		if c.Index != itoa(i+1) || c.ref == "" {
			t.Errorf("element %d = %+v", i, c)
		}
	}
	if c := space.targets[opType]["1"]; c == nil || c.Value != "Zurich" {
		t.Errorf("the field's current value did not ride the candidate: %+v", c)
	}

	allowed := buildActionSpace(formPage(), true, true)
	if !has(labels(allowed.targets[opClick]), "Book now") || len(allowed.withheld) != 0 {
		t.Errorf("allow_submit did not offer the committing controls: %v", labels(allowed.targets[opClick]))
	}
	noText := buildActionSpace(formPage(), false, false)
	if _, offered := noText.targets[opType]; offered {
		t.Error("TYPE_TEXT offered with nothing to write the text")
	}
}

func itoa(i int) string {
	return strings.TrimSpace(strings.Repeat(" ", 0) + string(rune('0'+i)))
}

func TestRequestOffersOnlySupportedOperationsAndATargetHeadPerOperation(t *testing.T) {
	r := &browserRun{goal: "search flights", allowSubmit: false}
	o := &observation{page: formPage(), space: buildActionSpace(formPage(), false, true)}
	req := r.request(o)
	op := req.Questions["operation"]
	for _, want := range []string{opClick, opType, opScrollDn, opScrollUp, opBack, opWait, opDone, opBlocked} {
		if _, ok := op.Criteria[want]; !ok {
			t.Errorf("operation head misses %s", want)
		}
	}
	if _, ok := req.Questions["click_target"]; !ok {
		t.Error("no click_target head")
	}
	if _, ok := req.Questions["type_text_target"]; !ok {
		t.Error("no type_text_target head")
	}
	state := req.State.(map[string]any)
	if state["withheld_controls"] == nil {
		t.Error("the state does not say which controls were withheld")
	}
	if state["goal"] != "search flights" {
		t.Errorf("state goal = %v", state["goal"])
	}
	// A page with nothing to type into offers no TYPE_TEXT head or operation.
	bare := &pageRead{Elements: []pageElement{{Ref: "e1", Tag: "a", Label: "Home"}}}
	req = r.request(&observation{page: bare, space: buildActionSpace(bare, false, true)})
	if _, ok := req.Questions["operation"].Criteria[opType]; ok {
		t.Error("TYPE_TEXT offered on a page with no fields")
	}
	if _, ok := req.Questions["type_text_target"]; ok {
		t.Error("a type_text_target head for a page with no fields")
	}
	// Every head's candidates are exactly what validation will check.
	for name, q := range req.Questions {
		if len(decision.Candidates(q)) == 0 {
			t.Errorf("%s has no candidates", name)
		}
	}
}

func TestParseFieldTextIsStrict(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		ok    bool
		fails bool
	}{
		{in: `{"text": "London"}`, want: "London", ok: true},
		{in: "```json\n{\"text\": \"Zurich\"}\n```", want: "Zurich", ok: true},
		{in: `{"text": null}`, ok: false},
		{in: `{"text": "   "}`, ok: false},
		{in: `{"text": "x", "note": "y"}`, fails: true},
		{in: `{"value": "x"}`, fails: true},
		{in: `London`, fails: true},
		{in: `{"text": "` + strings.Repeat("x", 2001) + `"}`, fails: true},
	}
	for _, c := range cases {
		got, ok, err := parseFieldText(c.in)
		if c.fails {
			if err == nil {
				t.Errorf("%q: expected an error, got %q ok=%v", c.in, got, ok)
			}
			continue
		}
		if err != nil || ok != c.ok || got != c.want {
			t.Errorf("%q: got %q ok=%v err=%v", c.in, got, ok, err)
		}
	}
}

func TestOperationForAndCommits(t *testing.T) {
	if op := operationFor(pageElement{Tag: "input", Type: "email"}); op != opType {
		t.Errorf("email field = %s", op)
	}
	if op := operationFor(pageElement{Tag: "input", Type: "radio"}); op != opClick {
		t.Errorf("radio = %s", op)
	}
	if op := operationFor(pageElement{Tag: "div"}); op != opClick {
		t.Errorf("div (role=button) = %s", op)
	}
	if op := operationFor(pageElement{Tag: "input", Type: "hidden"}); op != "" {
		t.Errorf("hidden = %q", op)
	}
	for _, label := range []string{"Buy", "Book now", "Proceed to checkout", "Sign up", "Send message", "Place order"} {
		if !commits(pageElement{Tag: "button", Label: label}) {
			t.Errorf("%q should commit", label)
		}
	}
	for _, label := range []string{"Search", "Next", "Round trip", "Bookmarks"} {
		if commits(pageElement{Tag: "button", Label: label}) {
			t.Errorf("%q should not commit", label)
		}
	}
}

func TestSummaryNamesEveryStopAndTheSteps(t *testing.T) {
	yes, no := true, false
	r := &browserRun{goal: "g", history: []stepRecord{
		{Step: 1, Operation: opType, Target: "Where to?", Text: "London", PageChanged: &yes},
		{Step: 2, Operation: opClick, Target: "Search", PageChanged: &no},
		{Step: 3, Operation: opClick, Target: "Gone", Note: "no element matches"},
	}}
	o := &observation{page: &pageRead{Title: "T", URL: "http://x", Text: strings.Repeat("t", runSummaryChars+50)},
		space: actionSpace{withheld: []string{"Book now"}}}
	for how, want := range map[string]string{
		"done": "completion candidate", "blocked": "BLOCKED", "stalled": "stopped changing", "unsure": "not confident",
		"budget": "narrower goal", "paused": "paused", "unavailable": "did not answer", "missing_value": "browser_fill",
		"error": "stopped:",
	} {
		out := r.summary(o, stop{how: how, note: "n"}).ForLLM
		if !strings.Contains(out, want) {
			t.Errorf("%s: summary lacks %q:\n%s", how, want, out)
		}
		for _, line := range []string{`TYPE_TEXT "Where to?" = "London" → page changed`, `CLICK "Search" → nothing changed`,
			`CLICK "Gone" → failed: no element matches`, `Withheld (allow_submit is false): "Book now"`, "Page now:"} {
			if !strings.Contains(out, line) {
				t.Errorf("%s: summary lacks %q:\n%s", how, line, out)
			}
		}
		if strings.Contains(out, strings.Repeat("t", runSummaryChars+1)) {
			t.Errorf("%s: page text not bounded", how)
		}
	}
	empty := (&browserRun{goal: "g"}).summary(nil, stop{how: "unavailable", note: "x"}).ForLLM
	if !strings.Contains(empty, "Steps: none taken.") {
		t.Errorf("empty run summary:\n%s", empty)
	}
}

type nilBackend struct{}

func (nilBackend) Decide(context.Context, *decision.Request) (*decision.Response, error) {
	return nil, decision.ErrUnavailable
}
func (nilBackend) Name() string { return "nil" }

func TestRunToolRefusesWithoutAnActiveDeciderOrAGoal(t *testing.T) {
	s := NewSession(config.BrowserConfig{Engine: "chromium"}, t.TempDir(), nil)
	tool := NewRunTool(s, nil, nil)
	if tool.Name() != "browser_run" || tool.Description() == "" || tool.Parameters()["required"] == nil {
		t.Fatal("tool identity")
	}
	res := tool.Execute(context.Background(), map[string]any{"goal": "x"})
	if !res.IsError || !strings.Contains(res.ForLLM, "decision.mode") {
		t.Errorf("no decider: %+v", res)
	}
	shadow := decision.New(nilBackend{}, decision.Options{Mode: decision.ModeShadow})
	res = NewRunTool(s, shadow, nil).Execute(context.Background(), map[string]any{"goal": "x"})
	if !res.IsError || !strings.Contains(res.ForLLM, "not active") {
		t.Errorf("shadow decider: %+v", res)
	}
	active := decision.New(nilBackend{}, decision.Options{Mode: decision.ModeActive})
	res = NewRunTool(s, active, nil).Execute(context.Background(), map[string]any{"goal": "   "})
	if !res.IsError || !strings.Contains(res.ForLLM, "needs a goal") {
		t.Errorf("blank goal: %+v", res)
	}
}

type scriptedText struct{ reply string }

func (s scriptedText) Chat(context.Context, *provider.Request) (*provider.Response, error) {
	return &provider.Response{Content: s.reply}, nil
}

func TestFieldTextNeedsAWriter(t *testing.T) {
	r := &browserRun{tool: &runTool{}}
	if _, _, err := r.fieldText(context.Background(), &candidate{Label: "x"}, &observation{page: &pageRead{}}); err == nil {
		t.Error("no writer should be an error")
	}
	r.tool.text = scriptedText{reply: `{"text": "Paris"}`}
	got, ok, err := r.fieldText(context.Background(), &candidate{Label: "x"}, &observation{page: &pageRead{}})
	if err != nil || !ok || got != "Paris" {
		t.Errorf("got %q ok=%v err=%v", got, ok, err)
	}
}
