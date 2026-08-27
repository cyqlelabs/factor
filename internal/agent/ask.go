package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/tools"
)

// Asker returns the asker the gateway hands ask_user: a turn that arrived off
// the bus from a conversational external channel asks in that chat — the
// question goes out as a message and the user's next inbound message there is
// claimed as the answer instead of steering — and every other turn falls back
// (under the daemon, the desktop dialog). The routing is per question, read
// from the tool context of the turn that asks, which is what a Telegram
// conversation needs: its question must land in the chat the user is actually
// looking at, not on a screen nobody is watching.
func (l *Loop) Asker(fallback tools.Asker) tools.Asker {
	return &turnAsker{loop: l, fallback: fallback}
}

type turnAsker struct {
	loop     *Loop
	fallback tools.Asker
}

func (a *turnAsker) Ask(ctx context.Context, q tools.Question) (tools.Answer, error) {
	if answer, handled, err := a.loop.askViaChannel(ctx, q); handled {
		return answer, err
	}
	if a.fallback == nil {
		return tools.Answer{}, tools.ErrAskUnavailable
	}
	return a.fallback.Ask(ctx, q)
}

// askViaChannel puts q to the user on the chat the running turn arrived from
// and waits for the next inbound message of that session, which claim hands
// over as the answer. handled is false when this turn is not one a chat can
// carry the question for — not bus-driven, not an external conversational
// channel, or a question already standing — leaving the caller to fall back.
func (l *Loop) askViaChannel(ctx context.Context, q tools.Question) (answer tools.Answer, handled bool, err error) {
	tc := tools.ToolContextFrom(ctx)
	if !bus.External(tc.Channel) || !l.channelConverses(tc.Channel) {
		return tools.Answer{}, false, nil
	}
	answers := make(chan bus.InboundMessage, 1)
	l.mu.Lock()
	t, running := l.active[tc.SessionKey]
	if !running || !t.busDriven || t.ask != nil {
		l.mu.Unlock()
		return tools.Answer{}, false, nil
	}
	t.ask = answers
	l.mu.Unlock()

	if !l.bus.PublishOutbound(bus.OutboundMessage{Channel: tc.Channel, ChatID: tc.ChatID, Content: askText(q)}) {
		l.withdrawAsk(t, answers)
		return tools.Answer{}, true, fmt.Errorf("could not send the question: the outbound queue is full")
	}

	select {
	case msg := <-answers:
		l.withdrawAsk(t, answers)
		return tools.Answer{Text: tools.MatchOption(msg.Content, q.Options)}, true, nil
	case <-ctx.Done():
		l.withdrawAsk(t, answers)
		return tools.Answer{}, true, ctx.Err()
	}
}

// withdrawAsk takes the question back. Every exit of askViaChannel goes
// through here, because every one of them races claim: a message can land in
// the buffer between this path's last read and the clear — handed over as the
// answer, so neither steered nor persisted. Clearing first (under the same
// lock claim delivers under) stops further deliveries; whatever the buffer
// then still holds is republished as fresh inbound, where it steers into this
// turn — late for the question, but never lost.
func (l *Loop) withdrawAsk(t *turn, answers chan bus.InboundMessage) {
	l.mu.Lock()
	t.ask = nil
	l.mu.Unlock()
	select {
	case msg := <-answers:
		l.bus.PublishInbound(msg)
	default:
	}
}

func (l *Loop) channelConverses(name string) bool {
	l.lastMu.Lock()
	defer l.lastMu.Unlock()
	return l.conversational == nil || l.conversational(name)
}

// askText renders the question as one chat message. Options are numbered so
// the shortest possible reply works; no hint line is added, because the chat
// is held in the user's language and a canned English sentence is not.
func askText(q tools.Question) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(q.Prompt))
	for i, o := range q.Options {
		fmt.Fprintf(&b, "\n%d) %s", i+1, o)
	}
	return b.String()
}
