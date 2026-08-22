package agent

import (
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/provider"
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
