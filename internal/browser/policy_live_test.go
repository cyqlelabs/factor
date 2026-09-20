//go:build !nobrowser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/decision"
	"github.com/cyqlelabs/factor/internal/tools"
)

// flightsPage is a search form of the kind the executor exists for: two
// fields, a search control that renders results in place, a button that
// would book, and a control that does nothing.
const flightsPage = `<html><head><title>Flight search</title></head><body>
<main>
  <input id="from" type="text" placeholder="Where from?" value="Zurich">
  <input id="to" type="text" placeholder="Where to?">
  <button id="search" onclick="search()">Search</button>
  <button id="noop" onclick="void 0">Noop</button>
  <button id="book" onclick="document.getElementById('results').innerText='BOOKED'">Book now</button>
  <div id="results"></div>
</main>
<script>
  function search() {
    const to = document.getElementById('to').value;
    document.getElementById('results').innerText = to ? 'Flights to ' + to + ': 3 options' : 'Enter a destination';
  }
</script>
</body></html>`

// tickerPage redraws itself every 80 ms, so no observation of it holds still.
const tickerPage = `<html><head><title>Ticker</title></head><body><main>
<button id="b">Next</button><div id="t"></div>
<script>setInterval(() => { const d = document.createElement('p'); d.innerText = 'tick'; document.getElementById('t').appendChild(d); }, 80);</script>
</main></body></html>`

func serveHTML(t *testing.T, html string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, html)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wireQuestions is one request as the decision model sees it.
type wireQuestions struct {
	State     map[string]any
	Questions map[string]decision.Question
}

// chooser plays the decision model: handed the questions exactly as the
// executor built them, it answers them. That is what makes these tests end
// to end from the page's DOM to the candidate ids the executor validates.
type chooser func(step int, req wireQuestions) map[string]decision.Answer

// fakeModel stands in for the local model, recording every request.
type fakeModel struct {
	mu       sync.Mutex
	requests []wireQuestions
	choose   chooser
	fail     bool
	// delay is how long an answer takes, for a test whose page must change
	// under the decision.
	delay time.Duration
}

func (f *fakeModel) Name() string { return "fake-laya" }

func (f *fakeModel) Decide(_ context.Context, req *decision.Request) (*decision.Response, error) {
	state, _ := req.State.(map[string]any)
	seen := wireQuestions{State: state, Questions: req.Questions}
	f.mu.Lock()
	f.requests = append(f.requests, seen)
	step := len(f.requests) - 1
	fail, delay, choose := f.fail, f.delay, f.choose
	f.mu.Unlock()
	time.Sleep(delay)
	if fail {
		return nil, fmt.Errorf("%w: the model is not running", decision.ErrUnavailable)
	}
	return &decision.Response{Answers: choose(step, seen), Model: "laya-multilingual",
		Usage: decision.Usage{InputTokens: 100}}, nil
}

func (f *fakeModel) seen() []wireQuestions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wireQuestions(nil), f.requests...)
}

// answerWith answers one question with a valid distribution favouring choice.
func answerWith(q decision.Question, choice string, conf float64) decision.Answer {
	ids := decision.Candidates(q)
	probs := map[string]float64{}
	rest := (1 - 0.9) / float64(max(len(ids)-1, 1))
	for _, id := range ids {
		probs[id] = rest
	}
	probs[choice] = 0.9
	if len(ids) == 1 {
		probs[choice] = 1
	}
	return decision.Answer{Choice: choice, Confidence: conf, Probabilities: probs}
}

// indexOf finds the candidate index whose element label contains want.
func indexOf(q decision.Question, want string) string {
	for idx, raw := range q.Criteria {
		entry, _ := raw.(map[string]any)
		if el, _ := entry["element"].(string); strings.Contains(el, want) {
			return idx
		}
	}
	return ""
}

// choose answers every head: the operation as named, and each target head
// pointing at the element whose label contains target.
func choose(req wireQuestions, op, target string, conf float64) map[string]decision.Answer {
	answers := map[string]decision.Answer{"operation": answerWith(req.Questions["operation"], op, conf)}
	for name, q := range req.Questions {
		if name == "operation" {
			continue
		}
		idx := indexOf(q, target)
		if idx == "" {
			idx = decision.Candidates(q)[0]
		}
		answers[name] = answerWith(q, idx, conf)
	}
	return answers
}

type runFixture struct {
	tool     tools.Tool
	session  *Session
	model    *fakeModel
	outcomes *[]decision.Outcome
}

func newRunFixture(t *testing.T, url string, ch chooser, text TextWriter) *runFixture {
	t.Helper()
	requireBrowser(t)
	model := &fakeModel{choose: ch}
	var outcomes []decision.Outcome
	var mu sync.Mutex
	d := decision.New(model, decision.Options{Mode: decision.ModeActive, Timeout: 5 * time.Second}).
		OnOutcome(func(_ context.Context, o decision.Outcome) {
			mu.Lock()
			outcomes = append(outcomes, o)
			mu.Unlock()
		})
	s := NewSession(liveConfig(), t.TempDir(), nil)
	t.Cleanup(s.Close)
	if res := (&navigateTool{s}).Execute(context.Background(), map[string]any{"url": url}); res.IsError {
		if strings.Contains(res.ForLLM, "browser start") {
			t.Skipf("chrome cannot start here: %s", res.ForLLM)
		}
		t.Fatalf("navigate: %s", res.ForLLM)
	}
	return &runFixture{tool: NewRunTool(s, d, text), session: s, model: model, outcomes: &outcomes}
}

// The whole path: the page is read into candidates, the fake model types a
// destination, activates the search, sees the results and says DONE, and the
// parent gets the steps, the withheld booking control and the page back.
func TestBrowserRunCompletesASearchEndToEnd(t *testing.T) {
	srv := serveHTML(t, flightsPage)
	f := newRunFixture(t, srv.URL, func(_ int, req wireQuestions) map[string]decision.Answer {
		page, _ := req.State["page"].(map[string]any)
		text, _ := page["text"].(string)
		if strings.Contains(text, "Flights to London") {
			return choose(req, opDone, "", 0.95)
		}
		if q, ok := req.Questions["type_text_target"]; ok {
			if idx := indexOf(q, "Where to?"); idx != "" {
				entry, _ := q.Criteria[idx].(map[string]any)
				if entry["current_value"] == nil {
					return choose(req, opType, "Where to?", 0.9)
				}
			}
		}
		return choose(req, opClick, "Search", 0.9)
	}, scriptedText{reply: `{"text": "London"}`})

	res := f.tool.Execute(context.Background(), map[string]any{"goal": "Find flights from Zurich to London; done when results are visible"})
	if res.IsError {
		t.Fatalf("run failed: %s", res.ForLLM)
	}
	for _, want := range []string{
		"DONE", "completion candidate",
		`1. TYPE_TEXT "Where to?" = "London"`,
		`2. CLICK "Search" → page changed`,
		`Withheld (allow_submit is false): "Book now"`,
		"Flights to London: 3 options",
		`"Where to?" = "London"`,
	} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("result lacks %q:\n%s", want, res.ForLLM)
		}
	}
	reqs := f.model.seen()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 decision requests, got %d", len(reqs))
	}
	first := reqs[0]
	if indexOf(first.Questions["click_target"], "Book now") != "" {
		t.Error("the booking control was offered as a click target")
	}
	if first.State["withheld_controls"] == nil {
		t.Error("the state did not name the withheld control")
	}
	if _, ok := first.Questions["operation"].Criteria[opDone]; !ok {
		t.Error("DONE was not offered")
	}
	kinds := map[string]int{}
	for _, o := range *f.outcomes {
		kinds[o.Kind+":"+o.Result]++
	}
	if kinds["operation:acted"] != 3 || kinds["target:acted"] != 2 {
		t.Errorf("recorded outcomes = %v", kinds)
	}
}

func TestBrowserRunOffersCommittingControlsOnlyWhenAllowed(t *testing.T) {
	srv := serveHTML(t, flightsPage)
	f := newRunFixture(t, srv.URL, func(_ int, req wireQuestions) map[string]decision.Answer {
		return choose(req, opBlocked, "", 0.9)
	}, nil)
	res := f.tool.Execute(context.Background(), map[string]any{"goal": "book it", "allow_submit": true})
	if res.IsError || !strings.Contains(res.ForLLM, "BLOCKED") {
		t.Fatalf("result: %+v", res)
	}
	req := f.model.seen()[0]
	if indexOf(req.Questions["click_target"], "Book now") == "" {
		t.Error("allow_submit did not offer the booking control")
	}
	if _, ok := req.Questions["type_text_target"]; ok {
		t.Error("TYPE_TEXT offered with no text writer")
	}
	if strings.Contains(res.ForLLM, "Withheld") {
		t.Error("nothing should be withheld under allow_submit")
	}
}

func TestBrowserRunStopsWhenThePageStopsChanging(t *testing.T) {
	srv := serveHTML(t, flightsPage)
	f := newRunFixture(t, srv.URL, func(_ int, req wireQuestions) map[string]decision.Answer {
		return choose(req, opClick, "Noop", 0.9)
	}, nil)
	res := f.tool.Execute(context.Background(), map[string]any{"goal": "click around"})
	if res.IsError || !strings.Contains(res.ForLLM, "stopped changing") {
		t.Fatalf("result: %+v", res)
	}
	if n := strings.Count(res.ForLLM, `CLICK "Noop" → nothing changed`); n != runStallLimit {
		t.Errorf("%d no-op steps recorded, want %d:\n%s", n, runStallLimit, res.ForLLM)
	}
}

func TestBrowserRunHandsBackWhenUnsure(t *testing.T) {
	srv := serveHTML(t, flightsPage)
	f := newRunFixture(t, srv.URL, func(_ int, req wireQuestions) map[string]decision.Answer {
		return choose(req, opClick, "Search", 0.2) // well under the bar
	}, nil)
	res := f.tool.Execute(context.Background(), map[string]any{"goal": "g"})
	if res.IsError || !strings.Contains(res.ForLLM, "not confident") || !strings.Contains(res.ForLLM, "Steps: none taken") {
		t.Fatalf("result: %+v", res)
	}
	if len(f.model.seen()) != runUnsureLimit {
		t.Errorf("%d requests, want %d", len(f.model.seen()), runUnsureLimit)
	}
	unsure := 0
	for _, o := range *f.outcomes {
		if o.Result == "unsure" {
			unsure++
		}
	}
	if unsure == 0 {
		t.Error("no unsure outcome recorded")
	}
}

func TestBrowserRunPausesForSteering(t *testing.T) {
	srv := serveHTML(t, flightsPage)
	f := newRunFixture(t, srv.URL, func(_ int, req wireQuestions) map[string]decision.Answer {
		return choose(req, opClick, "Search", 0.9)
	}, nil)
	ctx := tools.WithInterrupt(context.Background(), func() bool { return true })
	res := f.tool.Execute(ctx, map[string]any{"goal": "g"})
	if res.IsError || !strings.Contains(res.ForLLM, "paused") || !strings.Contains(res.ForLLM, "new message") {
		t.Fatalf("result: %+v", res)
	}
	if len(f.model.seen()) != 0 {
		t.Error("a paused run still asked for a decision")
	}
	// Steering that arrives after the first action stops before the second.
	calls := 0
	ctx = tools.WithInterrupt(context.Background(), func() bool { calls++; return calls > 1 })
	res = f.tool.Execute(ctx, map[string]any{"goal": "g"})
	if !strings.Contains(res.ForLLM, "paused") || !strings.Contains(res.ForLLM, "Steps (1)") {
		t.Errorf("result after one step: %s", res.ForLLM)
	}
}

func TestBrowserRunYieldsWhenTheBackendIsDown(t *testing.T) {
	srv := serveHTML(t, flightsPage)
	f := newRunFixture(t, srv.URL, nil, nil)
	f.model.fail = true
	res := f.tool.Execute(context.Background(), map[string]any{"goal": "g"})
	if res.IsError || !strings.Contains(res.ForLLM, "did not answer") || !strings.Contains(res.ForLLM, "Page now:") {
		t.Fatalf("result: %+v", res)
	}
	fallbacks := 0
	for _, o := range *f.outcomes {
		if o.Result == "fallback" {
			fallbacks++
		}
	}
	if fallbacks != 1 {
		t.Errorf("fallback outcomes = %d", fallbacks)
	}
}

func TestBrowserRunStopsWhenTheGoalDoesNotSupplyAValue(t *testing.T) {
	srv := serveHTML(t, flightsPage)
	f := newRunFixture(t, srv.URL, func(_ int, req wireQuestions) map[string]decision.Answer {
		return choose(req, opType, "Where to?", 0.9)
	}, scriptedText{reply: `{"text": null}`})
	res := f.tool.Execute(context.Background(), map[string]any{"goal": "search"})
	if res.IsError || !strings.Contains(res.ForLLM, `does not say what to enter in "Where to?"`) {
		t.Fatalf("result: %+v", res)
	}
	if strings.Contains(res.ForLLM, "TYPE_TEXT") {
		t.Error("nothing should have been typed")
	}
}

func TestBrowserRunRespectsTheStepBudget(t *testing.T) {
	srv := serveHTML(t, flightsPage)
	f := newRunFixture(t, srv.URL, func(_ int, req wireQuestions) map[string]decision.Answer {
		return choose(req, opClick, "Search", 0.9)
	}, nil)
	res := f.tool.Execute(context.Background(), map[string]any{"goal": "g", "max_steps": 1})
	if res.IsError || !strings.Contains(res.ForLLM, "1 actions taken") || !strings.Contains(res.ForLLM, "Steps (1)") {
		t.Fatalf("result: %+v", res)
	}
}

func TestBrowserRunWaitsScrollsAndGoesBack(t *testing.T) {
	first := serveHTML(t, flightsPage)
	f := newRunFixture(t, first.URL, func(step int, req wireQuestions) map[string]decision.Answer {
		switch step {
		case 0:
			return choose(req, opWait, "", 0.9)
		case 1:
			return choose(req, opScrollDn, "", 0.9)
		case 2:
			return choose(req, opScrollUp, "", 0.9)
		}
		return choose(req, opDone, "", 0.9)
	}, nil)
	res := f.tool.Execute(context.Background(), map[string]any{"goal": "g"})
	if res.IsError {
		t.Fatalf("result: %+v", res)
	}
	for _, want := range []string{"1. WAIT", "2. SCROLL_DOWN", "3. SCROLL_UP", "DONE"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("result lacks %q:\n%s", want, res.ForLLM)
		}
	}
	// A WAIT does not count toward a stall, and three of them do not end the run.
	if strings.Contains(res.ForLLM, "stopped changing") {
		t.Error("waits and scrolls on a static page were read as a stall")
	}

	// BACK from a second page lands on the first and the run continues.
	second := serveHTML(t, `<html><head><title>Second</title></head><body><main>second page</main></body></html>`)
	if res := (&navigateTool{f.session}).Execute(context.Background(), map[string]any{"url": second.URL}); res.IsError {
		t.Fatal(res.ForLLM)
	}
	f.model.mu.Lock()
	f.model.requests = nil
	f.model.choose = func(step int, req wireQuestions) map[string]decision.Answer {
		if step == 0 {
			return choose(req, opBack, "", 0.9)
		}
		return choose(req, opDone, "", 0.9)
	}
	f.model.mu.Unlock()
	res = f.tool.Execute(context.Background(), map[string]any{"goal": "g"})
	if res.IsError || !strings.Contains(res.ForLLM, "1. BACK → page changed") || !strings.Contains(res.ForLLM, "Flight search") {
		t.Fatalf("back result: %s", res.ForLLM)
	}
}

func TestBrowserRunGivesUpOnAPageThatWillNotHoldStill(t *testing.T) {
	srv := serveHTML(t, tickerPage)
	f := newRunFixture(t, srv.URL, func(_ int, req wireQuestions) map[string]decision.Answer {
		return choose(req, opClick, "Next", 0.9)
	}, nil)
	// A decision slower than the page's redraw, so the gate always finds
	// the page moved on from the observation it was decided over.
	f.model.delay = 250 * time.Millisecond
	res := f.tool.Execute(context.Background(), map[string]any{"goal": "g"})
	if res.IsError || !strings.Contains(res.ForLLM, "kept changing") {
		t.Fatalf("result: %+v", res)
	}
	if len(f.model.seen()) > runStaleLimit {
		t.Errorf("%d decisions spent on a page that never held still", len(f.model.seen()))
	}
}
