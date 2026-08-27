package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// scriptedAsker stands in for the desktop dialog behind the routing asker.
type scriptedAsker struct {
	answer tools.Answer
	got    tools.Question
	asked  bool
}

func (s *scriptedAsker) Ask(_ context.Context, q tools.Question) (tools.Answer, error) {
	s.got = q
	s.asked = true
	return s.answer, nil
}

func askToolCall(question string, options ...string) func(*provider.Request) (*provider.Response, error) {
	args := map[string]any{"question": question}
	if len(options) > 0 {
		opts := make([]any, 0, len(options))
		for _, o := range options {
			opts = append(opts, o)
		}
		args["options"] = opts
	}
	return toolCall("ask_user", args)
}

func awaitOutbound(t *testing.T, h *harness) bus.OutboundMessage {
	t.Helper()
	select {
	case out := <-h.bus.Outbound():
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived on the outbound bus")
		return bus.OutboundMessage{}
	}
}

func userMessageExactly(req *provider.Request, want string) bool {
	if req == nil {
		return false
	}
	for _, msg := range req.Messages {
		if msg.Role == "user" && msg.Content == want {
			return true
		}
	}
	return false
}

// The incident this exists for: the user chats on Telegram, the gateway pops
// a dialog on a screen nobody is watching, and two minutes later the agent is
// told the user is away. The question has to land in the chat the turn came
// from, and the user's next message there is its answer — not steering.
func TestAskOnAChatTurnAsksInTheChatAndTheNextMessageAnswersIt(t *testing.T) {
	h := newHarness(t, askToolCall("Which game first?", "Petscop", "Anatomy"), final("Anatomy it is"))
	h.registry.Register(tools.NewAskTool(h.loop.Asker(nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.loop.Run(ctx)

	h.bus.PublishInbound(bus.InboundMessage{Channel: "telegram", ChatID: "42", Content: "dale, arranquemos"})

	question := awaitOutbound(t, h)
	if question.Channel != "telegram" || question.ChatID != "42" {
		t.Fatalf("question went to %s:%s, want the asking chat", question.Channel, question.ChatID)
	}
	if !strings.Contains(question.Content, "Which game first?") || !strings.Contains(question.Content, "2) Anatomy") {
		t.Errorf("question = %q, want the prompt with numbered options", question.Content)
	}

	h.bus.PublishInbound(bus.InboundMessage{Channel: "telegram", ChatID: "42", Content: "2"})

	reply := awaitOutbound(t, h)
	if reply.Content != "Anatomy it is" {
		t.Errorf("reply = %q, want the turn's final answer", reply.Content)
	}
	req := lastRequest(h)
	if !containsToolResult(req, "the user answered: Anatomy") {
		t.Error("the answer never reached the model as the tool result")
	}
	if userMessageExactly(req, "2") {
		t.Error("the answer was also steered into the turn as a user message")
	}
}

func containsToolResult(req *provider.Request, want string) bool {
	if req == nil {
		return false
	}
	for _, msg := range req.Messages {
		if msg.Role == "tool" && strings.Contains(msg.Content, want) {
			return true
		}
	}
	return false
}

// A background job finishing while a question stands must not answer it for
// the user: it steers, the question keeps waiting, and the user's own next
// message is the answer.
func TestASystemMessageNeverAnswersAStandingQuestion(t *testing.T) {
	h := newHarness(t, askToolCall("Delete it?", "yes", "no"), final("done"))
	h.registry.Register(tools.NewAskTool(h.loop.Asker(nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.loop.Run(ctx)

	h.bus.PublishInbound(bus.InboundMessage{Channel: "telegram", ChatID: "7", Content: "clean up the folder"})
	awaitOutbound(t, h) // the question is out; the ask is standing

	h.bus.PublishInbound(bus.InboundMessage{Channel: "telegram", ChatID: "7",
		Content: "[system] Background job j1 finished.", System: true})
	h.bus.PublishInbound(bus.InboundMessage{Channel: "telegram", ChatID: "7", Content: "yes"})

	if reply := awaitOutbound(t, h); reply.Content != "done" {
		t.Errorf("reply = %q, want the turn's final answer", reply.Content)
	}
	req := lastRequest(h)
	if !containsToolResult(req, "the user answered: yes") {
		t.Error("the user's word never became the answer")
	}
	if containsToolResult(req, "j1") {
		t.Error("the job report was taken as the user's answer")
	}
	if !userMessageExactly(req, "[system] Background job j1 finished.") {
		t.Error("the job report was lost instead of steering into the turn")
	}
}

// A turn a connector runs itself (voice, phone) or a synchronous caller owns
// (cron, jobs) has no chat the answer could come back through; the question
// goes to the fallback asker — under the daemon, the desktop dialog.
func TestAskOffTheBusFallsBackToTheDialog(t *testing.T) {
	h := newHarness(t, askToolCall("Which?", "a", "b"), final("ok"))
	dialog := &scriptedAsker{answer: tools.Answer{Text: "a"}}
	h.registry.Register(tools.NewAskTool(h.loop.Asker(dialog)))

	if _, err := h.loop.ProcessDirect(context.Background(), "hola", "voice:local"); err != nil {
		t.Fatal(err)
	}
	if !dialog.asked || dialog.got.Prompt != "Which?" {
		t.Errorf("fallback asker saw %+v, want the question", dialog.got)
	}
	select {
	case out := <-h.bus.Outbound():
		t.Errorf("a question left on the bus for a turn no chat is behind: %+v", out)
	default:
	}
}

// A channel the manager says is not a bus-riding chat — the phone, whose
// non-interim sends are texted or dialled — never carries a question, even
// when the turn itself arrived off the bus (an inbound SMS).
func TestAskOnANonConversationalChannelFallsBackToTheDialog(t *testing.T) {
	h := newHarness(t, askToolCall("Which?", "a", "b"), final("ok"))
	dialog := &scriptedAsker{answer: tools.Answer{Text: "b"}}
	h.registry.Register(tools.NewAskTool(h.loop.Asker(dialog)))
	h.loop.SetConversational(func(string) bool { return false })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.loop.Run(ctx)

	h.bus.PublishInbound(bus.InboundMessage{Channel: "phone", ChatID: "+15550001111", Content: "which one?"})
	if reply := awaitOutbound(t, h); reply.Content != "ok" {
		t.Errorf("reply = %q; the first outbound should be the final answer, not a question", reply.Content)
	}
	if !dialog.asked {
		t.Error("the fallback asker was never used")
	}
}

func TestAskWithoutARouteOrAFallbackIsUnavailable(t *testing.T) {
	h := newHarness(t)
	_, err := h.loop.Asker(nil).Ask(context.Background(), tools.Question{Prompt: "Which?"})
	if !errors.Is(err, tools.ErrAskUnavailable) {
		t.Errorf("err = %v, want ErrAskUnavailable", err)
	}
}

// A cancelled wait leaves nothing standing: the question is withdrawn, and a
// message that raced the withdrawal is republished rather than lost.
func TestAChannelAskCancelledMidWaitWithdrawsTheQuestion(t *testing.T) {
	h := newHarness(t)
	key := "telegram:5"
	held, ok, _ := h.loop.claim(key, &bus.InboundMessage{Channel: "telegram", ChatID: "5"}, false, true)
	if !ok {
		t.Fatal("claim failed on an idle session")
	}

	ctx, cancel := context.WithCancel(context.Background())
	ctx = tools.WithToolContext(ctx, tools.ToolContext{Channel: "telegram", ChatID: "5", SessionKey: key})
	type outcome struct {
		answer  tools.Answer
		handled bool
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		a, handled, err := h.loop.askViaChannel(ctx, tools.Question{Prompt: "Which?"})
		done <- outcome{a, handled, err}
	}()
	if out := awaitOutbound(t, h); !strings.Contains(out.Content, "Which?") {
		t.Fatalf("outbound = %q, want the question", out.Content)
	}

	answer := bus.InboundMessage{Channel: "telegram", ChatID: "5", Content: "this one"}
	if _, _, steered := h.loop.claim(key, &answer, true, false); !steered {
		t.Fatal("the answer was not handed to the standing question")
	}
	cancel()

	got := <-done
	if !got.handled {
		t.Fatal("a routed ask reported unhandled")
	}
	// The select races the cancellation against the delivered answer; either
	// way the user's words survive — as the answer, or republished inbound.
	switch {
	case got.err == nil:
		if got.answer.Text != "this one" {
			t.Errorf("answer = %q, want the delivered message", got.answer.Text)
		}
	case errors.Is(got.err, context.Canceled):
		select {
		case msg := <-h.bus.Inbound():
			if msg.Content != "this one" {
				t.Errorf("republished = %q, want the raced answer", msg.Content)
			}
		case <-time.After(time.Second):
			t.Error("the raced answer was lost instead of being republished")
		}
	default:
		t.Errorf("err = %v, want nil or context.Canceled", got.err)
	}

	// The withdrawal is complete: the next message steers instead of answering.
	late := bus.InboundMessage{Channel: "telegram", ChatID: "5", Content: "late"}
	if _, _, steered := h.loop.claim(key, &late, true, false); !steered {
		t.Error("a message after the withdrawal did not steer")
	}
	if leftover := h.loop.release(key, held); len(leftover) != 1 || leftover[0].Content != "late" {
		t.Errorf("steering queue = %+v, want the late message", leftover)
	}
}

func TestAChannelAskReportsAFullOutboundQueue(t *testing.T) {
	h := newHarness(t)
	key := "telegram:6"
	held, ok, _ := h.loop.claim(key, &bus.InboundMessage{Channel: "telegram", ChatID: "6"}, false, true)
	if !ok {
		t.Fatal("claim failed")
	}
	defer h.loop.release(key, held)
	for h.bus.PublishOutbound(bus.OutboundMessage{Channel: "telegram", ChatID: "6", Content: "filler"}) {
	}

	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{Channel: "telegram", ChatID: "6", SessionKey: key})
	_, handled, err := h.loop.askViaChannel(ctx, tools.Question{Prompt: "Which?"})
	if !handled || err == nil || !strings.Contains(err.Error(), "outbound queue is full") {
		t.Errorf("handled = %v, err = %v; want a routed ask failing on the full queue", handled, err)
	}
}

func TestAskTextNumbersItsOptions(t *testing.T) {
	got := askText(tools.Question{Prompt: " Which database? ", Options: []string{"Postgres", "SQLite"}})
	if got != "Which database?\n1) Postgres\n2) SQLite" {
		t.Errorf("askText = %q", got)
	}
	if got := askText(tools.Question{Prompt: "Why?"}); got != "Why?" {
		t.Errorf("open question = %q, want the bare prompt", got)
	}
}
