package agent

import (
	"testing"

	"github.com/cyqlelabs/factor/internal/provider"
)

// markedRoles names what carries a cache mark, so placement can be asserted
// without depending on how many messages the fixture happens to hold.
func markedRoles(msgs []provider.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.CacheMark {
			out = append(out, m.Role+":"+m.Content)
		}
	}
	return out
}

func indexOfMark(msgs []provider.Message, from int) int {
	for i := from; i < len(msgs); i++ {
		if msgs[i].CacheMark {
			return i
		}
	}
	return -1
}

// The system prompt is invariant and the summary is not. Marking both means a
// compaction that rewrites the summary costs the summary's cache entry, not
// the tool schemas and system prompt rendered in front of it.
func TestAssembleMarksEverySystemMessage(t *testing.T) {
	h := newHarness(t)
	if err := h.loop.sessions.SetSummary("cli:x", "earlier things", 0); err != nil {
		t.Fatal(err)
	}
	msgs := h.loop.assemble("PROMPT", "CONTEXT", "cli:x", false, nil,
		[]provider.Message{{Role: "user", Content: "hi"}})

	var systems int
	for _, m := range msgs {
		if m.Role == "system" {
			systems++
			if !m.CacheMark {
				t.Errorf("system message %q carries no cache mark", m.Content)
			}
		}
	}
	if systems != 2 {
		t.Fatalf("expected the prompt and the summary, got %d system messages", systems)
	}
}

// turnContext is where two consecutive turns first differ — recall and the
// clock change every time. A mark after it would write an entry over bytes
// nothing ever reads back; the mark belongs on the last message before it.
func TestAssembleMarksTheEndOfPriorHistoryNotTheTail(t *testing.T) {
	h := newHarness(t)
	prior := []provider.Message{
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "old answer"},
	}
	msgs := h.loop.assemble("PROMPT", "TURN CONTEXT", "cli:x", true, prior,
		[]provider.Message{{Role: "user", Content: "new question"}})

	var ctxIdx = -1
	for i, m := range msgs {
		if m.Content == "TURN CONTEXT" {
			ctxIdx = i
		}
	}
	if ctxIdx < 1 {
		t.Fatalf("turn context not found in %+v", markedRoles(msgs))
	}
	if !msgs[ctxIdx-1].CacheMark {
		t.Errorf("the message before turnContext should close the reusable prefix, marks: %v", markedRoles(msgs))
	}
	for i := ctxIdx; i < len(msgs); i++ {
		if msgs[i].CacheMark {
			t.Errorf("assemble marked %q at or after turnContext", msgs[i].Content)
		}
	}
}

// A turn with no history behind it still has the fixed head worth caching.
func TestAssembleMarksHeadWithNoPriorHistory(t *testing.T) {
	h := newHarness(t)
	msgs := h.loop.assemble("PROMPT", "", "cli:x", true, nil,
		[]provider.Message{{Role: "user", Content: "hi"}})
	if indexOfMark(msgs, 0) != 0 {
		t.Errorf("the system prompt should be marked, marks: %v", markedRoles(msgs))
	}
}

// The growing half of a turn is what a tool-heavy session re-processes twenty
// times over. markTail is what gives each iteration something to read.
func TestMarkTailMarksTheLastMessage(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "p", CacheMark: true},
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "a"},
	}
	markTail(msgs)
	if !msgs[2].CacheMark {
		t.Error("the tail carries no mark")
	}
	if !msgs[0].CacheMark {
		t.Error("markTail must not disturb the fixed head")
	}
}

func TestMarkTailToleratesAnEmptyRequest(t *testing.T) {
	markTail(nil)
	markTail([]provider.Message{})
}

// Marks describe one request, so they must never reach the session file: a
// replayed history that arrived pre-marked would place breakpoints by accident
// of what some earlier turn did.
func TestCacheMarksDoNotReachTheSessionStore(t *testing.T) {
	h := newHarness(t)
	if err := h.loop.sessions.Append("cli:x", provider.Message{
		Role: "user", Content: "hi", CacheMark: true,
	}); err != nil {
		t.Fatal(err)
	}
	back, err := h.loop.sessions.History("cli:x")
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 {
		t.Fatalf("history = %+v", back)
	}
	if back[0].CacheMark {
		t.Error("a cache mark was persisted and read back")
	}
}
