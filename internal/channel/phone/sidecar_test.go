package phone

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain lets this test binary impersonate the Python voice shell, so the
// supervisor's spawn, health, restart and shutdown paths run against a real
// child process rather than a mock of one.
func TestMain(m *testing.M) {
	switch os.Getenv("FACTOR_TEST_VOICESHELL_MODE") {
	case "serve":
		fakeVoiceShell()
		os.Exit(0)
	case "exit":
		os.Exit(3) // dies immediately: exercises the restart path
	case "hang":
		select {} // never becomes healthy: exercises the unhealthy path
	}
	os.Exit(m.Run())
}

// fakeVoiceShell serves the control API the real shell exposes, echoing back
// the configuration blob it was handed so the tests can assert on it.
func fakeVoiceShell() {
	var cfg shellConfig
	if err := json.Unmarshal([]byte(os.Getenv("FACTOR_VOICE_CONFIG")), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "fake voice shell: bad config:", err)
		os.Exit(4)
	}
	var placed atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "config": cfg})
	})
	mux.HandleFunc("POST /call", func(w http.ResponseWriter, r *http.Request) {
		var req placeCallRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.To == "+15550009999" {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "the carrier refused the call"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"call_id": fmt.Sprintf("call-%d", placed.Add(1))})
	})
	// Watchdog: never outlive the test run, even if a test panics.
	go func() { time.Sleep(2 * time.Minute); os.Exit(0) }()
	srv := &http.Server{
		Addr:              net.JoinHostPort(cfg.ControlHost, fmt.Sprint(cfg.ControlPort)),
		Handler:           mux,
		ReadHeaderTimeout: time.Second,
	}
	_ = srv.ListenAndServe()
}

// selfCommand returns this test binary configured to act as a voice shell.
func selfCommand(t *testing.T, mode string) string {
	t.Helper()
	t.Setenv("FACTOR_TEST_VOICESHELL_MODE", mode) // inherited by the spawned child
	return os.Args[0]
}

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

// supervisorFor builds a supervisor around the fake shell in a throwaway home.
func supervisorFor(t *testing.T, mode string, mutate func(*Config)) (*supervisor, Config) {
	t.Helper()
	home := t.TempDir()
	cfg := prepared(t, func(c *Config) {
		c.Command = selfCommand(t, mode)
		c.SidecarPort = freePort(t)
		c.BridgePort = freePort(t)
		if mutate != nil {
			mutate(c)
		}
	})
	s := newSupervisor(cfg, home, "test-token", func() (shellConfig, error) {
		return renderShellConfig(cfg, cfg.BridgePort, "test-token"), nil
	})
	s.probeInterval = 50 * time.Millisecond
	t.Cleanup(s.stop)
	return s, cfg
}

func waitFor(t *testing.T, within time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestSupervisorSpawnsTheShellAndPassesItsConfiguration(t *testing.T) {
	s, cfg := supervisorFor(t, "serve", nil)
	s.start(context.Background())

	if !waitFor(t, 30*time.Second, s.Healthy) {
		t.Fatal("the voice shell never became healthy")
	}
	if reason := s.Down(); reason != "" {
		t.Errorf("a healthy shell reported itself down: %s", reason)
	}

	// The child got its configuration through the environment, not argv.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", cfg.SidecarPort))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Config shellConfig `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Config.BridgeToken != "test-token" {
		t.Errorf("bridge token did not reach the shell: %q", body.Config.BridgeToken)
	}
	if body.Config.Carrier.AuthToken != cfg.TwilioAuthToken {
		t.Errorf("carrier credentials did not reach the shell")
	}

	// The script is materialized next to the config so it can be inspected.
	if _, err := os.Stat(s.scriptPath); err != nil {
		t.Errorf("voice shell script not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.home, "logs", "voiceshell.log")); err != nil {
		t.Errorf("voice shell log not created: %v", err)
	}
}

func TestSupervisorNeverPutsSecretsInArgv(t *testing.T) {
	s, _ := supervisorFor(t, "serve", nil)
	// The only argument is the script path; everything else rides in the env.
	if strings.Contains(s.scriptPath, "twilio-secret") {
		t.Error("a secret leaked into the script path")
	}
	shell, err := s.shellCfg()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(shell)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), "twilio-secret") {
		t.Error("the carrier token is missing from the environment blob the shell needs")
	}
}

func TestSupervisorRestartsAShellThatDies(t *testing.T) {
	s, _ := supervisorFor(t, "exit", nil)
	s.start(context.Background())

	time.Sleep(300 * time.Millisecond)
	if s.Healthy() {
		t.Error("a dead shell reported healthy")
	}
	// Stopping while the supervisor is in its restart backoff must still be
	// prompt: the gateway waits on this during shutdown.
	done := make(chan struct{})
	go func() { s.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("stop hung while the supervisor was backing off")
	}
}

func TestSupervisorStopsAnUnresponsiveShell(t *testing.T) {
	s, _ := supervisorFor(t, "hang", nil)
	s.start(context.Background())

	time.Sleep(500 * time.Millisecond)
	if s.Healthy() {
		t.Error("a shell that never answers reported healthy")
	}
	done := make(chan struct{})
	go func() { s.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("stop hung on an unresponsive child")
	}
}

// A missing interpreter takes calls down, not the gateway: the supervisor
// reports why and keeps retrying.
func TestSupervisorReportsAMissingInterpreter(t *testing.T) {
	s, _ := supervisorFor(t, "serve", func(c *Config) {
		c.Command = filepath.Join(t.TempDir(), "no-such-python")
	})
	s.start(context.Background())

	if !waitFor(t, 10*time.Second, func() bool { return s.Down() != "" }) {
		t.Fatal("the supervisor never explained why it cannot run")
	}
	if !strings.Contains(s.Down(), "does not exist") {
		t.Errorf("reason = %q, want it to name the missing interpreter", s.Down())
	}
	if s.Healthy() {
		t.Error("healthy with no interpreter")
	}
}

func TestSupervisorSurfacesAnUnusableAudioTier(t *testing.T) {
	home := t.TempDir()
	cfg := prepared(t, func(c *Config) { c.Command = selfCommand(t, "serve") })
	s := newSupervisor(cfg, home, "test-token", func() (shellConfig, error) {
		return shellConfig{}, fmt.Errorf("local speech-to-text server is unreachable")
	})
	s.probeInterval = 50 * time.Millisecond
	t.Cleanup(s.stop)
	s.start(context.Background())

	if !waitFor(t, 10*time.Second, func() bool { return s.Down() != "" }) {
		t.Fatal("an unusable tier never surfaced")
	}
	if !strings.Contains(s.Down(), "unreachable") {
		t.Errorf("reason = %q", s.Down())
	}
}

// With control_api_base set, someone else runs the shell: Factor must watch it
// without ever spawning a second one.
func TestSupervisorInExternalModeNeverSpawns(t *testing.T) {
	shell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer shell.Close()

	home := t.TempDir()
	cfg := prepared(t, func(c *Config) {
		c.ControlAPIBase = shell.URL
		c.Command = filepath.Join(home, "must-never-be-spawned")
	})
	s := newSupervisor(cfg, home, "test-token", func() (shellConfig, error) {
		return renderShellConfig(cfg, cfg.BridgePort, "tok"), nil
	})
	s.probeInterval = 50 * time.Millisecond
	defer s.stop()
	s.start(context.Background())

	if !waitFor(t, 5*time.Second, s.Healthy) {
		t.Fatal("an external shell was never adopted")
	}
	if _, err := os.Stat(s.scriptPath); err == nil {
		t.Error("external mode wrote a script it will never run")
	}
}

func TestSupervisorTracksAnExternalShellGoingAway(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	shell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer shell.Close()

	cfg := prepared(t, func(c *Config) { c.ControlAPIBase = shell.URL })
	s := newSupervisor(cfg, t.TempDir(), "test-token", func() (shellConfig, error) { return shellConfig{}, nil })
	s.probeInterval = 50 * time.Millisecond
	defer s.stop()
	s.start(context.Background())

	if !waitFor(t, 5*time.Second, s.Healthy) {
		t.Fatal("never became healthy")
	}
	up.Store(false)
	if !waitFor(t, 5*time.Second, func() bool { return !s.Healthy() }) {
		t.Error("a shell that stopped answering is still reported healthy")
	}
	up.Store(true)
	if !waitFor(t, 5*time.Second, s.Healthy) {
		t.Error("health never recovered; the supervisor stopped probing")
	}
}

// ---- control client --------------------------------------------------------

func TestControlClientPlacesCalls(t *testing.T) {
	var got placeCallRequest
	shell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]string{"call_id": "CA-42"})
	}))
	defer shell.Close()

	client := newControlClient(shell.URL)
	id, err := client.placeCall(context.Background(), placeCallRequest{
		To: "+15550001111", Goal: "ask about dinner", FirstMessage: "hi, quick question",
	})
	if err != nil {
		t.Fatalf("placeCall: %v", err)
	}
	if id != "CA-42" {
		t.Errorf("call id = %q", id)
	}
	if got.To != "+15550001111" || got.Goal != "ask about dinner" || got.FirstMessage != "hi, quick question" {
		t.Errorf("the shell received %+v", got)
	}
}

func TestControlClientReportsRefusals(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{
			name: "the shell refuses",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"not on the outbound allowlist"}`))
			},
			wantErr: "not on the outbound allowlist",
		},
		{
			name: "the shell says nothing useful",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`carrier exploded`))
			},
			wantErr: "carrier exploded",
		},
		{
			name:    "the shell forgets the call id",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) },
			wantErr: "no call id",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			shell := httptest.NewServer(c.handler)
			defer shell.Close()
			_, err := newControlClient(shell.URL).placeCall(context.Background(), placeCallRequest{To: "+1555"})
			if err == nil {
				t.Fatalf("expected an error mentioning %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

func TestControlClientReportsAnAbsentShell(t *testing.T) {
	client := newControlClient("http://127.0.0.1:1")
	if err := client.health(context.Background()); err == nil {
		t.Error("health passed with nothing listening")
	}
	_, err := client.placeCall(context.Background(), placeCallRequest{To: "+1555"})
	if err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("placeCall error = %v, want it to say the shell is unreachable", err)
	}
}

func TestControlClientHealthRejectsAnUnhappyStatus(t *testing.T) {
	shell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer shell.Close()
	if err := newControlClient(shell.URL).health(context.Background()); err == nil {
		t.Error("health accepted a 503")
	}
}
