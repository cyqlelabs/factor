package phone

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/tools"
)

// The registry gates every call on the schema, so a tool whose schema and
// behaviour disagree fails in front of the model instead of in the code. This
// is the same contract the built-in arsenal is held to.
func TestPhoneToolContract(t *testing.T) {
	p, _, _ := newTestPhone(t, nil)
	set := p.Toolset()
	if len(set) != 2 {
		t.Fatalf("Toolset() returned %d tools, want phone_sms and phone_call", len(set))
	}

	names := map[string]bool{}
	for _, tool := range set {
		t.Run(tool.Name(), func(t *testing.T) {
			names[tool.Name()] = true
			if !strings.HasPrefix(tool.Name(), "phone_") {
				t.Errorf("name %q is not namespaced to the channel", tool.Name())
			}
			if strings.TrimSpace(tool.Description()) == "" {
				t.Error("Description() is empty; the model has nothing to route on")
			}

			schema := tool.Parameters()
			if schema["type"] != "object" {
				t.Errorf(`schema type = %v, want "object"`, schema["type"])
			}
			props, ok := schema["properties"].(map[string]any)
			if !ok {
				t.Fatalf("properties = %T, want map[string]any", schema["properties"])
			}
			for key, spec := range props {
				prop, ok := spec.(map[string]any)
				if !ok {
					t.Fatalf("property %q = %T", key, spec)
				}
				if desc, _ := prop["description"].(string); strings.TrimSpace(desc) == "" {
					t.Errorf("property %q has no description", key)
				}
			}
			if _, err := json.Marshal(schema); err != nil {
				t.Errorf("schema is not serializable: %v", err)
			}

			required, _ := schema["required"].([]any)
			for _, r := range required {
				key, _ := r.(string)
				if _, declared := props[key]; !declared {
					t.Errorf("required key %q is not declared in properties", key)
				}
			}
			// The schema must agree with the validator that actually gates calls.
			err := tools.ValidateArgs(schema, map[string]any{})
			switch {
			case len(required) > 0 && err == nil:
				t.Error("schema declares required keys but ValidateArgs accepts empty args")
			case len(required) == 0 && err != nil:
				t.Errorf("schema declares no required keys but ValidateArgs rejects empty args: %v", err)
			}
		})
	}
	for _, want := range []string{"phone_sms", "phone_call"} {
		if !names[want] {
			t.Errorf("%s is missing from the toolset", want)
		}
	}
}

// toolNamed picks one tool out of the connector's set.
func toolNamed(t *testing.T, p *Phone, name string) tools.Tool {
	t.Helper()
	for _, tool := range p.Toolset() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("no %s tool", name)
	return nil
}

func TestPhoneSMSSends(t *testing.T) {
	p, twilio, _ := newTestPhone(t, func(c *Config) { c.AllowCallTo = []string{"+15550003333"} })
	sms := toolNamed(t, p, "phone_sms")

	res := sms.Execute(context.Background(), map[string]any{"message": "on my way"})
	if res.IsError {
		t.Fatalf("phone_sms failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "+15550001111") {
		t.Errorf("result %q does not say who it went to", res.ForLLM)
	}
	sent := twilio.sent()
	if len(sent) != 1 || sent[0].Get("To") != "+15550001111" || sent[0].Get("Body") != "on my way" {
		t.Errorf("carrier received %v", sent)
	}

	// An explicitly allowlisted destination is honoured.
	if res := sms.Execute(context.Background(), map[string]any{"message": "hi", "to": "+1 555 000 3333"}); res.IsError {
		t.Fatalf("an allowlisted destination was refused: %s", res.ForLLM)
	}
	if got := twilio.sent()[1].Get("To"); got != "+15550003333" {
		t.Errorf("To = %q, want the normalized allowlisted number", got)
	}
}

func TestPhoneSMSRefusesWhatItShould(t *testing.T) {
	p, twilio, _ := newTestPhone(t, nil)
	sms := toolNamed(t, p, "phone_sms")

	cases := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{"empty message", map[string]any{"message": "   "}, "nothing to send"},
		{"number outside the allowlist", map[string]any{"message": "hi", "to": "+15559999999"}, "outbound allowlist"},
		{"not a phone number", map[string]any{"message": "hi", "to": "the office"}, "E.164"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := sms.Execute(context.Background(), c.args)
			if !res.IsError {
				t.Fatalf("the tool accepted it: %s", res.ForLLM)
			}
			if !strings.Contains(res.ForLLM, c.wantErr) {
				t.Errorf("error %q does not mention %q", res.ForLLM, c.wantErr)
			}
		})
	}
	if len(twilio.sent()) != 0 {
		t.Error("a refused message still reached the carrier")
	}
}

func TestPhoneSMSNeverLeaksTheCarrierToken(t *testing.T) {
	p, twilio, _ := newTestPhone(t, nil)
	twilio.fail(http.StatusUnauthorized, `{"message":"authenticate with twilio-secret","code":20003}`)

	res := toolNamed(t, p, "phone_sms").Execute(context.Background(), map[string]any{"message": "hi"})
	if !res.IsError {
		t.Fatal("a rejected message was reported as sent")
	}
	if strings.Contains(res.ForLLM, "twilio-secret") {
		t.Errorf("the carrier token reached the model: %q", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "could not send") {
		t.Errorf("result %q does not say what went wrong", res.ForLLM)
	}
}

func TestPhoneCallDialsAndRemembersItsOrigin(t *testing.T) {
	p, _, shell := newTestPhone(t, nil)
	call := toolNamed(t, p, "phone_call")

	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{
		Channel: "telegram", ChatID: "42", SessionKey: "telegram:42",
	})
	res := call.Execute(ctx, map[string]any{
		"goal": "ask whether tuesday works", "first_message": "hi, quick question",
	})
	if res.IsError {
		t.Fatalf("phone_call failed: %s", res.ForLLM)
	}
	for _, want := range []string{"+15550001111", "CA-out-1", "reply to the user now"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("result %q does not mention %q", res.ForLLM, want)
		}
	}

	placed := shell.placed()
	if len(placed) != 1 {
		t.Fatalf("the shell was asked for %d calls, want 1", len(placed))
	}
	if placed[0].Goal != "ask whether tuesday works" || placed[0].FirstMessage != "hi, quick question" {
		t.Errorf("call request = %+v", placed[0])
	}

	p.bridge.mu.Lock()
	info := p.bridge.calls["CA-out-1"]
	p.bridge.mu.Unlock()
	if info == nil || info.Origin.Channel != "telegram" || info.Origin.ChatID != "42" {
		t.Errorf("the outcome would have nowhere to go: %+v", info)
	}
}

func TestPhoneCallRefusesWhatItShould(t *testing.T) {
	p, _, shell := newTestPhone(t, nil)
	call := toolNamed(t, p, "phone_call")

	if res := call.Execute(context.Background(), map[string]any{"goal": "  "}); !res.IsError {
		t.Error("a call with no purpose was accepted")
	}
	res := call.Execute(context.Background(), map[string]any{"goal": "chat", "to": "+15559999999"})
	if !res.IsError || !strings.Contains(res.ForLLM, "outbound allowlist") {
		t.Errorf("dialling a stranger = %+v", res)
	}
	if len(shell.placed()) != 0 {
		t.Error("a refused call still reached the voice shell")
	}
}

func TestPhoneCallReportsAVoiceShellThatCannotDial(t *testing.T) {
	p, _, shell := newTestPhone(t, nil)
	p.shell.setDown("Patter is not installed and auto_install is off")

	res := toolNamed(t, p, "phone_call").Execute(context.Background(), map[string]any{"goal": "chat"})
	if !res.IsError {
		t.Fatal("a call was placed with the voice shell down")
	}
	if !strings.Contains(res.ForLLM, "not installed") {
		t.Errorf("result %q does not explain why", res.ForLLM)
	}
	if len(shell.placed()) != 0 {
		t.Error("the down shell was called anyway")
	}
}

func TestPhoneCallSurfacesACarrierRefusal(t *testing.T) {
	p, _, shell := newTestPhone(t, nil)
	shell.refuse = true

	res := toolNamed(t, p, "phone_call").Execute(context.Background(), map[string]any{"goal": "chat"})
	if !res.IsError {
		t.Fatal("a refused call was reported as dialling")
	}
	if !strings.Contains(res.ForLLM, "carrier is down") {
		t.Errorf("result %q does not carry the shell's reason", res.ForLLM)
	}
}
