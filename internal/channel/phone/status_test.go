package phone

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
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

// A local tier that has not been installed yet still takes calls, on the cloud
// tier, so status has to say which of the two is happening.
func TestDescribeReportsTheLocalSpeechStack(t *testing.T) {
	shell := newFakeShellAPI(t)
	raw := json.RawMessage(fmt.Sprintf(`{
		"user_number": "+15550001111",
		"phone_number": "+15550002222",
		"twilio_account_sid": "AC1",
		"twilio_auth_token": "t",
		"language": "es",
		"stt": {"provider": "local-openai"},
		"tts": {"provider": "local-openai"},
		"speech_server": {"whisper_model": "small", "whisper_device": "cuda",
			"piper_voice": "es_ES-davefx-medium"},
		"control_api_base": %q
	}`, shell.URL))

	status := Describe(context.Background(), raw, t.TempDir())
	if status.Tier != "tier 4 · fully local audio" {
		t.Errorf("tier = %q", status.Tier)
	}
	if status.SpeechInstalled {
		t.Error("an empty home should not report the speech engines as installed")
	}
	if !strings.Contains(status.Line(), "speech not installed yet") {
		t.Errorf("Line() = %q, want it to say the speech stack is missing", status.Line())
	}
	if !strings.Contains(status.Speech, "es_ES-davefx-medium") {
		t.Errorf("Speech = %q, want the installed voice named", status.Speech)
	}
}

// A speech server that is answering is working, whatever this process can find
// on disk — reporting "not installed yet" next to a healthy server is a
// contradiction the user has to debug.
func TestDescribeTrustsAHealthySpeechServerOverTheDisk(t *testing.T) {
	speech := newFakeShellAPI(t)
	port := portOf(t, speech.URL)
	shell := newFakeShellAPI(t)
	raw := json.RawMessage(fmt.Sprintf(`{
		"user_number": "+15550001111", "phone_number": "+15550002222",
		"twilio_account_sid": "AC1", "twilio_auth_token": "t",
		"stt": {"provider": "local-openai"}, "tts": {"provider": "local-openai"},
		"speech_server": {"port": %d, "whisper_model": "base", "piper_voice": "en_US-lessac-medium"},
		"control_api_base": %q
	}`, port, shell.URL))

	status := Describe(context.Background(), raw, t.TempDir())
	if !status.SpeechHealthy || !status.SpeechInstalled {
		t.Errorf("healthy=%v installed=%v, want a live server reported as both",
			status.SpeechHealthy, status.SpeechInstalled)
	}
	if strings.Contains(status.Line(), "not installed") {
		t.Errorf("Line() = %q, want no contradiction with a healthy server", status.Line())
	}
}

// An interpreter the user named is installed, even with no private virtualenv.
func TestDescribeCountsAConfiguredInterpreterAsInstalled(t *testing.T) {
	shell := newFakeShellAPI(t)
	raw := json.RawMessage(fmt.Sprintf(`{
		"user_number": "+15550001111", "phone_number": "+15550002222",
		"twilio_account_sid": "AC1", "twilio_auth_token": "t",
		"elevenlabs_api_key": "e",
		"stt": {"provider": "local-openai"},
		"speech_server": {"command": "/usr/bin/python3", "port": 1, "whisper_model": "base"},
		"control_api_base": %q
	}`, shell.URL))

	status := Describe(context.Background(), raw, t.TempDir())
	if !status.SpeechInstalled {
		t.Error("an interpreter named in the config should count as installed")
	}
	if status.SpeechHealthy {
		t.Error("nothing is listening on that port; it should not be healthy")
	}
}

// portOf pulls the port out of a test server URL.
func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// A cloud tier has no local stack, and must not grow a line about one.
func TestDescribeSaysNothingAboutSpeechOnTheCloudTier(t *testing.T) {
	shell := newFakeShellAPI(t)
	raw := json.RawMessage(fmt.Sprintf(`{
		"user_number": "+15550001111", "phone_number": "+15550002222",
		"twilio_account_sid": "AC1", "twilio_auth_token": "t",
		"elevenlabs_api_key": "e", "stt_api_key": "d",
		"control_api_base": %q
	}`, shell.URL))

	status := Describe(context.Background(), raw, t.TempDir())
	if status.Speech != "" {
		t.Errorf("Speech = %q, want nothing for a cloud tier", status.Speech)
	}
	if strings.Contains(status.Line(), "speech") {
		t.Errorf("Line() = %q, want no speech-server line", status.Line())
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
