package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// toolExchange builds one assistant call and its result.
func toolExchange(id, name, arg, result string) []provider.Message {
	return []provider.Message{
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: id, Name: name, Args: map[string]any{"url": arg}}}},
		{Role: "tool", ToolCallID: id, Content: result},
	}
}

// The bulk of a long session is not the conversation, it is the pages the
// agent read on the user's behalf. Those stop being worth their tokens as
// soon as the agent has acted on them — but only once it has.
func TestOldToolResultsAreMaskedAndRecentOnesAreNot(t *testing.T) {
	page := strings.Repeat("lorem ipsum ", 200)
	var history []provider.Message
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		history = append(history, toolExchange(id, "browser_navigate", "https://example.com/"+id, page)...)
	}

	masked := maskOldToolResults(history)

	var results []string
	for _, m := range masked {
		if m.Role == "tool" {
			results = append(results, m.Content)
		}
	}
	if len(results) != 6 {
		t.Fatalf("masking changed the shape of the history: %d results", len(results))
	}
	for i, got := range results[:2] {
		if got == page {
			t.Errorf("result %d was left whole; the oldest results are the ones worth clearing", i)
		}
		if !strings.Contains(got, "browser_navigate") {
			t.Errorf("stub %d does not name the tool that produced it: %q", i, got)
		}
	}
	for i, got := range results[2:] {
		if got != page {
			t.Errorf("result %d was masked, but only %d results are older than the keep window: %q",
				i+2, len(results)-keepRecentToolResults, got)
		}
	}
}

// A stub the agent cannot act on is just a hole. Keeping the call's own
// arguments is what makes the loss recoverable: the URL is the instruction
// for getting the page back.
func TestAMaskedResultKeepsWhatItWouldTakeToFetchItAgain(t *testing.T) {
	history := toolExchange("a", "browser_navigate", "https://example.com/dolar", strings.Repeat("x", 2000))
	history = append(history, make([]provider.Message, 0)...)
	for _, id := range []string{"b", "c", "d", "e"} {
		history = append(history, toolExchange(id, "read_file", "/tmp/"+id, "small")...)
	}

	got := maskOldToolResults(history)[1].Content
	for _, want := range []string{"browser_navigate", "https://example.com/dolar", "Run it again"} {
		if !strings.Contains(got, want) {
			t.Errorf("stub is missing %q: %q", want, got)
		}
	}
}

// Two results are never worth clearing: one small enough that the stub would
// not be shorter, and a failure, whose one useful sentence is that it failed.
func TestSmallResultsAreLeftAloneAndFailuresStaySignposted(t *testing.T) {
	var history []provider.Message
	history = append(history, toolExchange("a", "exec", "ls", "ERROR: "+strings.Repeat("boom ", 200))...)
	history = append(history, toolExchange("b", "exec", "pwd", "/home/nico")...)
	for _, id := range []string{"c", "d", "e", "f"} {
		history = append(history, toolExchange(id, "read_file", "/tmp/"+id, strings.Repeat("y", 1000))...)
	}

	masked := maskOldToolResults(history)
	if got := masked[1].Content; !strings.Contains(got, "failure") {
		t.Errorf("a cleared failure no longer reads as one, so the agent may retry it: %q", got)
	}
	if got := masked[3].Content; got != "/home/nico" {
		t.Errorf("a one-line result was replaced by a longer stub: %q", got)
	}
}

// Masking is a decision about one request. The transcript on disk is the
// record, and a record that quietly loses what a tool returned is worse than
// no record.
func TestMaskingDoesNotTouchTheHistoryItWasGiven(t *testing.T) {
	page := strings.Repeat("z", 3000)
	history := toolExchange("a", "browser_navigate", "https://example.com", page)
	for _, id := range []string{"b", "c", "d", "e"} {
		history = append(history, toolExchange(id, "read_file", "/tmp/"+id, "ok")...)
	}

	if masked := maskOldToolResults(history); masked[1].Content == page {
		t.Fatal("nothing was masked, so this proves nothing")
	}
	if history[1].Content != page {
		t.Errorf("the caller's history was rewritten: %q", history[1].Content)
	}
}

// Masking and compaction have to agree on what a session costs. Measuring the
// raw log would spend a summarizing call — and the fidelity it costs — on
// tokens the request was never going to carry.
func TestTheBudgetMeasuresTheMaskedHistoryNotTheRawLog(t *testing.T) {
	h := newHarness(t)
	page := strings.Repeat("lorem ipsum ", 400) // ~1.2k tokens each
	var history []provider.Message
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		history = append(history, toolExchange(id, "browser_navigate", "https://example.com/"+id, page)...)
	}

	h.loop.cfg.Agent.ContextWindowTokens = 0
	raw := estimateTokens(history)
	masked := estimateTokens(maskOldToolResults(history))
	if masked >= raw/2 {
		t.Fatalf("masking saved too little to tell the two apart: %d -> %d", raw, masked)
	}

	// A budget that clears the masked request but not the raw log: compaction
	// must not fire.
	h.loop.cfg.Agent.MaxContextTokens = h.loop.overhead() + masked + 200
	if h.loop.needsCompaction(history) {
		t.Error("compaction fired on tokens the request would never have carried")
	}
	h.loop.cfg.Agent.MaxContextTokens = h.loop.overhead() + masked - 200
	if !h.loop.needsCompaction(history) {
		t.Error("compaction did not fire when even the masked request overflows")
	}
}

// fatTool returns a result at the registry's per-result cap, which is what a
// browser read or a long log actually costs.
type fatTool struct{}

func (f *fatTool) Name() string        { return "fat" }
func (f *fatTool) Description() string { return "returns a page-sized result" }
func (f *fatTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (f *fatTool) Execute(context.Context, map[string]any) *tools.Result {
	return tools.Text(strings.Repeat("x", 16000))
}

func fatCall(id string) func(*provider.Request) (*provider.Response, error) {
	return func(*provider.Request) (*provider.Response, error) {
		return &provider.Response{ToolCalls: []provider.ToolCall{{ID: id, Name: "fat", Args: map[string]any{}}}}, nil
	}
}

// runFatTurn spends six tool iterations on page-sized results and returns the
// request that carried the turn's last call.
func runFatTurn(t *testing.T, ceiling int) []provider.Message {
	t.Helper()
	var script []func(*provider.Request) (*provider.Response, error)
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		script = append(script, fatCall(id))
	}
	script = append(script, final("listo"))

	h := newHarness(t, script...)
	h.loop.cfg.Agent.MaxToolIterations = 8
	h.loop.cfg.Agent.MaxContextTokens = ceiling
	h.registry.Register(&fatTool{})

	if _, err := h.loop.ProcessDirect(context.Background(), "trabaja", "cli:test"); err != nil {
		t.Fatal(err)
	}
	// Compaction may follow the turn onto the same fake provider; let it land
	// before reading what the fake recorded.
	h.loop.WaitBackground(10 * time.Second)

	if len(h.chat.requests) < 7 {
		t.Fatalf("the turn made %d calls, not the seven it was scripted for", len(h.chat.requests))
	}
	return h.chat.requests[6].Messages
}

func countToolResults(msgs []provider.Message) (stubs, whole int) {
	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		if strings.Contains(m.Content, "was cleared to save context") {
			stubs++
			continue
		}
		whole++
	}
	return stubs, whole
}

// Masking used to run once, when the request was assembled, and then leave the
// turn free to grow behind it. Twenty tool iterations at four thousand tokens
// a result is more than the whole budget, so a long turn ran past the working
// ceiling with nothing noticing until the provider refused the request — and a
// request that far past the ceiling is exactly where the rules at the head of
// the prompt stop being read.
func TestALongTurnIsTrimmedWhileItIsStillRunning(t *testing.T) {
	stubs, whole := countToolResults(runFatTurn(t, 12000))
	if stubs == 0 {
		t.Errorf("a turn six page-sized results past a 12k budget was never trimmed")
	}
	if whole != keepRecentToolResults {
		t.Errorf("the turn kept %d results whole, want the newest %d", whole, keepRecentToolResults)
	}
}

// The other half of the rule: under budget nothing is touched. A turn reading
// six pages to compare them needs all six, and clearing half to save tokens
// that were affordable is answering with half the evidence.
func TestATurnWithinBudgetKeepsEveryResult(t *testing.T) {
	stubs, whole := countToolResults(runFatTurn(t, 1<<20))
	if stubs != 0 {
		t.Errorf("a turn well inside its budget lost %d results", stubs)
	}
	if whole != 6 {
		t.Errorf("the turn carried %d results, want all 6", whole)
	}
}
