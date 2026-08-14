package gateway

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

// fakeTelegram serves getUpdates so the connector has something to poll that
// is not the real Telegram API.
func fakeTelegram(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			time.Sleep(50 * time.Millisecond) // imitate long polling
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The daemon's full wiring: a configured connector starts and is reported,
// the heartbeat loop runs, and shutdown still returns cleanly.
func TestRunStartsConfiguredChannelsAndHeartbeat(t *testing.T) {
	cfg, _ := gatewayConfig(t)
	tg := fakeTelegram(t)

	cfg.Heartbeat.Enabled = true
	cfg.Heartbeat.IntervalMinutes = 60 // long enough never to fire mid-test
	cfg.Channels = map[string]json.RawMessage{
		"telegram": json.RawMessage(fmt.Sprintf(
			`{"token":"test-token","api_base":%q,"allow_from":["1"]}`, tg.URL)),
		"nosuchconnector": json.RawMessage(`{}`),                // unknown: skipped with a log
		"disabled":        json.RawMessage(`{"enabled":false}`), // gated off
	}
	path := filepath.Join(config.Home(), "config.json")
	if err := os.WriteFile(path, mustJSON(t, cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- Run(path) }()

	healthURL := fmt.Sprintf("http://%s/health", net.JoinHostPort(cfg.Gateway.Host, strconv.Itoa(cfg.Gateway.Port)))
	body := waitForHealth(t, healthURL, 20*time.Second)
	if body == nil {
		t.Fatal("gateway never served /health")
	}
	channels, _ := body["channels"].([]any)
	if len(channels) != 1 || channels[0] != "telegram" {
		t.Errorf("channels = %v, want just the telegram connector", body["channels"])
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}
}

// fakeVoiceShell stands in for the Patter sidecar, so the gateway adopts a
// voice shell instead of spawning Python.
func fakeVoiceShell(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The phone connector is the one that runs its own turns and brings its own
// tools; this proves the daemon builds it, starts it, and reports it.
func TestRunStartsThePhoneChannel(t *testing.T) {
	cfg, _ := gatewayConfig(t)
	shell := fakeVoiceShell(t)

	cfg.Channels = map[string]json.RawMessage{
		"phone": json.RawMessage(fmt.Sprintf(`{
			"user_number": "+15550001111",
			"phone_number": "+15550002222",
			"twilio_account_sid": "AC1",
			"twilio_auth_token": "twilio-secret",
			"elevenlabs_api_key": "eleven-secret",
			"stt_api_key": "deepgram-secret",
			"sidecar_port": %d,
			"bridge_port": %d,
			"control_api_base": %q
		}`, freePort(t), freePort(t), shell.URL)),
	}
	path := filepath.Join(config.Home(), "config.json")
	if err := os.WriteFile(path, mustJSON(t, cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- Run(path) }()

	healthURL := fmt.Sprintf("http://%s/health", net.JoinHostPort(cfg.Gateway.Host, strconv.Itoa(cfg.Gateway.Port)))
	body := waitForHealth(t, healthURL, 20*time.Second)
	if body == nil {
		t.Fatal("gateway never served /health")
	}
	channels, _ := body["channels"].([]any)
	if len(channels) != 1 || channels[0] != "phone" {
		t.Errorf("channels = %v, want the phone connector", body["channels"])
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}
}

// A workspace path that cannot be created fails the start before the pid
// file, health server, or any channel comes up.
func TestRunFailsOnUnusableWorkspace(t *testing.T) {
	cfg, _ := gatewayConfig(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Agent.Workspace = filepath.Join(blocker, "workspace")
	path := filepath.Join(config.Home(), "config.json")
	if err := os.WriteFile(path, mustJSON(t, cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Run(path); err == nil {
		t.Error("Run started with an unusable workspace")
	}
	if _, err := os.Stat(pidPath()); !os.IsNotExist(err) {
		t.Error("pid file left behind after a failed start")
	}
}

func TestWritePidFileFailsWhenHomeIsUnusable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FACTOR_HOME", filepath.Join(blocker, "home"))
	if err := writePidFile(); err == nil {
		t.Error("writePidFile succeeded with an unusable FACTOR_HOME")
	}
}
