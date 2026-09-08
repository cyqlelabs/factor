//go:build !nobrowser

package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

// fakeCamofoxServe is what the test binary becomes when the session spawns
// it as Node. It answers /health with the environment it was handed, which
// is how the spawn tests prove the settings reached the process rather than
// only the struct.
//
// FACTOR_TEST_CAMOFOX_SERVE picks the behaviour: "ok" serves, "mute" starts
// and never listens, "exit" dies at once.
func fakeCamofoxServe() {
	switch os.Getenv("FACTOR_TEST_CAMOFOX_SERVE") {
	case "exit":
		os.Exit(3)
	case "mute":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		env := map[string]string{}
		for _, key := range []string{
			"CAMOFOX_PORT", "CAMOFOX_BIND_HOST", "CAMOFOX_CRASH_REPORT_ENABLED",
			"CAMOUFOX_INSTALL_DIR", "CAMOFOX_PROFILE_DIR", "CAMOFOX_COOKIES_DIR",
			"CAMOFOX_TRACES_DIR", "CAMOFOX_UPLOADS_DIR",
		} {
			env[key] = os.Getenv(key)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "args": os.Args[1:], "env": env})
	})
	// Never outlive the test run, however the test ends.
	go func() { time.Sleep(2 * time.Minute); os.Exit(0) }()
	srv := &http.Server{
		Addr:              net.JoinHostPort("127.0.0.1", os.Getenv("CAMOFOX_PORT")),
		Handler:           mux,
		ReadHeaderTimeout: time.Second,
	}
	_ = srv.ListenAndServe()
	os.Exit(0)
}

// spawnable prepares a home holding an installed package, and points the
// engine's Node lookup at this test binary in the given mode.
func spawnable(t *testing.T, mode string) (home string, cfg config.BrowserConfig) {
	t.Helper()
	home = t.TempDir()
	server := camofoxServer(CamofoxDir(home))
	if err := os.MkdirAll(filepath.Dir(server), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server, []byte("// server"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FACTOR_TEST_CAMOFOX_SERVE", mode) // inherited by the child
	saved := lookPath
	lookPath = func(name string) (string, error) {
		if name == "node" {
			return os.Args[0], nil
		}
		return "", errors.New("not found")
	}
	savedRun := runCmd
	runCmd = func(_ context.Context, argv []string) (string, error) {
		if len(argv) == 2 && argv[1] == "--version" {
			return "v22.14.0\n", nil
		}
		return "", nil
	}
	t.Cleanup(func() { lookPath, runCmd = saved, savedRun })
	return home, config.BrowserConfig{
		Engine:      "camofox",
		UserDataDir: filepath.Join(home, "browser"),
		Camofox:     config.CamofoxConfig{Port: mustFreePort(t)},
	}
}

func mustFreePort(t *testing.T) int {
	t.Helper()
	port, err := freeLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// The whole spawn: a process started with Factor's environment, waited for,
// and killed on Close.
func TestCamofoxSpawnsTheServerAndStopsIt(t *testing.T) {
	home, cfg := spawnable(t, "ok")
	c := newCamofox(cfg, home, t.TempDir(), nil)
	if err := c.ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if c.cmd == nil || c.cmd.Process == nil {
		t.Fatal("no child was started")
	}
	pid := c.cmd.Process.Pid

	var health struct {
		Args []string          `json:"args"`
		Env  map[string]string `json:"env"`
	}
	if err := c.call(context.Background(), http.MethodGet, "/health", nil, &health); err != nil {
		t.Fatal(err)
	}
	if len(health.Args) != 1 || health.Args[0] != camofoxServer(CamofoxDir(home)) {
		t.Errorf("the child was run as %v, want the package's server", health.Args)
	}
	want := map[string]string{
		"CAMOFOX_PORT":                 strconv.Itoa(cfg.Camofox.Port),
		"CAMOFOX_BIND_HOST":            "127.0.0.1",
		"CAMOFOX_CRASH_REPORT_ENABLED": "false",
		"CAMOUFOX_INSTALL_DIR":         camoufoxDir(CamofoxDir(home)),
		"CAMOFOX_PROFILE_DIR":          filepath.Join(cfg.UserDataDir, "camofox", "profiles"),
	}
	for key, value := range want {
		if health.Env[key] != value {
			t.Errorf("the child got %s=%q, want %q", key, health.Env[key], value)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "camofox.log")); err != nil {
		t.Errorf("no log was opened for the child: %v", err)
	}

	// A second ensure adopts the running server rather than starting another.
	if err := c.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.cmd.Process.Pid != pid {
		t.Errorf("ensure restarted a healthy server (%d → %d)", pid, c.cmd.Process.Pid)
	}

	c.Close()
	c.Close() // idempotent
	deadline := time.Now().Add(5 * time.Second)
	for c.healthy() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if c.healthy() {
		t.Error("the server outlived Close")
	}
}

// A server somebody else is running is adopted, and never killed by Close:
// it belongs to whoever started it.
func TestCamofoxAdoptsAServerItDidNotStart(t *testing.T) {
	f, srv := newFakeCamofox(t)
	port, _ := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	home := t.TempDir() // no install here at all
	c := newCamofox(config.BrowserConfig{Camofox: config.CamofoxConfig{Port: port}}, home, t.TempDir(), nil)
	if err := c.ensure(context.Background()); err != nil {
		t.Fatalf("a running server was not adopted: %v", err)
	}
	if c.cmd != nil {
		t.Error("a child was started beside the running server")
	}
	c.Close()
	if !c.healthy() {
		t.Error("Close stopped a server Factor did not start")
	}
	if f.saw("POST /tabs") {
		t.Error("adoption opened a tab")
	}
}

// A port held by something that is not Camofox is reported. Spawning there
// would fork a child that dies on bind, and the next call would fork another.
func TestCamofoxRefusesAPortHeldBySomethingElse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	c := newCamofox(config.BrowserConfig{Camofox: config.CamofoxConfig{Port: port}}, t.TempDir(), t.TempDir(), nil)
	err = c.ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "browser.camofox.port") {
		t.Fatalf("err = %v, want the held port named with the way out", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Errorf("the error does not name the port: %v", err)
	}
}

// A server that starts and never listens gives up at the deadline, and says
// where to read what it printed.
func TestCamofoxGivesUpOnAServerThatNeverAnswers(t *testing.T) {
	saved := camofoxStart
	camofoxStart = 600 * time.Millisecond
	t.Cleanup(func() { camofoxStart = saved })

	home, cfg := spawnable(t, "mute")
	c := newCamofox(cfg, home, t.TempDir(), nil)
	defer c.Close()
	err := c.ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(home, "camofox.log")) {
		t.Errorf("the error does not name the log: %v", err)
	}
	if c.cmd != nil {
		t.Error("the child was left running after the deadline")
	}
}

// A cancelled turn stops waiting for a server rather than holding the lock
// to the deadline.
func TestCamofoxEnsureHonoursCancellation(t *testing.T) {
	saved := camofoxStart
	camofoxStart = 30 * time.Second
	t.Cleanup(func() { camofoxStart = saved })

	home, cfg := spawnable(t, "mute")
	c := newCamofox(cfg, home, t.TempDir(), nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.ensure(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s past the cancellation", elapsed)
	}
}

// Without a Node the engine says so, naming the version it needs — the one
// thing the user can act on.
func TestCamofoxReportsAMissingNode(t *testing.T) {
	home := t.TempDir()
	server := camofoxServer(CamofoxDir(home))
	if err := os.MkdirAll(filepath.Dir(server), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server, []byte("// server"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { lookPath = saved })

	c := newCamofox(config.BrowserConfig{Camofox: config.CamofoxConfig{Port: mustFreePort(t)}}, home, t.TempDir(), nil)
	err := c.ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), nodeMinVersion) {
		t.Fatalf("err = %v, want the Node requirement named", err)
	}
}

// A configured directory that holds no install is a mistake to report, not
// something to quietly install over.
func TestCamofoxReportsAConfiguredDirWithNoInstall(t *testing.T) {
	dir := t.TempDir()
	cfg := config.BrowserConfig{Camofox: config.CamofoxConfig{Port: mustFreePort(t), Dir: dir}}
	c := newCamofox(cfg, t.TempDir(), t.TempDir(), nil)
	err := c.ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "browser.camofox.dir") {
		t.Fatalf("err = %v, want the configured directory named", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("the error does not name the directory: %v", err)
	}
}

// Provisioning is tried once per session: a machine that cannot install the
// engine must not run npm again on every page the model asks for.
func TestCamofoxInstallsOnceASession(t *testing.T) {
	calls := 0
	saved := provisionCamofox
	provisionCamofox = func(context.Context, string, Progress) (string, bool, error) {
		calls++
		return "", false, errors.New("no network")
	}
	t.Cleanup(func() { provisionCamofox = saved })

	c := newCamofox(config.BrowserConfig{Camofox: config.CamofoxConfig{Port: mustFreePort(t)}}, t.TempDir(), t.TempDir(), nil)
	first := c.ensure(context.Background())
	if first == nil || !strings.Contains(first.Error(), "no network") {
		t.Fatalf("first ensure = %v", first)
	}
	second := c.ensure(context.Background())
	if second == nil || !strings.Contains(second.Error(), "factor init") {
		t.Fatalf("second ensure = %v, want it to stop trying and say what to run", second)
	}
	if calls != 1 {
		t.Errorf("provisioned %d times in one session", calls)
	}
}

// A reply the server never sends is a failure the tools report, not a hang.
func TestCamofoxReportsAServerThatDiesMidCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close() // the answer never arrives
	}))
	defer srv.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	c := newCamofox(config.BrowserConfig{Camofox: config.CamofoxConfig{Port: port}}, t.TempDir(), t.TempDir(), nil)
	res := c.navigate(context.Background(), "https://example.com")
	if !res.IsError || !strings.Contains(res.ForLLM, "navigate failed") {
		t.Errorf("navigate = %+v", res)
	}
}

// A body that is not JSON is reported with what the server actually said,
// which is the only clue when a proxy or a different program answers.
func TestCamofoxReportsANonJSONReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>proxy error</html>\nsecond line"))
	}))
	defer srv.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	c := newCamofox(config.BrowserConfig{Camofox: config.CamofoxConfig{Port: port}}, t.TempDir(), t.TempDir(), nil)
	res := c.navigate(context.Background(), "https://example.com")
	if !res.IsError || !strings.Contains(res.ForLLM, "proxy error") {
		t.Errorf("navigate = %+v, want what the server said", res)
	}
	if strings.Contains(res.ForLLM, "second line") {
		t.Errorf("the whole body was pasted in: %+v", res)
	}
}

// The engine writes nothing outside Factor's directories, including when no
// user data directory is configured.
func TestCamofoxEnvFallsBackToTheFactorHome(t *testing.T) {
	c := newCamofox(config.BrowserConfig{}, filepath.Join("home", ".factor"), "/ws", nil)
	env := strings.Join(c.env(), "\n")
	if !strings.Contains(env, "CAMOFOX_PROFILE_DIR="+filepath.Join("home", ".factor", "browser", "camofox", "profiles")) {
		t.Errorf("env: %s", env)
	}
	if !strings.Contains(env, fmt.Sprintf("CAMOFOX_PORT=%d", 9377)) {
		t.Error("the default port did not reach the child")
	}
}
