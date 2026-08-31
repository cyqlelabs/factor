package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/skills"
	"github.com/cyqlelabs/factor/internal/tools"
	"github.com/cyqlelabs/factor/internal/trace"
)

func turnOf(calls int) []provider.Message {
	msgs := []provider.Message{{Role: "user", Content: "do the thing"}}
	for i := 0; i < calls; i++ {
		msgs = append(msgs,
			provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "t", Name: "read_file"}}},
			provider.Message{Role: "tool", ToolCallID: "t", Content: "ok"})
	}
	return append(msgs, provider.Message{Role: "assistant", Content: "done"})
}

func pendingFor(l *Loop, key string) (induceCandidate, bool) {
	l.induceMu.Lock()
	defer l.induceMu.Unlock()
	c, ok := l.pendingInduce[key]
	return c, ok
}

// Steering is the cheapest feedback the system gets and the only kind nobody
// has to be asked for. A turn that needed it is a better thing to learn from
// than a long one that went smoothly, so it qualifies on fewer calls.
func TestCorrectedTurnQualifiesOnFewerToolCalls(t *testing.T) {
	h := newHarness(t)
	in := turnInput{sessionKey: "cli:x", toolCtx: tools.ToolContext{SessionKey: "cli:x"}}
	msgs := turnOf(induceCorrectedToolCalls)

	h.loop.noteInduceCandidate(in, msgs, len(msgs), "done", false)
	if _, ok := pendingFor(h.loop, "cli:x"); ok {
		t.Fatal("an uncorrected turn below the ordinary floor should not qualify")
	}

	h.loop.noteInduceCandidate(in, msgs, len(msgs), "done", true)
	cand, ok := pendingFor(h.loop, "cli:x")
	if !ok {
		t.Fatal("a corrected turn at the lower floor should qualify")
	}
	if !cand.corrected {
		t.Error("the candidate did not remember that it was corrected")
	}
}

// Below even the lowered floor there is no workflow, corrected or not.
func TestCorrectionDoesNotQualifyATrivialTurn(t *testing.T) {
	h := newHarness(t)
	in := turnInput{sessionKey: "cli:x", toolCtx: tools.ToolContext{SessionKey: "cli:x"}}
	msgs := turnOf(1)
	h.loop.noteInduceCandidate(in, msgs, len(msgs), "done", true)
	if _, ok := pendingFor(h.loop, "cli:x"); ok {
		t.Error("one tool call is not a workflow, however it went")
	}
}

// The trajectory of a corrected turn holds a wrong approach before the right
// one, and the prompt has to say so or the model learns the detour.
func TestInduceInputNamesTheCorrection(t *testing.T) {
	var none []skills.Skill
	plain := induceInput(induceCandidate{task: "t", transcript: "x"}, none, none, false)
	if strings.Contains(plain, "corrected this turn") {
		t.Error("an uncorrected turn should not claim it was corrected")
	}
	fixed := induceInput(induceCandidate{task: "t", transcript: "x", corrected: true}, none, none, false)
	if !strings.Contains(fixed, "corrected this turn") {
		t.Errorf("the correction was not passed on:\n%s", fixed)
	}
}

// The seam a connector uses to report what the loop cannot see. It must be
// safe with tracing off, which is how most of these calls will land.
func TestNoteFeedbackReachesTheTrace(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	rec := trace.NewRecorder(dir, trace.Config{Enabled: true}, nil)
	t.Cleanup(func() { _ = rec.Close() })
	h.loop.WithTracer(rec)

	turn := rec.Begin("voice:local", "user", "")
	h.loop.NoteFeedback("voice:local", trace.EventBargeIn, "talked over the reply")
	turn.End("interrupted", nil)

	recs, err := trace.Since(dir, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Count(trace.EventBargeIn) != 1 {
		t.Fatalf("records = %+v", recs)
	}
	// An interruption is the user's correction, not a fault to report.
	if recs[0].Failed() {
		t.Error("a barged-in turn must not read as a failure")
	}
}

func TestNoteFeedbackWithoutATracerIsSafe(t *testing.T) {
	h := newHarness(t)
	h.loop.WithTracer(nil)
	h.loop.NoteFeedback("cli:x", trace.EventBargeIn, "")
}
