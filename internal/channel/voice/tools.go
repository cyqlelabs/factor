package voice

import (
	"context"
	"fmt"
	"strings"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/tools"
)

// speakersTool manages the voice profiles behind speaker identification. It
// only exists when speaker_id is on, and it is how profiles get their real
// names: enrollment names a voice "speaker-2", and the person telling the
// agent who they are is what turns that into "Roxana".
type speakersTool struct {
	voice *Voice
}

func (t *speakersTool) Name() string { return "voice_speakers" }

func (t *speakersTool) Description() string {
	return "Manage the voices this machine recognizes. Use action=list to see the enrolled speakers " +
		"and who spoke last; action=rename when someone tells you who they are (name defaults to " +
		"the voice that spoke last); action=forget to delete a profile. Renaming a speaker renames " +
		"their conversation too, from their next utterance on."
}

func (t *speakersTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"list", "rename", "forget"},
				"description": "What to do with the voice profiles",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "The profile to act on; for rename it defaults to the voice that spoke last",
			},
			"new_name": map[string]any{
				"type":        "string",
				"description": "The person's name, for rename",
			},
		},
		"required": []string{"action"},
	}
}

func (t *speakersTool) Execute(_ context.Context, args map[string]any) *tools.Result {
	store := t.voice.speakers
	if store == nil {
		return tools.Errorf("speaker identification is off (channels.voice.speaker_id)")
	}
	name := strings.TrimSpace(tools.StringArg(args, "name"))
	switch tools.StringArg(args, "action") {
	case "list":
		profiles := store.list()
		if len(profiles) == 0 {
			return tools.Textf("No voices are enrolled yet.")
		}
		var b strings.Builder
		for _, p := range profiles {
			b.WriteString("- " + p.Name)
			if p.Primary {
				b.WriteString(" (primary)")
			}
			fmt.Fprintf(&b, " — %d utterances\n", p.Utterances)
		}
		if last := t.voice.lastSpeakerName(); last != "" {
			b.WriteString("Last heard: " + last)
		}
		return tools.Textf("%s", strings.TrimSpace(b.String()))
	case "rename":
		newName := strings.TrimSpace(tools.StringArg(args, "new_name"))
		if newName == "" {
			return tools.Errorf("new_name is required")
		}
		if name == "" {
			if name = t.voice.lastSpeakerName(); name == "" {
				return tools.Errorf("no voice has spoken recently — say which profile to rename")
			}
		}
		if err := store.rename(name, newName); err != nil {
			return tools.Errorf("%v", err)
		}
		t.voice.renameSticky(name, newName)
		return tools.Textf("%s is now %s.", name, newName)
	case "forget":
		if name == "" {
			return tools.Errorf("name is required")
		}
		if err := store.forget(name); err != nil {
			return tools.Errorf("%v", err)
		}
		return tools.Textf("Forgot the voice of %s.", name)
	default:
		return tools.Errorf("unknown action (want list, rename, or forget)")
	}
}

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
