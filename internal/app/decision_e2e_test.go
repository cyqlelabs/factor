//go:build !nobrowser

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/browser"
	"github.com/cyqlelabs/factor/internal/decision"
	"github.com/cyqlelabs/factor/internal/trace"
)

// The whole stack, end to end: a user asks for a flight search, the model
// (fake, over the OpenAI wire) opens the page and delegates to browser_run,
// the executor drives a real Chromium against a real page with decisions from
// a fake TypeSafe endpoint and field text from the light chain, hands the
// evidence back, the model answers, and the completion check passes the
// answer. Nothing in the path is mocked below the wire: the loop, the
// registry, the browser session, the decision client, the meter and the
// trace are the ones a running Factor uses.

const e2eFlightsPage = `<html><head><title>Flight search</title></head><body>
<main>
  <input id="from" type="text" placeholder="Where from?" value="Zurich">
  <input id="to" type="text" placeholder="Where to?">
  <button id="search" onclick="search()">Search</button>
  <button id="book" onclick="void 0">Book now</button>
  <div id="results"></div>
</main>
<script>
  function search() {
    const to = document.getElementById('to').value;
    document.getElementById('results').innerText = to ? 'Flights to ' + to + ': 3 options' : 'Enter a destination';
  }
</script>
</body></html>`

// e2eModel speaks the OpenAI chat-completions dialect for three callers at
// once: the conversation's model, scripted step by step; the light chain
// writing a field value; and the filler, should the turn run long enough.
type e2eModel struct {
	t    *testing.T
	page string
	mu   sync.Mutex
	main int
	// runResult is the browser_run result the model was handed back.
	runResult string
	textCalls int
}

func (m *e2eModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []wireMessage `json:"messages"`
		Tools    []any         `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	text := func(i int) string {
		s, _ := req.Messages[i].Content.(string)
		return s
	}
	say := func(content string) {
		writeCompletion(w, map[string]any{"role": "assistant", "content": content}, "stop")
	}
	call := func(name string, args map[string]any) {
		encoded, _ := json.Marshal(args)
		writeCompletion(w, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": fmt.Sprintf("call_%d", time.Now().UnixNano()), "type": "function",
				"function": map[string]any{"name": name, "arguments": string(encoded)}},
		}}, "tool_calls")
	}
	if len(req.Messages) > 0 && strings.HasPrefix(text(0), "Return a JSON object with exactly one key") {
		m.mu.Lock()
		m.textCalls++
		m.mu.Unlock()
		say(`{"text": "London"}`)
		return
	}
	if len(req.Tools) == 0 {
		say("one moment") // the filler, or a wrap-up; neither is expected here
		return
	}
	m.mu.Lock()
	step := m.main
	m.main++
	m.mu.Unlock()
	switch step {
	case 0:
		call("browser_navigate", map[string]any{"url": m.page})
	case 1:
		call("browser_run", map[string]any{"goal": "Find flights from Zurich to London; done when results for London are visible."})
	default:
		last := text(len(req.Messages) - 1)
		if req.Messages[len(req.Messages)-1].Role == "tool" {
			m.mu.Lock()
			m.runResult = last
			m.mu.Unlock()
		}
		say("Flights from Zurich to London are showing: 3 options.")
	}
}

// e2eJudge plays TypeSafe: the browser's operation and target heads are
// answered from the candidates it was sent, the completion check is passed.
type e2eJudge struct {
	mu    sync.Mutex
	kinds []string
}

func (j *e2eJudge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State     map[string]any               `json:"state"`
		Questions map[string]decision.Question `json:"questions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	answers := map[string]decision.Answer{}
	pick := func(name string, q decision.Question, choice string) {
		ids := decision.Candidates(q)
		probs := map[string]float64{}
		for _, id := range ids {
			probs[id] = 0.1 / float64(max(len(ids)-1, 1))
		}
		probs[choice] = 0.9
		if len(ids) == 1 {
			probs[choice] = 1
		}
		answers[name] = decision.Answer{Choice: choice, Confidence: 0.92, Probabilities: probs}
	}
	labelled := func(q decision.Question, want string) string {
		for idx, raw := range q.Criteria {
			entry, _ := raw.(map[string]any)
			if el, _ := entry["element"].(string); strings.Contains(el, want) {
				return idx
			}
		}
		return decision.Candidates(q)[0]
	}
	if op, isBrowser := req.Questions["operation"]; isBrowser {
		page, _ := req.State["page"].(map[string]any)
		pageText, _ := page["text"].(string)
		var operation, target string
		switch {
		case strings.Contains(pageText, "Flights to London"):
			operation = "DONE"
		case req.Questions["type_text_target"].Criteria != nil && fieldEmpty(req.Questions["type_text_target"], "Where to?"):
			operation, target = "TYPE_TEXT", "Where to?"
		default:
			operation, target = "CLICK", "Search"
		}
		j.mu.Lock()
		j.kinds = append(j.kinds, "operation:"+operation)
		j.mu.Unlock()
		pick("operation", op, operation)
		for name, q := range req.Questions {
			if name != "operation" {
				pick(name, q, labelled(q, target))
			}
		}
	} else {
		for name, q := range req.Questions {
			j.mu.Lock()
			j.kinds = append(j.kinds, name)
			j.mu.Unlock()
			choice := decision.Candidates(q)[0]
			if name == decision.KindCompletion {
				choice = "verified"
			}
			pick(name, q, choice)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"answers": answers, "model": "jev-test",
		"usage": map[string]any{"input_tokens": 400, "output_tokens": 6}})
}

func fieldEmpty(q decision.Question, label string) bool {
	for _, raw := range q.Criteria {
		entry, _ := raw.(map[string]any)
		if el, _ := entry["element"].(string); strings.Contains(el, label) {
			return entry["current_value"] == nil
		}
	}
	return false
}

func TestFlightSearchEndToEndThroughTheWholeApp(t *testing.T) {
	if _, err := browser.FindBrowserBinary(""); err != nil {
		t.Skipf("no chromium available: %v", err)
	}
	if testing.Short() {
		t.Skip("short mode")
	}
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, e2eFlightsPage)
	}))
	defer site.Close()
	model := &e2eModel{t: t, page: site.URL}
	llm := httptest.NewServer(model)
	defer llm.Close()
	judge := &e2eJudge{}
	typesafe := httptest.NewServer(judge)
	defer typesafe.Close()

	cfg := testConfig(t)
	cfg.Provider.Type = "openai"
	cfg.Provider.APIBase = llm.URL + "/v1"
	cfg.Browser.Enabled = true
	cfg.Browser.Engine = "chromium"
	cfg.Browser.Headless = true
	cfg.Browser.NoSandbox = true
	// The profile lives outside t.TempDir: a browser being torn down keeps
	// writing to it for a moment, which fails the strict cleanup TempDir does.
	profile, err := os.MkdirTemp("", "factor-e2e-profile")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profile) })
	cfg.Browser.UserDataDir = profile
	cfg.Decision.Mode = "active"
	cfg.Decision.APIKey = "ts-e2e-key-12345"
	cfg.Decision.APIBase = typesafe.URL
	a := newTestApp(t, cfg)
	if _, ok := a.Registry.Get("browser_run"); !ok {
		t.Fatal("browser_run not mounted")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	reply, err := a.Loop.ProcessDirect(ctx, "Find me flights from Zurich to London", "cli:e2e")
	if err != nil {
		if strings.Contains(err.Error(), "browser start") {
			t.Skipf("chrome cannot start here: %v", err)
		}
		t.Fatalf("turn failed: %v", err)
	}
	if !strings.Contains(reply, "3 options") {
		t.Errorf("reply = %q", reply)
	}

	model.mu.Lock()
	result, textCalls := model.runResult, model.textCalls
	model.mu.Unlock()
	if strings.Contains(result, "browser start failed") {
		t.Skipf("chrome cannot start here: %s", result)
	}
	for _, want := range []string{"DONE", `TYPE_TEXT "Where to?" = "London"`, `CLICK "Search" → page changed`,
		`Withheld (allow_submit is false): "Book now"`, "Flights to London: 3 options"} {
		if !strings.Contains(result, want) {
			t.Errorf("browser_run result lacks %q:\n%s", want, result)
		}
	}
	if textCalls != 1 {
		t.Errorf("the light chain wrote %d values, want 1", textCalls)
	}
	judge.mu.Lock()
	kinds := strings.Join(judge.kinds, " ")
	judge.mu.Unlock()
	for _, want := range []string{"operation:TYPE_TEXT", "operation:CLICK", "operation:DONE", decision.KindCompletion} {
		if !strings.Contains(kinds, want) {
			t.Errorf("decisions asked = %q, missing %s", kinds, want)
		}
	}

	// Money and record: the decisions are billed to the session under the
	// model that answered, priced rather than listed as unpriced, and the
	// trace carries every verdict beside the tools.
	snap := a.Cost.Snapshot("cli:e2e")
	if _, ok := snap.Models["jev-test"]; !ok {
		t.Errorf("the decision model is not in the session's ledger: %v", snap.Models)
	}
	if unpriced := a.Cost.Unpriced(snap); len(unpriced) != 0 {
		t.Errorf("models left unpriced: %v", unpriced)
	}
	if snap.Session.USD <= 0 {
		t.Error("nothing was billed for the decisions")
	}
	recs, err := trace.Since(a.Traces(), time.Now().Add(-time.Minute))
	if err != nil || len(recs) == 0 {
		t.Fatalf("traces: %d records, err %v", len(recs), err)
	}
	var turn trace.Record
	for _, r := range recs {
		if r.Session == "cli:e2e" && r.Trigger == "user" {
			turn = r
		}
	}
	acted, completion, jevUSD := 0, 0, 0.0
	for _, d := range turn.Decisions {
		if d.Result == "acted" {
			acted++
		}
		if d.Kind == decision.KindCompletion {
			completion++
		}
	}
	for _, m := range turn.Models {
		if m.Model == "jev-test" {
			jevUSD += m.USD
		}
	}
	if acted < 5 || completion != 1 || jevUSD <= 0 {
		t.Errorf("trace: acted=%d completion=%d jevUSD=%v decisions=%+v", acted, completion, jevUSD, turn.Decisions)
	}
	tools := map[string]bool{}
	for _, tc := range turn.Tools {
		tools[tc.Name] = true
	}
	if !tools["browser_navigate"] || !tools["browser_run"] {
		t.Errorf("trace tools = %v", tools)
	}
}
