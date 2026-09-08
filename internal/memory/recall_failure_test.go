package memory

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
	"github.com/cyqlelabs/factor/internal/trace"
)

// downEngine is an engine that does not answer, which is what a sidecar
// mid-restart or one swapped out by the kernel looks like from here.
type downEngine struct{ stubEngine }

func (*downEngine) Recall(context.Context, string, int, float64, Scope) ([]Memory, error) {
	return nil, errors.New("smrti unreachable: context deadline exceeded")
}
func (*downEngine) Healthy() bool { return false }
func (*downEngine) Enabled() bool { return true }

// A recall the engine did not answer is said so to the turn and recorded on
// it. Silent, the model read an empty recall as "nothing was ever saved"
// and rebuilt what it had learned the week before; nothing counted how
// often that happened.
func TestRecallFailureIsNamedToTheTurnAndTraced(t *testing.T) {
	a := NewAmbient(&downEngine{}, 5, 0.1, 5, 500, 500, nil, testPolicy())
	dir := t.TempDir()
	rec := trace.NewRecorder(dir, trace.Config{Enabled: true, KeepDays: 14}, nil)
	turn := rec.Begin("cli:x", "user", "")
	ctx := trace.WithTurn(tools.WithToolContext(context.Background(), tools.ToolContext{Channel: "cli"}), turn)

	out := a.MemoryPrompt(ctx, []provider.Message{{Role: "user", Content: "earlier"}}, "how do I send the report")
	if !strings.Contains(out, "could not be consulted") || !strings.Contains(out, "skills catalog") {
		t.Fatalf("the turn was not told memory is unavailable: %q", out)
	}
	if strings.Contains(out, "Recalled from long-term memory") {
		t.Error("an unavailable memory was rendered as a recall")
	}
	turn.End("ok", nil)
	recs, err := trace.Since(dir, time.Now().Add(-time.Minute))
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %v, %v", recs, err)
	}
	if recs[0].Count(trace.EventRecallFailed) != 1 {
		t.Errorf("the failure was not recorded on the turn: %+v", recs[0].Events)
	}
}

// One of the two queries failing while the other answers is still a recall.
func TestRecallSurvivesOneQueryFailing(t *testing.T) {
	eng := &flakyEngine{}
	a := NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, testPolicy())
	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{Channel: "cli"})
	out := a.MemoryPrompt(ctx, []provider.Message{{Role: "user", Content: "the tail of the conversation"}}, "and her?")
	if !strings.Contains(out, "one good answer") || strings.Contains(out, "could not be consulted") {
		t.Errorf("out = %q", out)
	}
}

// flakyEngine fails the first query it is asked and answers the second.
type flakyEngine struct {
	stubEngine
	calls atomic.Int32
}

func (e *flakyEngine) Recall(context.Context, string, int, float64, Scope) ([]Memory, error) {
	if e.calls.Add(1) == 1 {
		return nil, errors.New("timeout")
	}
	return []Memory{{ID: "m1", Content: "one good answer", Confidence: 0.8, Severity: SeverityContext}}, nil
}
func (e *flakyEngine) Enabled() bool { return true }
func (e *flakyEngine) Healthy() bool { return true }
