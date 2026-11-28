package agent

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/tools"
)

// gatewayLike wires a loop the way the gateway does: telegram and voice run,
// and only telegram is a written chat.
func gatewayLike(l *Loop) {
	running := map[string]bool{"telegram": true, "voice": true, "slack": true}
	l.SetReachable(func(name string) bool { return running[name] })
	l.SetConversational(func(name string) bool { return name != "voice" })
}

func sendMessage(t *testing.T, tool tools.Tool, tc tools.ToolContext, args map[string]any) *tools.Result {
	t.Helper()
	return tool.Execute(tools.WithToolContext(context.Background(), tc), args)
}

func delivered(t *testing.T, b *bus.MessageBus) bus.OutboundMessage {
	t.Helper()
	select {
	case out := <-b.Outbound():
		return out
	default:
		t.Fatal("nothing was published")
		return bus.OutboundMessage{}
	}
}

// A task another agent handed this machine answers that agent, and a spoken
// turn answers out loud: message_user is how either puts text in front of
// the user, in the chat they last wrote in — past a spoken conversation.
func TestMessageUserWritesToTheLastWrittenChat(t *testing.T) {
	h := newHarness(t)
	gatewayLike(h.loop)
	h.loop.recordLastChannel(bus.InboundMessage{Channel: "telegram", ChatID: "42"})
	h.loop.recordLastChannel(bus.InboundMessage{Channel: "voice", ChatID: "local"})
	// Slack runs and is written, but nobody has written there yet.
	if got := h.loop.WrittenChats(); !slices.Equal(got, []string{"telegram"}) {
		t.Errorf("WrittenChats = %v", got)
	}
	tool := h.loop.MessageTool()
	if tool.Name() != MessageToolName || tool.Description() == "" || tool.Parameters()["type"] != "object" {
		t.Error("the tool schema is incomplete")
	}

	remote := tools.ToolContext{Channel: "a2a", ChatID: "peer:ctx", Audience: tools.AudienceShared}
	res := sendMessage(t, tool, remote, map[string]any{"message": "the report"})
	if res.IsError || strings.Contains(res.ForLLM, "spoken") {
		t.Fatalf("remote turn: %s", res.ForLLM)
	}
	if out := delivered(t, h.bus); out.Channel != "telegram" || out.ChatID != "42" || out.Content != "the report" {
		t.Errorf("outbound = %+v", out)
	}

	spoken := tools.ToolContext{Channel: "voice", ChatID: "local"}
	res = sendMessage(t, tool, spoken, map[string]any{"message": "the code", "channel": "Telegram"})
	if res.IsError || !strings.Contains(res.ForLLM, "one short sentence") {
		t.Fatalf("spoken turn: %s", res.ForLLM)
	}
	delivered(t, h.bus)

	// The next process finds the written chat behind the spoken one.
	if res := sendMessage(t, restartedWired(t, h).MessageTool(), remote, map[string]any{"message": "x"}); res.IsError {
		t.Errorf("after a restart: %s", res.ForLLM)
	}
}

func restartedWired(t *testing.T, h *harness) *Loop {
	l := restarted(t, h)
	gatewayLike(l)
	return l
}

func TestMessageUserRefuses(t *testing.T) {
	h := newHarness(t)
	gatewayLike(h.loop)
	tool := h.loop.MessageTool()
	remote := tools.ToolContext{Channel: "a2a", ChatID: "peer:ctx", Audience: tools.AudienceShared}
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"message": " "}, "required"},
		{map[string]any{"message": "x"}, "no written chat"},
		{map[string]any{"message": "x", "channel": "terminal"}, "no terminal"},
		{map[string]any{"message": "x", "channel": "phone"}, "not running"},
		{map[string]any{"message": "x", "channel": "voice"}, "not a written chat"},
		{map[string]any{"message": "x", "channel": "slack"}, "has not written"},
	} {
		if res := sendMessage(t, tool, remote, c.args); !res.IsError || !strings.Contains(res.ForLLM, c.want) {
			t.Errorf("%v: %q, want %q", c.args, res.ForLLM, c.want)
		}
	}

	h.loop.recordLastChannel(bus.InboundMessage{Channel: "telegram", ChatID: "42"})
	// A written chat answers itself; the message would arrive twice.
	own := tools.ToolContext{Channel: "telegram", ChatID: "42"}
	if res := sendMessage(t, tool, own, map[string]any{"message": "x"}); !res.IsError {
		t.Error("a turn wrote to its own chat")
	}
	// A private turn writes nothing into a chat somebody else can read.
	h.loop.SetAudience(func(string, string) string { return tools.AudienceShared })
	private := tools.ToolContext{Channel: "voice", ChatID: "local"}
	if res := sendMessage(t, tool, private, map[string]any{"message": "x"}); !res.IsError {
		t.Error("a private turn wrote into a shared chat")
	}
	if res := sendMessage(t, tool, remote, map[string]any{"message": "dinner"}); res.IsError {
		t.Errorf("a shared turn was refused: %s", res.ForLLM)
	}
}

// A record written before chats were kept per channel still names the last
// one.
func TestMessageUserReadsAnOlderRecord(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(lastChannelPath(), []byte(`{"channel":"telegram","chat_id":"42"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	l := restartedWired(t, h)
	res := sendMessage(t, l.MessageTool(), tools.ToolContext{Channel: "a2a"}, map[string]any{"message": "x"})
	if res.IsError || !strings.Contains(res.ForLLM, "telegram") {
		t.Errorf("older record: %s", res.ForLLM)
	}
}

// A chat session writes only to its own terminal, and only for its spoken
// turns: a typed one is already in the terminal.
func TestTerminalMessageUserWritesToTheTerminal(t *testing.T) {
	h := newHarness(t)
	tool := h.loop.TerminalMessageTool("main")
	spoken := tools.ToolContext{Channel: "voice", ChatID: "local"}
	res := sendMessage(t, tool, spoken, map[string]any{"message": "the list"})
	if res.IsError || !strings.Contains(res.ForLLM, "one short sentence") {
		t.Fatalf("spoken turn: %s", res.ForLLM)
	}
	if out := delivered(t, h.bus); out.Channel != "cli" || out.ChatID != "main" {
		t.Errorf("outbound = %+v", out)
	}
	if res := sendMessage(t, tool, spoken, map[string]any{"message": "x", "channel": "telegram"}); !res.IsError {
		t.Error("a chat session claimed a telegram chat")
	}
	for _, session := range []string{"main", "main-1764280000"} { // the second after /new
		typed := tools.ToolContext{Channel: "cli", ChatID: session}
		if res := sendMessage(t, tool, typed, map[string]any{"message": "x"}); !res.IsError {
			t.Errorf("a typed turn in session %q wrote to its own terminal", session)
		}
	}
}
