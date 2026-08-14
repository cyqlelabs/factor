package phone

import (
	"context"
	"fmt"
	"strings"

	"github.com/cyqlelabs/factor/internal/tools"
)

func errNotE164(s string) error {
	return fmt.Errorf("%q is not a phone number in E.164 form (e.g. +15550001234)", s)
}

func errNotAllowed(to string) error {
	return fmt.Errorf("%s is not on the outbound allowlist; it has to be added to channels.phone.allow_call_to first", to)
}

// These two tools are how the agent reaches out on its own: a text for
// anything that can wait to be read, a call for anything that cannot. Both are
// bounded by the outbound allowlist, which defaults to the owner alone — a
// model that can be talked into dialling arbitrary numbers is a toll-fraud
// engine, so the guard lives here rather than in the prompt.

type smsTool struct{ phone *Phone }

func (t *smsTool) Name() string { return "phone_sms" }
func (t *smsTool) Description() string {
	return "Send an SMS text message. Use it to reach the user when they are not in a chat with you — a reminder, a short result, a heads-up. Keep it short: texts are charged per segment. Defaults to the owner's number; any other number must be on the outbound allowlist."
}
func (t *smsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message": map[string]any{"type": "string", "description": "The text to send, plain text, no markdown"},
			"to":      map[string]any{"type": "string", "description": "Destination in E.164 (e.g. +15550001234); defaults to the owner's number"},
		},
		"required": []any{"message"},
	}
}

func (t *smsTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	message := strings.TrimSpace(tools.StringArg(args, "message"))
	if message == "" {
		return tools.Errorf("message is empty; there is nothing to send")
	}
	to, err := t.phone.target(tools.StringArg(args, "to"))
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if err := t.phone.sendSMS(ctx, to, message); err != nil {
		return tools.Errorf("could not send the text: %v", t.phone.redact(err))
	}
	return tools.Textf("Text sent to %s.", to)
}

type callTool struct{ phone *Phone }

func (t *callTool) Name() string { return "phone_call" }
func (t *callTool) Description() string {
	return "Call someone on the phone and talk with them out loud. Returns as soon as the call is dialling — you are notified automatically when it ends, with how it went and what was said, so tell the user you are calling and stop waiting. Defaults to the owner's number; any other number must be on the outbound allowlist."
}
func (t *callTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"goal":          map[string]any{"type": "string", "description": "Why you are calling, in one sentence — you are told this again at the start of the call"},
			"first_message": map[string]any{"type": "string", "description": "The exact words to open with when they pick up; omit to open in your own words"},
			"to":            map[string]any{"type": "string", "description": "Destination in E.164 (e.g. +15550001234); defaults to the owner's number"},
		},
		"required": []any{"goal"},
	}
}

func (t *callTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	goal := strings.TrimSpace(tools.StringArg(args, "goal"))
	if goal == "" {
		return tools.Errorf("goal is empty; say why you are calling")
	}
	to, err := t.phone.target(tools.StringArg(args, "to"))
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if down := t.phone.Down(); down != "" {
		return tools.Errorf("the voice shell cannot place calls right now: %s", down)
	}

	tc := tools.ToolContextFrom(ctx)
	callID, err := t.phone.placeCall(ctx, to, goal, strings.TrimSpace(tools.StringArg(args, "first_message")),
		origin{Channel: tc.Channel, ChatID: tc.ChatID})
	if err != nil {
		return tools.Errorf("could not place the call: %v", t.phone.redact(err))
	}
	return tools.Textf("Dialling %s now (call %s). You will be told how it went when it ends; reply to the user now.", to, callID)
}

// target resolves and authorizes a destination, defaulting to the owner.
func (p *Phone) target(requested string) (string, error) {
	to := normalizeNumber(requested)
	if to == "" {
		to = p.cfg.UserNumber
	}
	if !validNumber(to) {
		return "", errNotE164(requested)
	}
	if !p.cfg.outboundAllowed(to) {
		return "", errNotAllowed(to)
	}
	return to, nil
}
