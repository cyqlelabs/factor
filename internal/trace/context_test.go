package trace

import (
	"context"
	"testing"
	"time"
)

// The turn rides the context so anything working on the turn's behalf can
// note what happened to it, and nothing has to check for a missing one:
// every method on a nil Turn is a no-op.
func TestTurnRidesTheContext(t *testing.T) {
	if got := TurnFrom(context.Background()); got != nil {
		t.Errorf("a bare context carried %v", got)
	}
	TurnFrom(context.Background()).Event(EventRecallFailed, "nobody is hurt") // nil-safe

	rec := NewRecorder(t.TempDir(), Config{Enabled: true, KeepDays: 14}, nil)
	turn := rec.Begin("cli:x", "user", "")
	ctx := WithTurn(context.Background(), turn)
	if got := TurnFrom(ctx); got != turn {
		t.Errorf("TurnFrom = %v, want the stamped turn", got)
	}
	if got := WithTurn(context.Background(), nil); TurnFrom(got) != nil {
		t.Error("stamping nil produced a turn")
	}
	TurnFrom(ctx).Event(EventRecallFailed, "engine down")
	turn.End("ok", nil)
	recs, err := Since(rec.Dir(), time.Now().Add(-time.Minute))
	if err != nil || len(recs) != 1 || recs[0].Count(EventRecallFailed) != 1 {
		t.Errorf("records = %+v, %v", recs, err)
	}
}
