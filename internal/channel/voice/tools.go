package voice

import (
	"context"
	"strings"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/tools"
)

// writeTool is how a spoken conversation gets a written answer: the user asks
// for something in writing, and the agent hands it to wherever their text
// already flows — the terminal in a CLI session, the last external chat under
// the gateway.
type writeTool struct {
	voice *Voice
}

func (t *writeTool) Name() string { return "voice_write" }

func (t *writeTool) Description() string {
	return "Deliver text to the user in writing instead of speech: in their terminal when Factor " +
		"runs as a chat session, or in their usual chat (e.g. Telegram) when it runs as a daemon. " +
		"Use it when the user asks for a written reply, or when the content would be painful to " +
		"listen to — code, lists, links, anything long. Afterwards keep the spoken reply to one " +
		"short sentence saying where the text went."
}

func (t *writeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message": map[string]any{
				"type":        "string",
				"description": "The text to deliver in writing, formatted for reading",
			},
		},
		"required": []string{"message"},
	}
}

func (t *writeTool) Execute(_ context.Context, args map[string]any) *tools.Result {
	message := strings.TrimSpace(tools.StringArg(args, "message"))
	if message == "" {
		return tools.Errorf("message is required")
	}
	target := t.voice.lastExternal
	if target == nil {
		return tools.Errorf("no written chat is reachable from here — speak the answer instead")
	}
	chatChannel, chatID, ok := target()
	if !ok || chatChannel == "voice" {
		return tools.Errorf("no written chat is reachable right now — speak the answer instead")
	}
	if !t.voice.publish(bus.OutboundMessage{Channel: chatChannel, ChatID: chatID, Content: message}) {
		return tools.Errorf("the outbound queue is full — try again in a moment")
	}
	return tools.Textf("Delivered in writing via %s. Keep the spoken reply to one short sentence.", chatChannel)
}
