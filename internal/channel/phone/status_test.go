package phone

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestDescribeReportsAHealthyVoiceShell(t *testing.T) {
	shell := newFakeShellAPI(t)
	raw := json.RawMessage(fmt.Sprintf(`{
		"user_number": "+15550001111",
		"phone_number": "+15550002222",
		"twilio_account_sid": "AC1",
		"twilio_auth_token": "t",
		"elevenlabs_api_key": "e",
		"stt_api_key": "d",
		"control_api_base": %q
	}`, shell.URL))

	status := Describe(context.Background(), raw, t.TempDir())
	if !status.Configured || !status.Enabled || !status.Healthy {
		t.Fatalf("status = %+v", status)
	}
	if status.Tier != "tier 1 · cloud audio" || status.Number != "+15550002222" {
		t.Errorf("status = %+v", status)
	}
	if line := status.Line(); !strings.Contains(line, "healthy") || !strings.Contains(line, "+15550002222") {
		t.Errorf("Line() = %q", line)
	}
}

func TestDescribeReportsProblems(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantLine string
	}{
		{"unreadable", `{"user_number":`, "unreadable section"},
		{"invalid", `{"user_number":"nope"}`, "user_number"},
		{
			name: "shell not running",
			raw: `{"user_number":"+15550001111","phone_number":"+15550002222",
				"twilio_account_sid":"AC1","twilio_auth_token":"t",
				"elevenlabs_api_key":"e","stt_api_key":"d",
				"control_api_base":"http://127.0.0.1:1"}`,
			wantLine: "not answering",
		},
		{
			name:     "disabled",
			raw:      `{"enabled":false,"user_number":"+15550001111"}`,
			wantLine: "disabled",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status := Describe(context.Background(), json.RawMessage(c.raw), t.TempDir())
			if !status.Configured {
				t.Error("a present section was reported as absent")
			}
			if status.Healthy {
				t.Error("a broken configuration was reported healthy")
			}
			if !strings.Contains(status.Line(), c.wantLine) {
				t.Errorf("Line() = %q, want it to mention %q", status.Line(), c.wantLine)
			}
		})
	}
}
