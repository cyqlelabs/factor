package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/tools"
)

// MessageToolName is the tool that writes to the user from a turn whose own
// reply does not reach them in writing.
const MessageToolName = "message_user"

// MessageTool is message_user for the gateway: it writes to the chats its
// connectors run. A task another agent handed this machine answers that
// agent, and a spoken turn answers out loud, so neither could otherwise put
// a line in front of the user to read.
func (l *Loop) MessageTool() tools.Tool { return &messageTool{loop: l} }

// TerminalMessageTool is message_user for a chat session, whose one place to
// write is its own terminal: the session prints every outbound message
// addressed to it.
func (l *Loop) TerminalMessageTool(session string) tools.Tool {
	return &messageTool{loop: l, terminal: session}
}

type messageTool struct {
	loop     *Loop
	terminal string
}

func (t *messageTool) Name() string { return MessageToolName }

func (t *messageTool) Description() string {
	return "Send the user a written message: in this terminal when Factor runs as a chat session, otherwise " +
		"in a chat they read — the one they wrote in last, or the channel named (e.g. telegram). Use it when " +
		"this turn's own reply does not reach them in writing: a task another agent handed you, or a spoken " +
		"conversation where they asked for it in writing or the content is painful to listen to — code, " +
		"lists, links, anything long. Never use it to answer a written chat; just reply there."
}

func (t *messageTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message": map[string]any{
				"type":        "string",
				"description": "The text to deliver, formatted for reading",
			},
			"channel": map[string]any{
				"type":        "string",
				"description": "Which chat channel to write in, e.g. telegram; omit for the one the user wrote in last",
			},
		},
		"required": []string{"message"},
	}
}

func (t *messageTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	message := strings.TrimSpace(tools.StringArg(args, "message"))
	if message == "" {
		return tools.Errorf("message is required")
	}
	to, err := t.target(strings.ToLower(strings.TrimSpace(tools.StringArg(args, "channel"))))
	if err != nil {
		return tools.Errorf("%v", err)
	}
	tc := tools.ToolContextFrom(ctx)
	// A chat session has one terminal, whichever session /new moved it to.
	if to.Channel == tc.Channel && (to.ChatID == tc.ChatID || to.Channel == "cli") {
		return tools.Errorf("that is the chat this conversation is in — say it in your reply")
	}
	if tc.Audience == "" && t.loop.audienceAt(to.Channel, to.ChatID) == tools.AudienceShared {
		return tools.Errorf("somebody else can read %s right now and this turn may hold private details, so "+
			"nothing was sent", to.Channel)
	}
	if !t.loop.bus.PublishOutbound(bus.OutboundMessage{Channel: to.Channel, ChatID: to.ChatID, Content: message}) {
		return tools.Errorf("the outbound queue is full — try again in a moment")
	}
	if t.spoken(tc) {
		return tools.Textf("Delivered in writing via %s. Keep the spoken reply to one short sentence saying "+
			"where the text went.", to.Channel)
	}
	return tools.Textf("Delivered in writing via %s.", to.Channel)
}

// target is the chat a message goes to: the terminal of a chat session, or
// the newest chat the user used on a running channel that is written.
func (t *messageTool) target(channel string) (chatAddr, error) {
	if t.terminal != "" {
		if channel != "" && channel != "cli" && channel != "terminal" {
			return chatAddr{}, fmt.Errorf("only this terminal can be written to from a chat session")
		}
		return chatAddr{Channel: "cli", ChatID: t.terminal}, nil
	}
	if channel == "cli" || channel == "terminal" {
		return chatAddr{}, fmt.Errorf("no terminal is reachable from the daemon")
	}
	l := t.loop
	l.lastMu.Lock()
	chats := l.lastChats
	l.lastMu.Unlock()
	for _, c := range chats {
		if (channel == "" || c.Channel == channel) && t.written(c.Channel) {
			return c, nil
		}
	}
	return chatAddr{}, t.noChat(channel)
}

// written reports whether a channel is a chat running here that the user
// reads: a connector that runs its own turns speaks or dials instead.
func (t *messageTool) written(channel string) bool {
	return t.loop.Reachable(channel) && t.loop.channelConverses(channel)
}

// noChat says why no chat on channel (any channel, when blank) can be
// written to.
func (t *messageTool) noChat(channel string) error {
	switch {
	case channel == "":
		return fmt.Errorf("no written chat is reachable: the user has not written to Factor on any chat " +
			"channel running here")
	case !t.loop.Reachable(channel):
		return fmt.Errorf("%s is not running here", channel)
	case !t.loop.channelConverses(channel):
		return fmt.Errorf("%s is not a written chat", channel)
	}
	return fmt.Errorf("the user has not written to Factor on %s yet, so there is no chat to write to", channel)
}

// spoken reports whether the calling turn's own reply is heard rather than
// read: a chat session only ever lends this tool to its spoken turns, and
// under the daemon a running channel that is not a written chat speaks.
func (t *messageTool) spoken(tc tools.ToolContext) bool {
	if t.terminal != "" {
		return true
	}
	outlet := tc.Outlet
	if outlet == "" {
		outlet = tc.Channel
	}
	return t.loop.Reachable(outlet) && !t.loop.channelConverses(outlet)
}
