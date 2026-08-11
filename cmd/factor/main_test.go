package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
)

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()

	runErr := fn()
	os.Stdout = original
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out, runErr
}

func testHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	return home
}

func TestRunInitCreatesConfigAndWorkspace(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, "config.json")

	// non-interactive, no-install: the scriptable path, nothing to prompt and
	// no smrti download.
	out, err := captureStdout(t, func() error { return runInit(path, true, true) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "workspace", "AGENT.md")); err != nil {
		t.Errorf("workspace not created: %v", err)
	}
	if !strings.Contains(out, path) || !strings.Contains(out, "workspace") {
		t.Errorf("output did not report what was created: %q", out)
	}
	// no api key configured yet, so the warning must appear
	if !strings.Contains(out, "no API key") {
		t.Errorf("missing the API key warning: %q", out)
	}

	// a second run over the existing config is harmless
	if _, err := captureStdout(t, func() error { return runInit(path, true, true) }); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

func TestRunInitRejectsBadConfig(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return runInit(path, true, true) }); err == nil {
		t.Error("runInit accepted a malformed config")
	}
}

func TestRunStatusReportsUnreachableMemoryAndStoppedGateway(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, "config.json")
	cfg := config.Default()
	cfg.Memory.Host, cfg.Memory.Port = "127.0.0.1", 1 // nothing listens here
	writeConfig(t, path, cfg)

	out, err := captureStdout(t, func() error { return runStatus(path) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"config:", "workspace:", "provider:", "gateway:   not running", "memory:    unreachable"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestRunStatusReportsRunningGateway(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, "config.json")
	writeConfig(t, path, config.Default())
	// a live pid file (our own pid) means the daemon is up
	if err := os.WriteFile(filepath.Join(home, "factor.pid"),
		[]byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error { return runStatus(path) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "gateway:   running") {
		t.Errorf("status did not report the running gateway:\n%s", out)
	}
}

func TestRunStatusRejectsBadConfig(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return runStatus(path) }); err == nil {
		t.Error("runStatus accepted a malformed config")
	}
}

func TestRunChatOneShotSurfacesProviderFailure(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, "config.json")
	cfg := config.Default()
	cfg.Memory.Mode = "off"
	cfg.Browser.Enabled = false
	cfg.Provider.APIBase = "http://127.0.0.1:1/v1" // dead port: fails fast, no network
	cfg.Provider.MaxRetries = 0
	cfg.Provider.RetryBackoffSecs = 1
	writeConfig(t, path, cfg)

	_, err := captureStdout(t, func() error { return runChat(path, "smoke", "hello") })
	if err == nil {
		t.Error("one-shot chat reported success with an unreachable provider")
	}
}

func TestRunChatRejectsBadConfig(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runChat(path, "main", "hi"); err == nil {
		t.Error("runChat accepted a malformed config")
	}
}

func writeConfig(t *testing.T, path string, cfg *config.Config) {
	t.Helper()
	data, err := marshalConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func marshalConfig(cfg *config.Config) ([]byte, error) {
	return json.MarshalIndent(cfg, "", "  ")
}
