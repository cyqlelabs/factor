package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
)

func rawSection(t *testing.T, section map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(section)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDescribeNeverFails(t *testing.T) {
	env := scriptedEnv("linux", "parec", "paplay")

	broken := Describe(context.Background(), json.RawMessage(`{"enabled": "yes"}`), env)
	if !broken.Configured || broken.Problem == "" {
		t.Errorf("an unreadable section reported %+v", broken)
	}

	disabled := Describe(context.Background(),
		rawSection(t, map[string]any{"enabled": false, "stt_api_key": "k", "elevenlabs_api_key": "k"}), env)
	if disabled.Enabled {
		t.Error("a disabled section reported enabled")
	}
	if disabled.Line() != "disabled in the config" {
		t.Errorf("Line() = %q", disabled.Line())
	}

	invalid := Describe(context.Background(), rawSection(t, map[string]any{"activation": "sometimes"}), env)
	if invalid.Problem == "" || !strings.Contains(invalid.Line(), "unknown activation") {
		t.Errorf("an invalid section reported %+v", invalid)
	}
}

func TestDescribeReportsMissingHelpersAndSilence(t *testing.T) {
	deaf := scriptedEnv("linux")
	status := Describe(context.Background(),
		rawSection(t, map[string]any{"stt_api_key": "k", "elevenlabs_api_key": "k", "control_port": freePort(t)}), deaf)
	if len(status.MissingHelpers) != 2 || !strings.Contains(status.Line(), "missing") {
		t.Errorf("status = %+v, line = %q", status, status.Line())
	}

	// Helpers present, but nothing running on the control port.
	quiet := Describe(context.Background(),
		rawSection(t, map[string]any{"stt_api_key": "k", "elevenlabs_api_key": "k", "control_port": freePort(t)}),
		scriptedEnv("linux", "parec", "paplay"))
	if quiet.Listening {
		t.Error("nothing is running, yet the status says listening")
	}
	if !strings.Contains(quiet.Line(), "not listening") {
		t.Errorf("Line() = %q", quiet.Line())
	}
}

func TestDescribeSeesARunningChannel(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.start()
	status := Describe(context.Background(),
		rawSection(t, map[string]any{
			"stt":          map[string]any{"provider": "local-openai", "base_url": h.api.URL},
			"tts":          map[string]any{"provider": "local-openai", "base_url": h.api.URL},
			"control_port": h.v.cfg.ControlPort,
		}), scriptedEnv("linux", "parec", "paplay"))
	if !status.Listening {
		t.Errorf("a listening channel was not seen: %+v", status)
	}
	if !strings.Contains(status.Line(), "listening") {
		t.Errorf("Line() = %q", status.Line())
	}
}

func TestTalkArmsARunningChannel(t *testing.T) {
	h := newVoiceHarness(t, func(c *Config) { c.Activation = "push-to-talk" })
	h.start()

	raw := rawSection(t, map[string]any{
		"stt":          map[string]any{"provider": "local-openai", "base_url": h.api.URL},
		"tts":          map[string]any{"provider": "local-openai", "base_url": h.api.URL},
		"control_port": h.v.cfg.ControlPort,
	})
	if err := Talk(context.Background(), raw); err != nil {
		t.Fatalf("Talk: %v", err)
	}
	h.v.mu.Lock()
	armed := time.Now().Before(h.v.pttUntil)
	h.v.mu.Unlock()
	if !armed {
		t.Error("Talk did not arm push-to-talk")
	}
}

func TestTalkExplainsWhenNothingIsListening(t *testing.T) {
	raw := rawSection(t, map[string]any{"stt_api_key": "k", "elevenlabs_api_key": "k", "control_port": freePort(t)})
	err := Talk(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "not listening") {
		t.Errorf("Talk = %v", err)
	}
	if err := Talk(context.Background(), json.RawMessage(`{"enabled": "yes"}`)); err == nil {
		t.Error("Talk accepted an unreadable section")
	}
}

func TestVoiceWriteDeliversToTheWrittenChat(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	b := bus.New()
	v, err := New(validConfig(), b)
	if err != nil {
		t.Fatal(err)
	}
	tool := v.Toolset()[0]
	if tool.Name() != "voice_write" {
		t.Fatalf("tool = %q", tool.Name())
	}
	if tool.Parameters()["type"] != "object" || tool.Description() == "" {
		t.Error("the tool schema is incomplete")
	}

	// Unbound: there is nowhere to write to.
	if res := tool.Execute(context.Background(), map[string]any{"message": "hi"}); !res.IsError {
		t.Error("an unbound channel accepted a written reply")
	}

	v.BindLastExternal(func() (string, string, bool) { return "telegram", "42", true })
	if res := tool.Execute(context.Background(), map[string]any{"message": ""}); !res.IsError {
		t.Error("an empty message was accepted")
	}
	res := tool.Execute(context.Background(), map[string]any{"message": "here are the details"})
	if res.IsError {
		t.Fatalf("Execute: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "telegram") {
		t.Errorf("the result does not say where it went: %q", res.ForLLM)
	}
	select {
	case out := <-b.Outbound():
		if out.Channel != "telegram" || out.ChatID != "42" || out.Content != "here are the details" {
			t.Errorf("outbound = %+v", out)
		}
	default:
		t.Fatal("nothing was published")
	}

	// The last external chat being the voice channel itself is not writing.
	v.BindLastExternal(func() (string, string, bool) { return "voice", "local", true })
	if res := tool.Execute(context.Background(), map[string]any{"message": "hi"}); !res.IsError {
		t.Error("a written reply was routed back to the speakers")
	}
	v.BindLastExternal(func() (string, string, bool) { return "", "", false })
	if res := tool.Execute(context.Background(), map[string]any{"message": "hi"}); !res.IsError {
		t.Error("a written reply was accepted with no chat to take it")
	}
}

func TestRedactKeepsSecretsOutOfErrors(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	v, err := New(validConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	leaky := fmt.Errorf("401 from api: key dg-key rejected, token %s", v.token)
	got := v.redact(leaky).Error()
	if strings.Contains(got, "dg-key") || strings.Contains(got, v.token) {
		t.Errorf("secrets survived redaction: %q", got)
	}
	if v.redact(nil) != nil {
		t.Error("redact(nil) != nil")
	}
}
