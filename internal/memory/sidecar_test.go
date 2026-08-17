package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

// TestMain lets the test binary impersonate the `smrti` executable so the
// sidecar's spawn/health/shutdown paths run against a real child process.
func TestMain(m *testing.M) {
	switch os.Getenv("FACTOR_TEST_SMRTI_MODE") {
	case "serve":
		fakeSmrtiServe()
		os.Exit(0)
	case "exit":
		os.Exit(3) // dies immediately: exercises the restart path
	case "hang":
		select {} // never becomes healthy: exercises the startup-timeout warning
	}
	os.Exit(m.Run())
}

// fakeSmrtiServe parses `serve rest --host H --port P` like the real CLI and
// serves /status, echoing back the environment the sidecar handed it.
func fakeSmrtiServe() {
	host, port := "127.0.0.1", "8420"
	for i, arg := range os.Args {
		if arg == "--host" && i+1 < len(os.Args) {
			host = os.Args[i+1]
		}
		if arg == "--port" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		env := map[string]string{}
		for _, key := range []string{
			"SMRTI_DB", "SMRTI_TENANT_ID", "SMRTI_SPACE", "SMRTI_PERSONALITY",
			"SMRTI_REFLECT_INTERVAL", "SMRTI_EXTRACT_MODE", "SMRTI_EXTRACT_URL",
			"SMRTI_EXTRACT_MODEL", "SMRTI_IGNORE_PATTERNS", "SMRTI_API_KEY",
		} {
			env[key] = os.Getenv(key)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"total_atoms": 1, "env": env})
	})
	mux.HandleFunc("/recall", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"memories":[]}`))
	})
	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		go func() { time.Sleep(50 * time.Millisecond); os.Exit(0) }()
	})
	// Watchdog: never outlive the test run, even if a test panics.
	go func() { time.Sleep(2 * time.Minute); os.Exit(0) }()
	srv := &http.Server{Addr: net.JoinHostPort(host, port), Handler: mux, ReadHeaderTimeout: time.Second}
	_ = srv.ListenAndServe()
}

// selfCommand returns this test binary configured to act as a fake smrti.
func selfCommand(t *testing.T, mode string) string {
	t.Helper()
	t.Setenv("FACTOR_TEST_SMRTI_MODE", mode) // inherited by the spawned child
	return os.Args[0]
}

// freePort reserves and releases a port so the child can bind it.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func sidecarConfig(t *testing.T, mode string) config.MemoryConfig {
	t.Helper()
	cfg := config.Default().Memory
	cfg.Host = "127.0.0.1"
	cfg.Port = freePort(t)
	cfg.Command = selfCommand(t, mode)
	cfg.DBPath = filepath.Join(t.TempDir(), "memory.db")
	cfg.Space = "testspace"
	cfg.Tenant = "testtenant"
	cfg.StartupTimeoutSecs = 1
	cfg.IgnorePatterns = []string{"^HEARTBEAT_OK$"}
	return cfg
}

func waitHealthy(t *testing.T, eng Engine, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if eng.Healthy() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestSidecarSpawnsAndPassesEnvironment(t *testing.T) {
	cfg := sidecarConfig(t, "serve")
	logDir := t.TempDir()
	extract := ExtractSettings{Mode: "hybrid", URL: "http://127.0.0.1:11434", Model: "qwen3", Key: "llm-key"}

	eng, err := NewEngine(context.Background(), cfg, extract, logDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	if !waitHealthy(t, eng, 20*time.Second) {
		t.Fatal("sidecar never became healthy")
	}
	if !eng.Enabled() {
		t.Error("engine reports itself disabled after a successful spawn")
	}

	status, err := eng.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	env, _ := status["env"].(map[string]any)
	want := map[string]string{
		"SMRTI_DB":               cfg.DBPath,
		"SMRTI_TENANT_ID":        "testtenant",
		"SMRTI_SPACE":            "testspace",
		"SMRTI_PERSONALITY":      cfg.Personality,
		"SMRTI_REFLECT_INTERVAL": strconv.Itoa(cfg.ReflectIntervalSecs),
		"SMRTI_EXTRACT_MODE":     "hybrid",
		"SMRTI_EXTRACT_URL":      "http://127.0.0.1:11434",
		"SMRTI_EXTRACT_MODEL":    "qwen3",
		"SMRTI_IGNORE_PATTERNS":  "^HEARTBEAT_OK$",
	}
	for key, expected := range want {
		if got, _ := env[key].(string); got != expected {
			t.Errorf("child env %s = %q, want %q", key, got, expected)
		}
	}
	// the sidecar's own log file is created next to the other logs
	if _, err := os.Stat(filepath.Join(logDir, "smrti.log")); err != nil {
		t.Errorf("sidecar log not created: %v", err)
	}
}

func TestSidecarBuildEnvOptionalFields(t *testing.T) {
	cfg := config.Default().Memory
	cfg.IgnorePatterns = nil
	cfg.APIKey = "smrti-key"
	s := &Sidecar{cfg: cfg, extract: ExtractSettings{Mode: "local"}}
	env := strings.Join(s.buildEnv(), "\n")

	if !strings.Contains(env, "SMRTI_EXTRACT_MODE=local") {
		t.Error("extract mode missing")
	}
	if !strings.Contains(env, "SMRTI_API_KEY=smrti-key") {
		t.Error("api key missing")
	}
	if strings.Contains(env, "SMRTI_IGNORE_PATTERNS=") {
		t.Error("empty ignore patterns should be omitted entirely")
	}
	if strings.Contains(env, "SMRTI_EXTRACT_URL=") || strings.Contains(env, "SMRTI_EXTRACT_MODEL=") {
		t.Error("local extraction must not set an extraction endpoint")
	}
	// multiple ignore patterns are newline-joined, as smrti expects
	cfg.IgnorePatterns = []string{"^a$", "^b$"}
	s = &Sidecar{cfg: cfg, extract: ExtractSettings{Mode: "hybrid"}}
	if !strings.Contains(strings.Join(s.buildEnv(), "\n"), "SMRTI_IGNORE_PATTERNS=^a$\n^b$") {
		t.Error("ignore patterns not newline-joined")
	}
}

func TestSidecarKeepAliveLeavesProcessRunning(t *testing.T) {
	cfg := sidecarConfig(t, "serve")
	cfg.KeepAlive = true

	eng, err := NewEngine(context.Background(), cfg, ExtractSettings{Mode: "local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !waitHealthy(t, eng, 20*time.Second) {
		t.Fatal("sidecar never became healthy")
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}

	// the child outlives us on purpose: a later run adopts it warm
	client := NewClient(cfg.BaseURL(), "", "")
	if err := client.CheckHealth(context.Background()); err != nil {
		t.Errorf("keep_alive sidecar died with the engine: %v", err)
	}

	// a second engine adopts it without spawning anything (command is bogus now)
	adoptCfg := cfg
	adoptCfg.Command = "/nonexistent-binary-must-not-spawn"
	adopted, err := NewEngine(context.Background(), adoptCfg, ExtractSettings{Mode: "local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !adopted.Healthy() {
		t.Error("warm sidecar not adopted synchronously at startup")
	}
	_ = adopted.Close()
	stopFakeSmrti(t, cfg.BaseURL())
}

// A recall that times out while smrti warms up (first run downloads models)
// marks the client unhealthy. The supervisor must keep re-probing the live
// child so the verdict recovers; it used to stop after the first success,
// leaving "memory ×" stuck for the rest of the session.
func TestSidecarReprobesAfterHealthPoisoned(t *testing.T) {
	cfg := sidecarConfig(t, "serve")
	cfg.KeepAlive = false

	s := &Sidecar{
		client:        NewClient(cfg.BaseURL(), "", ""),
		cfg:           cfg,
		extract:       ExtractSettings{Mode: "local"},
		probeInterval: 50 * time.Millisecond,
	}
	s.start(context.Background())
	defer func() { _ = s.Close() }()

	if !waitHealthy(t, s, 20*time.Second) {
		t.Fatal("sidecar never became healthy")
	}
	// Simulate the timed-out request: the server is fine, but the client
	// flagged itself unhealthy.
	s.client.healthy.Store(false)
	if !waitHealthy(t, s, 5*time.Second) {
		t.Error("health verdict never recovered; supervisor stopped probing after the first success")
	}
}

func TestSidecarWithoutKeepAliveStopsTheProcess(t *testing.T) {
	cfg := sidecarConfig(t, "serve")
	cfg.KeepAlive = false

	eng, err := NewEngine(context.Background(), cfg, ExtractSettings{Mode: "local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !waitHealthy(t, eng, 20*time.Second) {
		t.Fatal("sidecar never became healthy")
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}

	client := NewClient(cfg.BaseURL(), "", "")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if client.CheckHealth(context.Background()) != nil {
			return // process gone, as required
		}
		time.Sleep(50 * time.Millisecond)
	}
	stopFakeSmrti(t, cfg.BaseURL())
	t.Error("sidecar survived Close despite keep_alive=false")
}

// stopFakeSmrti asks a surviving keep-alive child to exit so no process or
// port leaks between tests.
func stopFakeSmrti(t *testing.T, baseURL string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(baseURL + "/shutdown")
	if err == nil {
		_ = resp.Body.Close()
	}
}

func TestSidecarRestartsAfterChildExit(t *testing.T) {
	cfg := sidecarConfig(t, "exit")
	cfg.KeepAlive = false

	eng, err := NewEngine(context.Background(), cfg, ExtractSettings{Mode: "local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	// It can never become healthy; what matters is that Close still returns
	// promptly while the supervisor is in its restart backoff.
	time.Sleep(300 * time.Millisecond)
	if eng.Healthy() {
		t.Error("engine reports healthy with a dead child")
	}
	done := make(chan struct{})
	go func() { _ = eng.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung while the supervisor was backing off")
	}
}

func TestSidecarStartupTimeoutWarnsButKeepsWaiting(t *testing.T) {
	cfg := sidecarConfig(t, "hang")
	cfg.KeepAlive = false
	cfg.StartupTimeoutSecs = 1

	eng, err := NewEngine(context.Background(), cfg, ExtractSettings{Mode: "local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // past the startup timeout
	if eng.Healthy() {
		t.Error("hung child reported healthy")
	}
	done := make(chan struct{})
	go func() { _ = eng.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Close hung on an unresponsive child")
	}
}

func TestExternalModeNeverSpawns(t *testing.T) {
	cfg := config.Default().Memory
	cfg.Mode = "external"
	cfg.Command = "/nonexistent-binary-must-not-spawn"
	cfg.Port = freePort(t)
	cfg.URL = fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)

	eng, err := NewEngine(context.Background(), cfg, ExtractSettings{Mode: "local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()
	time.Sleep(100 * time.Millisecond)
	if eng.Healthy() {
		t.Error("external engine healthy with nothing listening")
	}
	if !eng.Enabled() {
		t.Error("external engine must stay enabled even while unreachable")
	}
}

func TestSidecarDelegatesEveryOperation(t *testing.T) {
	srv, remembered := fakeSmrti(t)
	cfg := config.Default().Memory
	cfg.Mode = "external"
	cfg.URL = srv.URL

	eng, err := NewEngine(context.Background(), cfg, ExtractSettings{Mode: "local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()
	if !waitHealthy(t, eng, 5*time.Second) {
		t.Fatal("engine not healthy against the fake server")
	}
	ctx := context.Background()

	if _, err := eng.Remember(ctx, RememberRequest{Content: "via sidecar"}); err != nil {
		t.Errorf("Remember: %v", err)
	}
	if len(*remembered) == 0 {
		t.Error("Remember did not reach the server")
	}
	if _, err := eng.Recall(ctx, "q", 3, 0.1, Scope{}); err != nil {
		t.Errorf("Recall: %v", err)
	}
	if err := eng.Forget(ctx, "q", "because", ""); err != nil {
		t.Errorf("Forget: %v", err)
	}
	if _, err := eng.Reflect(ctx); err != nil {
		t.Errorf("Reflect: %v", err)
	}
	if _, err := eng.Status(ctx); err != nil {
		t.Errorf("Status: %v", err)
	}
}
