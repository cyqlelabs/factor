package evals

import (
	"context"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/cost"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// The prompt is the product, and none of it had a test. Each case below is a
// documented invariant of how a turn is assembled or run — the kind that
// breaks quietly when somebody edits a paragraph, and that no unit test on a
// function's return value would notice.

// --- what reaches the model ------------------------------------------------

// The system prompt has to be byte-identical across turns and across sessions.
// The whole request is ordered around that: a prompt cache reuses the longest
// identical prefix, and anything that varies here pushes the divergence to the
// head of the request, making every turn of a long session more expensive than
// the last. Nothing else in the codebase would notice this breaking.
func TestSystemPromptIsInvariantAcrossTurnsAndSessions(t *testing.T) {
	e := newEnv(t, answer("ok"))
	if _, err := e.say("cli:one", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.say("cli:one", "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.say("telegram:999", "elsewhere"); err != nil {
		t.Fatal(err)
	}

	reqs := e.chat.seen()
	first := reqs[0].Messages[0]
	if first.Role != "system" {
		t.Fatalf("the request does not open with the system prompt: %q", first.Role)
	}
	for i, req := range reqs[1:] {
		got := req.Messages[0]
		if got.Role != "system" || got.Content != first.Content {
			t.Errorf("request %d opens differently; the cacheable prefix is broken", i+1)
		}
	}
}

// The per-turn context — recall, the clock, the briefing — rides a user
// message. On the Anthropic dialect a system message is hoisted to the head of
// the request, which would silently undo the ordering above.
func TestTurnContextIsAUserMessageNotASystemOne(t *testing.T) {
	e := newEnv(t, answer("ok"))
	if _, err := e.say("cli:x", "what time is it"); err != nil {
		t.Fatal(err)
	}
	req := e.lastRequest()

	if strings.Contains(systemText(req), "Current time:") {
		t.Error("the clock reached the system prompt, which makes the prefix vary per turn")
	}
	if !strings.Contains(userText(req), "Current time:") {
		t.Errorf("the turn context did not arrive as a user message:\n%s", userText(req))
	}
	// And it is framed as machinery, so it is not read as the user speaking.
	if !strings.Contains(userText(req), "not a message from the user") {
		t.Error("the turn context does not say it is not the user talking")
	}
}

// A skill in the workspace has to be listed, or the catalog-in-prompt design
// is a directory nobody reads.
func TestWorkspaceSkillsAreListedInThePrompt(t *testing.T) {
	e := newEnv(t, answer("ok"))
	e.writeSkill("deploy-notes", "How this project ships a release", "Run make check first.")
	if _, err := e.say("cli:x", "hello"); err != nil {
		t.Fatal(err)
	}
	req := e.lastRequest()
	whole := systemText(req) + userText(req)
	if !strings.Contains(whole, "deploy-notes") {
		t.Errorf("the skill was not offered to the model:\n%s", whole)
	}
	// The catalog is a catalog: the body stays on disk until asked for.
	if strings.Contains(whole, "Run make check first") {
		t.Error("the skill body was inlined; progressive disclosure is not working")
	}
}

// Past rulesFadeAt the handful of rules that decay first are restated at the
// end of the request. The head of a request does not move as a conversation
// grows behind it, which is how a long session ends up ignoring the tools it
// has.
func TestOperatingRulesAreRestatedInALongSession(t *testing.T) {
	e := newEnv(t, answer("ok"))
	// A short session first: the rules are still near enough in the system
	// prompt, and there is nothing to restate.
	if _, err := e.say("cli:x", "hi"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(userText(e.lastRequest())), "job_start") {
		t.Error("the rules were restated to a session with two messages in it")
	}

	// Now grow the history itself past the fade point. The rule is about how
	// much conversation sits between the head of the request and its tail,
	// so it is the stored history that has to grow, not one long message.
	filler := strings.Repeat("a long conversation about many things. ", 400)
	for i := 0; i < 6; i++ {
		if _, err := e.say("cli:x", filler); err != nil {
			t.Fatal(err)
		}
	}
	long := userText(e.lastRequest())
	if !strings.Contains(strings.ToLower(long), "job_start") {
		t.Errorf("the rules that decay first were not restated:\n%s", tail(long, 800))
	}
}

// Every tool the model is offered needs a description. A tool with an empty
// one is a tool the model cannot choose, and nothing else would fail.
func TestEveryToolOfferedHasADescription(t *testing.T) {
	e := newEnv(t, answer("ok"))
	if _, err := e.say("cli:x", "hi"); err != nil {
		t.Fatal(err)
	}
	defs := e.lastRequest().Tools
	if len(defs) == 0 {
		t.Fatal("no tools were offered")
	}
	for _, d := range defs {
		if strings.TrimSpace(d.Description) == "" {
			t.Errorf("tool %q is offered with no description", d.Name)
		}
		if d.Parameters == nil {
			t.Errorf("tool %q is offered with no schema", d.Name)
		}
	}
}

// --- what the loop does with the answer -------------------------------------

// A tool result gets written into the history under the id that asked for it,
// or replay stops working on the next turn.
func TestToolResultIsRecordedAgainstItsCall(t *testing.T) {
	e := newEnv(t, callThen("list_dir", map[string]any{"path": "."}, "there are files"))
	reply, err := e.say("cli:x", "what is in the workspace")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "there are files" {
		t.Errorf("reply = %q", reply)
	}
	history, err := e.sessions.History("cli:x")
	if err != nil {
		t.Fatal(err)
	}
	var sawCall, sawResult bool
	for _, m := range history {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			sawCall = true
		}
		if m.Role == "tool" && m.ToolCallID == "c1" {
			sawResult = true
		}
	}
	if !sawCall || !sawResult {
		t.Errorf("the call and its result are not both in the history: %+v", history)
	}
}

// Older tool results are stubbed and the recent ones survive whole. This is
// where a long session's tokens actually are, and the stub has to name the
// call so the model can fetch it again.
func TestOldToolResultsAreStubbedAndRecentOnesSurvive(t *testing.T) {
	big := strings.Repeat("x", 2000)
	step := 0
	e := newEnv(t, func(_ int, _ *provider.Request) *provider.Response {
		step++
		// Five calls in the first turn, then answers. A turn inside its
		// budget is never masked while it runs — reading five files to
		// compare them needs all five — so the masking under test is what
		// the *next* turn inherits.
		if step <= 5 {
			return &provider.Response{ToolCalls: []provider.ToolCall{
				{ID: idFor(step), Name: "echo", Args: map[string]any{"text": big}},
			}}
		}
		return &provider.Response{Content: "done", FinishReason: "stop"}
	})
	e.registry.Register(&echoTool{})

	if _, err := e.say("cli:x", "read five things"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.say("cli:x", "and now summarize"); err != nil {
		t.Fatal(err)
	}
	req := e.lastRequest()

	var whole, stubbed int
	for _, m := range req.Messages {
		if m.Role != "tool" {
			continue
		}
		if strings.Contains(m.Content, "was cleared to save context") {
			stubbed++
			if !strings.Contains(m.Content, "echo") {
				t.Errorf("the stub does not name the call that produced it: %q", m.Content)
			}
		} else {
			whole++
		}
	}
	if stubbed == 0 {
		t.Error("nothing was masked in a turn with six large results")
	}
	if whole == 0 {
		t.Error("everything was masked; the model has nothing to react to")
	}
}

// A budget cap is a decision the user made. It comes back as a sentence the
// user can act on, not as a breakage.
func TestBudgetRefusalIsAnAnswerNotAnError(t *testing.T) {
	e := newEnv(t, answer("never reached"))
	e.chat.err = &cost.BudgetError{Scope: "session", Spent: 5, Limit: 5}

	reply, err := e.say("cli:x", "do something expensive")
	if err != nil {
		t.Fatalf("a cap should not surface as an error: %v", err)
	}
	if !strings.Contains(reply, "Budget cap reached") {
		t.Errorf("reply = %q", reply)
	}
}

// An image result rides a follow-up user message, and only a placeholder is
// persisted: session files stay small and replay stays valid on any model.
func TestImageResultRidesAUserMessageAndIsNotPersisted(t *testing.T) {
	e := newEnv(t, callThen("snap", nil, "I see it"))
	e.registry.Register(&snapTool{})

	if _, err := e.say("cli:x", "look at the screen"); err != nil {
		t.Fatal(err)
	}
	req := e.lastRequest()

	var carried bool
	for _, m := range req.Messages {
		if len(m.Images) > 0 {
			carried = true
			if m.Role != "user" {
				t.Errorf("the image rode a %q message", m.Role)
			}
		}
	}
	if !carried {
		t.Error("the image never reached the model")
	}
	history, _ := e.sessions.History("cli:x")
	for _, m := range history {
		if len(m.Images) > 0 {
			t.Error("an image was written into the session file")
		}
	}
}

// A path outside the workspace is refused. The guard is the one rule every
// file tool obeys, and a prompt change must never be able to talk past it.
func TestPathGuardRefusesOutsideTheWorkspace(t *testing.T) {
	e := newEnv(t, callThen("read_file", map[string]any{"path": "/etc/passwd"}, "could not"))
	if _, err := e.say("cli:x", "read /etc/passwd"); err != nil {
		t.Fatal(err)
	}
	var refused bool
	for _, req := range e.chat.seen() {
		for _, m := range req.Messages {
			if m.Role == "tool" && strings.HasPrefix(m.Content, "ERROR: ") {
				refused = true
			}
		}
	}
	if !refused {
		t.Error("a read outside the workspace was not refused")
	}
}

// --- helpers ---------------------------------------------------------------

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func idFor(step int) string { return "c" + string(rune('0'+step)) }

type echoTool struct{ tools.ReadOnly }

func (echoTool) Name() string        { return "echo" }
func (echoTool) Description() string { return "Echo the text back, for evaluating context handling." }
func (echoTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"text": map[string]any{"type": "string"},
	}}
}
func (echoTool) Execute(_ context.Context, args map[string]any) *tools.Result {
	return tools.Text(tools.StringArg(args, "text"))
}

type snapTool struct{}

func (snapTool) Name() string               { return "snap" }
func (snapTool) Description() string        { return "Return a picture, for evaluating image handling." }
func (snapTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (snapTool) Execute(context.Context, map[string]any) *tools.Result {
	return &tools.Result{
		ForLLM: "a screenshot",
		Images: []provider.ImagePart{{MediaType: "image/png", Data: "iVBORw0KGgo="}},
	}
}

// The request is marked where two consecutive turns stop being identical, so
// a dialect with an explicit prompt cache has somewhere to put a breakpoint.
// Nothing else fails when these move — the cache just quietly stops paying.
func TestCacheBreakpointsLandAtTheDocumentedBoundaries(t *testing.T) {
	e := newEnv(t, answer("ok"))
	if _, err := e.say("cli:x", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.say("cli:x", "second"); err != nil {
		t.Fatal(err)
	}
	req := e.lastRequest()

	if !req.Messages[0].CacheMark {
		t.Error("the system prompt is not marked; the largest fixed block goes uncached")
	}

	ctxIdx := -1
	for i, m := range req.Messages {
		if m.Role == "user" && strings.Contains(m.Content, "not a message from the user") {
			ctxIdx = i
		}
	}
	if ctxIdx < 1 {
		t.Fatal("no turn context in the request")
	}
	if !req.Messages[ctxIdx-1].CacheMark {
		t.Error("the end of the reusable prefix is not marked")
	}
	if req.Messages[ctxIdx].CacheMark {
		t.Error("the turn context is marked, so the cache is written over bytes nothing reads back")
	}
	if !req.Messages[len(req.Messages)-1].CacheMark {
		t.Error("the tail is not marked, so a tool-heavy turn reprocesses itself every iteration")
	}
}

// Who said something rides the message, not the prompt head. A name in the
// head is a head that differs per speaker, which costs the cache for everyone.
func TestSpeakerIsMarkedOnTheMessageNotThePromptHead(t *testing.T) {
	e := newEnv(t, answer("ok"))
	if _, err := e.loop.ProcessDirectNotice(context.Background(), "hello", "voice:local", "roxana", "", nil); err != nil {
		t.Fatal(err)
	}
	req := e.lastRequest()
	if strings.Contains(req.Messages[0].Content, "roxana") {
		t.Error("the speaker's name reached the system prompt")
	}
	if !strings.Contains(userText(req), "roxana") {
		t.Errorf("the speaker was not marked on the message:\n%s", userText(req))
	}
}

// A repeated identical call in one batch is answered once. The model asking
// twice is repeating itself, and both ids still need a result or replay stops.
func TestRepeatedCallInOneBatchRunsOnce(t *testing.T) {
	runs := 0
	e := newEnv(t, func(step int, _ *provider.Request) *provider.Response {
		if step == 0 {
			return &provider.Response{ToolCalls: []provider.ToolCall{
				{ID: "a", Name: "counter", Args: map[string]any{"k": "same"}},
				{ID: "b", Name: "counter", Args: map[string]any{"k": "same"}},
			}}
		}
		return &provider.Response{Content: "done", FinishReason: "stop"}
	})
	e.registry.Register(&counterTool{n: &runs})

	if _, err := e.say("cli:x", "do it twice"); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Errorf("the tool ran %d times for one distinct call", runs)
	}
	results := 0
	for _, m := range e.lastRequest().Messages {
		if m.Role == "tool" {
			results++
		}
	}
	if results != 2 {
		t.Errorf("%d results for two call ids; replay needs one each", results)
	}
}

type counterTool struct {
	tools.ReadOnly
	n *int
}

func (counterTool) Name() string        { return "counter" }
func (counterTool) Description() string { return "Count its own calls, for evaluating deduplication." }
func (counterTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"k": map[string]any{"type": "string"},
	}}
}
func (c counterTool) Execute(context.Context, map[string]any) *tools.Result {
	*c.n++
	return tools.Text("counted")
}
