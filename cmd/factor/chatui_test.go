package main

import (
	"context"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/tui"
)

// pipedUI wires a chat UI to a pipe so its output can be read back. Without
// a terminal the console prints plain lines, which is all the assertions
// here care about.
func pipedUI(t *testing.T) (*chatUI, func() string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ui := newChatUI(tui.NewChat(nil, w))
	return ui, func() string {
		_ = w.Close()
		out, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		return string(out)
	}
}

func act(phase agent.Phase, detail string) agent.Activity {
	return agent.Activity{SessionKey: "cli:main", Phase: phase, Detail: detail}
}

// flakyMemory is an enabled engine whose health a test can flip.
type flakyMemory struct {
	memory.Noop
	healthy bool
}

func (f *flakyMemory) Enabled() bool { return true }
func (f *flakyMemory) Healthy() bool { return f.healthy }

func TestChatBarShowsWhereYouAreAndHowToTypeANewline(t *testing.T) {
	bar := chatBar("main", "some/model", &flakyMemory{healthy: true}, nil)
	if bar.Session != "main" || bar.Model != "some/model" {
		t.Errorf("bar = %+v", bar)
	}
	if bar.Memory != "memory ✓" {
		t.Errorf("memory = %q, want a healthy mark", bar.Memory)
	}
	if len(bar.Hints) == 0 || !strings.Contains(bar.Hints[0], "newline") {
		t.Errorf("hints = %v, want the newline shortcut first", bar.Hints)
	}

	if got := chatBar("main", "m", &flakyMemory{}, nil).Memory; got != "memory ✗" {
		t.Errorf("unhealthy memory = %q", got)
	}
	// Memory turned off is left out of the bar entirely.
	if got := chatBar("main", "m", memory.Noop{}, nil).Memory; got != "" {
		t.Errorf("disabled memory = %q, want nothing", got)
	}
	if got := chatBar("main", "m", nil, nil).Memory; got != "" {
		t.Errorf("absent memory = %q, want nothing", got)
	}
}

func TestChatUIRefreshesTheBarAtTurnBoundaries(t *testing.T) {
	ui, _ := pipedUI(t)
	calls := 0
	ui.bar = func() tui.Bar { calls++; return tui.Bar{Session: "main"} }

	ui.begin("cli:main")
	ui.activity(act(agent.PhaseDone, ""))
	if calls != 2 {
		t.Errorf("bar refreshed %d times, want one per turn boundary", calls)
	}

	// A UI without a bar source must not panic.
	plain, _ := pipedUI(t)
	plain.refreshBar()
}

// The bar is drawn before the sidecar finishes warming up, so without a
// repaint between turns a healthy engine kept showing "memory ✗" until the
// first message was sent.
func TestChatUIWatchBarRepaintsWhileIdle(t *testing.T) {
	ui, _ := pipedUI(t)
	var calls atomic.Int32
	ui.bar = func() tui.Bar { calls.Add(1); return tui.Bar{Session: "main"} }

	ctx, cancel := context.WithCancel(context.Background())
	go ui.watchBar(ctx, time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if calls.Load() < 3 {
		t.Error("watchBar never repainted the idle bar")
	}
	n := calls.Load()
	time.Sleep(20 * time.Millisecond)
	if calls.Load() > n+1 { // one tick may already be in flight at cancel
		t.Error("watchBar kept repainting after its context was cancelled")
	}
}

func TestChatUIReportsWhatTheTurnDid(t *testing.T) {
	ui, output := pipedUI(t)

	ui.begin("cli:main")
	if !ui.spin.Running() {
		t.Fatal("the pulse should start the moment a message is sent")
	}
	ui.activity(act(agent.PhaseContext, ""))
	ui.activity(act(agent.PhaseTool, "read_file"))
	ui.activity(act(agent.PhaseDone, ""))
	if ui.spin.Running() {
		t.Error("the pulse should stop when the turn is done")
	}
	ui.reply("here you go")

	out := output()
	if !strings.Contains(out, "factor› here you go") {
		t.Errorf("reply not printed: %q", out)
	}
	if !strings.Contains(out, "read_file") {
		t.Errorf("summary note missing the tool it ran: %q", out)
	}
}

func TestChatUIPrintsWhatTheAgentSaysMidTurn(t *testing.T) {
	ui, output := pipedUI(t)

	ui.begin("cli:main")
	ui.activity(act(agent.PhaseNotice, "Reading the config first."))
	if !ui.spin.Running() {
		t.Error("a mid-turn note should not end the turn it came from")
	}
	ui.activity(act(agent.PhaseDone, ""))
	ui.reply("here you go")

	out := output()
	if !strings.Contains(out, "Reading the config first.") {
		t.Errorf("mid-turn note not printed: %q", out)
	}
	if strings.Index(out, "Reading the config first.") > strings.Index(out, "here you go") {
		t.Errorf("the note printed after the answer it introduced: %q", out)
	}
}

func TestChatUIAdoptsTurnsNobodyAskedFor(t *testing.T) {
	ui, output := pipedUI(t)

	// A finished background job re-enters the session on its own.
	ui.activity(act(agent.PhaseThinking, ""))
	if !ui.spin.Running() {
		t.Fatal("an unprompted turn should still show the pulse")
	}
	ui.activity(act(agent.PhaseDone, ""))
	ui.reply("your build finished")

	if out := output(); !strings.Contains(out, "your build finished") {
		t.Errorf("proactive message not printed: %q", out)
	}
}

func TestChatUIIgnoresOtherChannels(t *testing.T) {
	ui, output := pipedUI(t)

	ui.activity(agent.Activity{SessionKey: "telegram:42", Phase: agent.PhaseThinking})
	if ui.spin.Running() {
		t.Error("a Telegram turn should not drive the CLI's pulse")
	}

	// A CLI turn on another session is ignored while one is already live.
	ui.begin("cli:main")
	ui.activity(agent.Activity{SessionKey: "cli:other", Phase: agent.PhaseTool, Detail: "exec_command"})
	ui.activity(act(agent.PhaseDone, ""))
	ui.reply("done")

	if out := output(); strings.Contains(out, "exec_command") {
		t.Errorf("another session's tool leaked into the summary: %q", out)
	}
}

func TestChatUIAbortLeavesNoSummaryBehind(t *testing.T) {
	ui, output := pipedUI(t)

	ui.begin("cli:main")
	ui.activity(act(agent.PhaseTool, "read_file"))
	ui.abort() // the message never made it onto the bus
	if ui.spin.Running() {
		t.Error("abort should stop the pulse")
	}
	ui.reply("an unrelated proactive message")

	out := output()
	if strings.Contains(out, "read_file") {
		t.Errorf("the aborted turn's summary stuck around: %q", out)
	}
	if !strings.Contains(out, "an unrelated proactive message") {
		t.Errorf("message not printed: %q", out)
	}
}

func TestChatUIKeepsOneClockAcrossSteering(t *testing.T) {
	ui, _ := pipedUI(t)

	ui.begin("cli:main")
	ui.activity(act(agent.PhaseTool, "read_file"))
	ui.begin("cli:main") // a steering message joins the running turn
	ui.activity(act(agent.PhaseSteering, ""))
	ui.activity(act(agent.PhaseDone, ""))

	ui.mu.Lock()
	sum := ui.summary
	ui.mu.Unlock()
	if sum == nil {
		t.Fatal("no summary after the turn ended")
	}
	if sum.Steps != 1 || len(sum.Tools) != 1 {
		t.Errorf("summary = %+v, want the steps from before the steering", sum)
	}
}
