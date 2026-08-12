package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/agent"
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
