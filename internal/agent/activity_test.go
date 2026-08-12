package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/provider"
)

// collect records every activity a turn emits.
func collect(t *testing.T, h *harness) *[]Activity {
	t.Helper()
	var mu sync.Mutex
	var seen []Activity
	h.loop.OnActivity(func(act Activity) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, act)
	})
	return &seen
}

func phases(acts []Activity) []Phase {
	out := make([]Phase, 0, len(acts))
	for _, a := range acts {
		out = append(out, a.Phase)
	}
	return out
}

func equalPhases(got []Phase, want ...Phase) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestActivityReportsEveryPhaseOfAToolTurn(t *testing.T) {
	h := newHarness(t,
		toolCall("probe", map[string]any{"value": "abc"}),
		final("done"),
	)
	seen := collect(t, h)

	if _, err := h.loop.ProcessDirect(context.Background(), "go", "cli:test"); err != nil {
		t.Fatal(err)
	}

	got := phases(*seen)
	if !equalPhases(got, PhaseContext, PhaseThinking, PhaseTool, PhaseThinking, PhaseDone) {
		t.Fatalf("phases = %v", got)
	}
	for _, act := range *seen {
		if act.SessionKey != "cli:test" {
			t.Errorf("session key = %q", act.SessionKey)
		}
		if act.Phase == PhaseTool && act.Detail != "probe" {
			t.Errorf("tool detail = %q, want the tool name", act.Detail)
		}
	}
}

func TestActivityReportsDoneWhenATurnFails(t *testing.T) {
	h := newHarness(t, func(*provider.Request) (*provider.Response, error) {
		return nil, context.DeadlineExceeded
	})
	seen := collect(t, h)

	if _, err := h.loop.ProcessDirect(context.Background(), "go", "cli:test"); err == nil {
		t.Fatal("want the provider error")
	}
	got := phases(*seen)
	if len(got) == 0 || got[len(got)-1] != PhaseDone {
		t.Errorf("phases = %v, want a trailing done", got)
	}
}

func TestActivityReportsSteering(t *testing.T) {
	h := newHarness(t, final("first"), final("second"))
	seen := collect(t, h)

	// Steer a message in before the turn starts, so the first final answer is
	// folded back into another provider round.
	claimed, ok := h.loop.claim("cli:test", nil)
	if !ok {
		t.Fatal("could not claim the session")
	}
	claimed.steering <- bus.InboundMessage{Channel: "cli", ChatID: "test", Content: "more context"}
	defer h.loop.release("cli:test", claimed)

	reply, err := h.loop.execute(context.Background(), turnInput{sessionKey: "cli:test", content: "go"}, claimed)
	if err != nil {
		t.Fatal(err)
	}
	if reply != "second" {
		t.Errorf("reply = %q, want the answer after steering", reply)
	}
	found := false
	for _, act := range *seen {
		if act.Phase == PhaseSteering {
			found = true
		}
	}
	if !found {
		t.Errorf("no steering phase in %v", phases(*seen))
	}
}

func TestOnActivityCanBeCleared(t *testing.T) {
	h := newHarness(t, final("ok"))
	seen := collect(t, h)
	h.loop.OnActivity(nil)

	if _, err := h.loop.ProcessDirect(context.Background(), "go", "cli:test"); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 0 {
		t.Errorf("watcher still called after being cleared: %v", phases(*seen))
	}
}
