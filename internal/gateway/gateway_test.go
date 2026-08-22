package gateway

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/upgrade"
)

func freePort(t *testing.T) int {
	t.Helper()
	return freePorts(t, 1)[0]
}

// freePorts hands out n ports that are free and distinct from each other.
//
// Taking them one at a time does not: each listener is closed before the next
// is opened, so the kernel is free to offer the same port again — and the
// phone connector refuses a configuration whose bridge port collides with the
// voice shell's, which turns that coincidence into a connector that never
// starts and a test that fails for a reason nowhere near itself. Holding
// every listener open until all of them are chosen is what makes them
// distinct; they are only closed once the set is complete.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	ports := make([]int, 0, n)
	for len(ports) < n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, l)
		port := l.Addr().(*net.TCPAddr).Port
		// The voice shell's webhook sits one above its control port, so a
		// neighbouring pair collides just as surely as an identical one.
		if slices.ContainsFunc(ports, func(p int) bool { return p == port || p == port+1 || p+1 == port }) {
			continue
		}
		ports = append(ports, port)
	}
	return ports
}

func TestPidFileLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)

	if pid, alive := ReadPidFile(); alive || pid != 0 {
		t.Errorf("no pid file should report (0,false), got (%d,%v)", pid, alive)
	}

	if err := writePidFile(); err != nil {
		t.Fatal(err)
	}
	pid, alive := ReadPidFile()
	if !alive || pid != os.Getpid() {
		t.Errorf("ReadPidFile = (%d,%v), want (%d,true)", pid, alive, os.Getpid())
	}

	// a second gateway refuses to start while the first is alive
	if err := writePidFile(); err == nil {
		t.Error("writePidFile succeeded while another gateway holds the pid file")
	}

	// a stale pid file (dead process) is not treated as running
	if err := os.WriteFile(pidPath(), []byte("999999999"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, alive := ReadPidFile(); alive {
		t.Error("stale pid reported as alive")
	}
	if err := writePidFile(); err != nil {
		t.Errorf("writePidFile refused to take over a stale pid file: %v", err)
	}

	// garbage content is ignored rather than crashing
	for _, bad := range []string{"", "not-a-number", "-5", "0"} {
		if err := os.WriteFile(pidPath(), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if pid, alive := ReadPidFile(); alive || pid != 0 {
			t.Errorf("pid file %q reported (%d,%v)", bad, pid, alive)
		}
	}
}

func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Error("current process reported dead")
	}
	if pidAlive(999999999) {
		t.Error("nonexistent pid reported alive")
	}
}

func TestPidPathUsesFactorHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	if got, want := pidPath(), filepath.Join(home, "factor.pid"); got != want {
		t.Errorf("pidPath() = %q, want %q", got, want)
	}
}

// gatewayConfig builds a hermetic daemon config: no channels, memory off,
// browser off, so nothing external is spawned.
func gatewayConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	cfg := config.Default()
	cfg.Agent.Workspace = filepath.Join(home, "workspace")
	cfg.Memory.Mode = "off"
	cfg.Browser.Enabled = false
	cfg.Heartbeat.Enabled = false
	cfg.Upgrade.Check = false // no reaching out to GitHub from a unit test
	// Nor to OpenRouter for prices: the default provider is a paid model, so
	// every gateway test was really fetching the price catalogue over the
	// internet and writing it into this temp home — on whatever schedule the
	// network answered, which is not one the test controls. A dead local port
	// fails the refresh instantly, as the app package's own config does.
	cfg.Cost.PricesURL = "http://127.0.0.1:1/models"
	cfg.Gateway.Host = "127.0.0.1"
	cfg.Gateway.Port = freePort(t)
	cfg.Provider.APIKey = "test-key"
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, mustJSON(t, cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, path
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func waitForHealth(t *testing.T, url string, within time.Duration) map[string]any {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			defer resp.Body.Close()
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
				return body
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil
}

func TestRunServesHealthAndShutsDownOnSignal(t *testing.T) {
	cfg, path := gatewayConfig(t)
	healthURL := fmt.Sprintf("http://%s/health", net.JoinHostPort(cfg.Gateway.Host, strconv.Itoa(cfg.Gateway.Port)))

	errCh := make(chan error, 1)
	go func() { errCh <- Run(path) }()

	body := waitForHealth(t, healthURL, 20*time.Second)
	if body == nil {
		t.Fatal("gateway never served /health")
	}
	if body["version"] == nil {
		t.Errorf("health body missing version: %v", body)
	}
	if healthy, ok := body["memory_healthy"].(bool); !ok || healthy {
		t.Errorf("memory_healthy = %v, want false in off mode", body["memory_healthy"])
	}
	if _, ok := body["channels"]; !ok {
		t.Errorf("health body missing channels: %v", body)
	}

	// the daemon holds the pid file while running
	if pid, alive := ReadPidFile(); !alive || pid != os.Getpid() {
		t.Errorf("pid file during Run = (%d,%v)", pid, alive)
	}
	// the workspace was materialized
	if _, err := os.Stat(filepath.Join(cfg.Agent.Workspace, "AGENT.md")); err != nil {
		t.Errorf("workspace not created: %v", err)
	}

	// SIGTERM triggers a clean shutdown (the handler is installed by now)
	stopSelf(t)
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}

	// pid file released and the port freed
	if _, err := os.Stat(pidPath()); !os.IsNotExist(err) {
		t.Errorf("pid file not removed on shutdown: %v", err)
	}
	if body := waitForHealth(t, healthURL, 2*time.Second); body != nil {
		t.Error("health server still serving after shutdown")
	}
}

func TestRunShutsDownOnRequestStop(t *testing.T) {
	cfg, path := gatewayConfig(t)
	healthURL := fmt.Sprintf("http://%s/health", net.JoinHostPort(cfg.Gateway.Host, strconv.Itoa(cfg.Gateway.Port)))

	errCh := make(chan error, 1)
	go func() { errCh <- Run(path) }()
	if waitForHealth(t, healthURL, 20*time.Second) == nil {
		t.Fatal("gateway never served /health")
	}

	// While it serves, the tray's overview reads from it: this config runs
	// bare, and the rows must say so rather than claim more.
	status := strings.Join(StatusLines(), "\n")
	for _, want := range []string{"factor ", "\nup ", "memory: off", "channels: none"} {
		if !strings.Contains(status, want) {
			t.Errorf("status %q is missing %q", status, want)
		}
	}

	// The tray's quit item ends the daemon the way SIGTERM does — cleanly,
	// and without coming back.
	RequestStop()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after RequestStop")
	}
	if _, err := os.Stat(pidPath()); !os.IsNotExist(err) {
		t.Errorf("pid file not removed on shutdown: %v", err)
	}
}

func TestRunRejectsSecondInstance(t *testing.T) {
	_, path := gatewayConfig(t)
	if err := writePidFile(); err != nil { // pretend a gateway is already up
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(pidPath()) }()

	err := Run(path)
	if err == nil {
		t.Fatal("second gateway started while one was running")
	}
	if _, statErr := os.Stat(pidPath()); statErr != nil {
		t.Error("the refused start deleted the running gateway's pid file")
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run(path); err == nil {
		t.Error("Run accepted a malformed config")
	}
}

func TestRunRejectsUnusableGatewayPort(t *testing.T) {
	cfg, _ := gatewayConfig(t)
	// hold the port so the health listener cannot bind
	l, err := net.Listen("tcp", net.JoinHostPort(cfg.Gateway.Host, strconv.Itoa(cfg.Gateway.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	cfg.Agent.Workspace = filepath.Join(home, "workspace")
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, mustJSON(t, cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Run(path); err == nil {
		t.Error("Run started with an unavailable gateway port")
	}
	if _, statErr := os.Stat(pidPath()); !os.IsNotExist(statErr) {
		t.Error("pid file left behind after a failed start")
	}
}

func TestAnnounceRelease(t *testing.T) {
	var sent []bus.OutboundMessage
	publish := func(m bus.OutboundMessage) bool { sent = append(sent, m); return true }
	rel := upgrade.Release{Version: "v0.4.0", Notes: "https://example.test/v0.4.0"}

	// Nobody has spoken to this daemon yet: there is no one to tell.
	announceRelease(rel, func() (string, string, bool) { return "", "", false }, publish)
	if len(sent) != 0 {
		t.Fatalf("announced into the void: %+v", sent)
	}

	announceRelease(rel, func() (string, string, bool) { return "telegram", "42", true }, publish)
	if len(sent) != 1 {
		t.Fatalf("sent %d messages", len(sent))
	}
	got := sent[0]
	if got.Channel != "telegram" || got.ChatID != "42" {
		t.Errorf("addressed to %s:%s", got.Channel, got.ChatID)
	}
	for _, want := range []string{"v0.4.0", "factor upgrade", rel.Notes} {
		if !strings.Contains(got.Content, want) {
			t.Errorf("message %q is missing %q", got.Content, want)
		}
	}
}

// A newer engine image is reported, never installed on its own: swapping it
// is the user's call even though it never takes Factor down.
func TestAnnounceEngine(t *testing.T) {
	var sent []bus.OutboundMessage
	publish := func(m bus.OutboundMessage) bool { sent = append(sent, m); return true }
	rel := upgrade.SmrtiRelease{Version: "0.9.1", Running: "0.9.0"}

	// No external chat has been used yet: there is nobody to tell.
	announceEngine(rel, func() (string, string, bool) { return "", "", false }, publish)
	if len(sent) != 0 {
		t.Fatalf("announced into the void: %+v", sent)
	}

	announceEngine(rel, func() (string, string, bool) { return "telegram", "42", true }, publish)
	if len(sent) != 1 {
		t.Fatalf("sent %d messages", len(sent))
	}
	got := sent[0]
	if got.Channel != "telegram" || got.ChatID != "42" {
		t.Errorf("addressed to %s:%s", got.Channel, got.ChatID)
	}
	// Both versions have to be in the line, or "is out" says nothing about
	// whether this machine is behind.
	for _, want := range []string{"0.9.1", "0.9.0", "factor upgrade"} {
		if !strings.Contains(got.Content, want) {
			t.Errorf("message %q is missing %q", got.Content, want)
		}
	}
}
