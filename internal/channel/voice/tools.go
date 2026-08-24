package voice

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

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
		// A rename moves a person's conversation to a new session key, so it
		// belongs in the log beside the turns on either side of it.
		slog.Info("speaker renamed", "from", name, "to", newName)
		return tools.Textf("%s is now %s.", name, newName)
	case "forget":
		if name == "" {
			return tools.Errorf("name is required")
		}
		if err := store.forget(name); err != nil {
			return tools.Errorf("%v", err)
		}
		slog.Info("speaker forgotten", "speaker", name)
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

// roomTool is how the conversation itself corrects the room. Sound can only
// report people who make it: somebody who walks in and listens is invisible
// to every acoustic signal, and somebody who leaves is announced by nothing
// at all. Both of those are routinely said out loud — "Roxana just got here",
// "she's gone, it's just me" — so the model is the sensor for them, and this
// is where what it heard becomes state.
type roomTool struct {
	voice *Voice
}

func (t *roomTool) Name() string { return "room" }

func (t *roomTool) Description() string {
	return "Report who else is within earshot, so replies and memory stay scoped to the room. " +
		"Use action=company when someone joins or you learn a person is present who has not spoken " +
		"(names optional); action=alone when the user says everyone has left; action=left with a name " +
		"for one person leaving; action=status to check. While company is present the conversation " +
		"runs in a shared session and only shared memory is recalled, so nothing said in private is " +
		"repeated out loud. Call it as soon as you learn the room changed, not at the end of the turn."
}

func (t *roomTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"company", "alone", "left", "status"},
				"description": "What changed about who is in the room",
			},
			"names": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Who joined, for company; who left, for left",
			},
		},
		"required": []string{"action"},
	}
}

func (t *roomTool) Execute(_ context.Context, args map[string]any) *tools.Result {
	r := t.voice.room
	if r == nil {
		return tools.Errorf("room isolation is off (channels.voice.room_isolation)")
	}
	now := time.Now()
	switch tools.StringArg(args, "action") {
	case "company":
		r.declare(true, stringsArg(args, "names"), now)
	case "alone":
		r.declare(false, nil, now)
	case "left":
		names := stringsArg(args, "names")
		if len(names) == 0 {
			return tools.Errorf("names is required for action=left; use action=alone if everyone has gone")
		}
		for _, n := range names {
			r.forget(n)
		}
	case "status":
	default:
		return tools.Errorf("unknown action; use company, alone, left, or status")
	}
	st := r.snapshot(now)
	if !st.Shared {
		return tools.Textf("The room is private: nobody but the user is within earshot.")
	}
	return tools.Textf("The room is shared with %s. Replies are audible to them, and only shared "+
		"memory is being recalled. Nothing announces a departure to the microphone, so if the "+
		"user says nobody else is here, they are right and this state is stale: correct it with "+
		"action=alone, or action=left naming who went.", strings.Join(st.Present, ", "))
}

// stringsArg reads a JSON array of strings, which arrives as []any.
func stringsArg(args map[string]any, key string) []string {
	raw, _ := args[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}
