package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/app"
	"github.com/cyqlelabs/factor/internal/config"
)

func appFor(t *testing.T, cfg *config.Config) *app.App {
	t.Helper()
	a, err := app.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

// fakeVoiceControl answers the voice channel's loopback control API on a port
// of the test's choosing, standing in for a running factor.
func fakeVoiceControl(t *testing.T) (int, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var armed atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ptt", func(w http.ResponseWriter, _ *http.Request) {
		armed.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port, &armed
}

func voiceConfigWith(t *testing.T, home string, port int) string {
	t.Helper()
	cfg := config.Default()
	cfg.Memory.Mode = "off"
	cfg.Channels = map[string]json.RawMessage{
		"voice": json.RawMessage(fmt.Sprintf(
			`{"stt_api_key":"dg","elevenlabs_api_key":"el","control_port":%d}`, port)),
	}
	path := filepath.Join(home, "config.json")
	writeConfig(t, path, cfg)
	return path
}

func TestRunTalkArmsTheRunningChannel(t *testing.T) {
	home := testHome(t)
	port, armed := fakeVoiceControl(t)
	path := voiceConfigWith(t, home, port)

	out, err := captureStdout(t, func() error { return runTalk(path) })
	if err != nil {
		t.Fatalf("runTalk: %v", err)
	}
	if !strings.Contains(out, "listening") {
		t.Errorf("output = %q", out)
	}
	if armed.Load() != 1 {
		t.Errorf("the control endpoint was armed %d times", armed.Load())
	}
}

func TestRunTalkExplainsWhatIsMissing(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, "config.json")
	writeConfig(t, path, config.Default())
	if err := runTalk(path); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("without a voice section: %v", err)
	}

	// Configured but nothing running: the error says what to start.
	quiet := voiceConfigWith(t, home, freeTCPPort(t))
	if err := runTalk(quiet); err == nil || !strings.Contains(err.Error(), "not listening") {
		t.Errorf("with nothing running: %v", err)
	}
}

func TestRunTalkRejectsBadConfig(t *testing.T) {
	path := filepath.Join(testHome(t), "config.json")
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runTalk(path); err == nil {
		t.Error("an unparseable config was accepted")
	}
}

// startVoiceChannel must never take the chat down: no section, a disabled
// section, and a channel that cannot start all report "off" and leave a
// harmless stop func.
func TestStartVoiceChannelDegradesGracefully(t *testing.T) {
	home := testHome(t)
	srv := stubProvider(t, "ok")
	cfg := config.Default()
	cfg.Memory.Mode = "off"
	cfg.Browser.Enabled = false
	cfg.Provider.APIBase = srv.URL
	cfg.Provider.APIKey = "k"
	writeConfig(t, filepath.Join(home, "config.json"), cfg)

	loaded, err := config.Load(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	a := appFor(t, loaded)

	// No section at all.
	stop, on := startVoiceChannel(context.Background(), a, loaded, "main")
	stop()
	if on {
		t.Error("an unconfigured voice channel reported itself on")
	}

	// A disabled section.
	loaded.Channels = map[string]json.RawMessage{"voice": json.RawMessage(`{"enabled":false}`)}
	stop, on = startVoiceChannel(context.Background(), a, loaded, "main")
	stop()
	if on {
		t.Error("a disabled voice channel reported itself on")
	}

	// A valid section whose control port is already taken: Start fails, the
	// chat goes on without it.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	port := taken.Addr().(*net.TCPAddr).Port
	loaded.Channels = map[string]json.RawMessage{"voice": json.RawMessage(fmt.Sprintf(
		`{"stt_api_key":"dg","elevenlabs_api_key":"el","control_port":%d}`, port))}
	stop, on = startVoiceChannel(context.Background(), a, loaded, "main")
	stop()
	if on {
		t.Error("a channel that could not start reported itself on")
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}
