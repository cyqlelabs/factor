package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

// stubProvider answers one chat completion so the one-shot path can run to
// completion without a real model.
func stubProvider(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": reply},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunChatOneShotPrintsTheReply(t *testing.T) {
	home := testHome(t)
	srv := stubProvider(t, "the one-shot answer")

	cfg := config.Default()
	cfg.Memory.Mode = "off"
	cfg.Browser.Enabled = false
	cfg.Provider.APIBase = srv.URL
	cfg.Provider.APIKey = "k"
	path := filepath.Join(home, "config.json")
	writeConfig(t, path, cfg)

	out, err := captureStdout(t, func() error { return runChat(path, "oneshot", "hello there") })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "the one-shot answer") {
		t.Errorf("reply not printed: %q", out)
	}

	// the exchange was persisted to the named session
	sessions, err := os.ReadDir(filepath.Join(home, "workspace", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range sessions {
		if strings.Contains(e.Name(), "oneshot") {
			found = true
		}
	}
	if !found {
		t.Errorf("session not persisted; files = %v", sessions)
	}
}

// The gateway subcommand is the daemon entry point: it must boot from the
// CLI and shut down on a signal.
func TestCLIGatewayCommandBootsAndStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has no cross-process stop signal; its clean shutdown goes through the tray, covered by internal/gateway")
	}
	home := t.TempDir()
	port := freeCLIPort(t)

	cfg := config.Default()
	cfg.Agent.Workspace = filepath.Join(home, "workspace")
	cfg.Memory.Mode = "off"
	cfg.Browser.Enabled = false
	cfg.Heartbeat.Enabled = false
	cfg.Gateway.Host, cfg.Gateway.Port = "127.0.0.1", port
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "FACTOR_TEST_MAIN_ARGS=gateway", "FACTOR_HOME="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(30 * time.Second)
	up := false
	for time.Now().Before(deadline) {
		if resp, err := client.Get(healthURL); err == nil {
			_ = resp.Body.Close()
			up = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !up {
		t.Fatal("factor gateway never served /health")
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("gateway exited with %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("gateway did not exit after SIGTERM")
	}
	if _, err := os.Stat(filepath.Join(home, "factor.pid")); !os.IsNotExist(err) {
		t.Error("pid file survived shutdown")
	}
}

func freeCLIPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func TestCLIHelpFlagPrintsUsage(t *testing.T) {
	out, code := runCLI(t, t.TempDir(), "-h")
	if !strings.Contains(out, "factor — desktop AI agent") {
		t.Errorf("usage output = %q", out)
	}
	if code != 0 {
		t.Errorf("-h exit code = %d", code)
	}
}
