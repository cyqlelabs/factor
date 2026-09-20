package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/decision"
)

// The supervisor is exercised against a fake server spawned by re-execing
// this test binary, which is how the memory engine's and the speech server's
// supervisors are tested: a real child, a real port, a real health probe and
// a real HTTP round trip, with only the model itself stood in for.

func TestMain(m *testing.M) {
	switch os.Getenv("FACTOR_TEST_LAYA_MODE") {
	case "serve":
		fakeLayaServer()
		os.Exit(0)
	case "exit":
		os.Exit(3) // dies immediately: exercises the restart path
	case "hang":
		select {} // never answers: exercises the never-ready path
	}
	os.Exit(m.Run())
}

// fakeLayaServer answers the two routes the real one does, off the same
// configuration blob Factor hands the real one.
func fakeLayaServer() {
	var cfg struct {
		Host   string `json:"host"`
		Port   int    `json:"port"`
		Device string `json:"device"`
	}
	if err := json.Unmarshal([]byte(os.Getenv("FACTOR_DECISION_CONFIG")), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "fake laya: bad config:", err)
		os.Exit(4)
	}
	maxLen := 1024
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "max_len": maxLen, "head_max_len": 192,
		})
	})
	mux.HandleFunc("POST /v1/systemone", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model     string                       `json:"model"`
			State     any                          `json:"state"`
			Questions map[string]decision.Question `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		answers := map[string]decision.Answer{}
		for name, q := range req.Questions {
			ids := decision.Candidates(q)
			probs := map[string]float64{}
			for i, id := range ids {
				probs[id] = 0.1 / float64(max(len(ids)-1, 1))
				if i == 0 {
					probs[id] = 0.9
				}
			}
			answers[name] = decision.Answer{Choice: ids[0], Confidence: 0.9, Probabilities: probs}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": answers, "model": "laya-multilingual",
			"usage": map[string]any{"input_tokens": 120, "output_tokens": 0},
		})
	})
	go func() { time.Sleep(2 * time.Minute); os.Exit(0) }()
	srv := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port)),
		Handler:           mux,
		ReadHeaderTimeout: time.Second,
	}
	_ = srv.ListenAndServe()
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// backendFor builds a supervisor around the fake server, with the install
// path never reached: Command names this test binary.
func backendFor(t *testing.T, mode string) *Backend {
	t.Helper()
	t.Setenv("FACTOR_TEST_LAYA_MODE", mode)
	b := New(Config{Port: freePort(t), Command: os.Args[0]}, t.TempDir())
	b.probeInterval = 50 * time.Millisecond
	t.Cleanup(b.Stop)
	return b
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The whole path: the supervisor spawns the server, waits for it to answer,
// reads the limits it reports, and a decision round-trips over the same
// client the hosted backend uses.
func TestBackendSpawnsServesAndAnswers(t *testing.T) {
	b := backendFor(t, "serve")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)
	waitFor(t, "the model to become healthy", b.Healthy)

	if b.Down() != "" {
		t.Errorf("a healthy model reports itself down: %q", b.Down())
	}
	if name := b.Name(); name != "laya-multilingual" {
		t.Errorf("Name() = %q", name)
	}
	// The multilingual checkpoint's window, as the server reported it.
	if l := b.Limits(); l.MaxCandidates != 16 || l.MaxStateChars != (1024-192)*3 {
		t.Errorf("limits = %+v", l)
	}

	resp, err := b.Decide(ctx, &decision.Request{
		State: map[string]any{"page": "a flight search"},
		Questions: map[string]decision.Question{
			"operation": {Criteria: map[string]any{"CLICK": "click", "DONE": "done"}},
		},
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if resp.Model != "laya-multilingual" || resp.Usage.InputTokens != 120 {
		t.Errorf("response = %+v", resp)
	}
	if err := decision.Validate(resp.Answers["operation"], []string{"CLICK", "DONE"}); err != nil {
		t.Errorf("the answer did not validate: %v", err)
	}

	b.Stop()
	if b.Healthy() {
		t.Error("a stopped model still reads as healthy")
	}
}

func TestLimitsOfIsSafeOnNonsense(t *testing.T) {
	cases := map[string]struct {
		h    health
		want decision.Limits
	}{
		"nothing reported":        {health{}, decision.Limits{}},
		"head bigger than window": {health{MaxLen: 100, HeadMaxLen: 200}, decision.Limits{MaxCandidates: 16}},
		"tiny head":               {health{MaxLen: 512, HeadMaxLen: 24}, decision.Limits{MaxCandidates: 2, MaxStateChars: (512 - 24) * 3}},
		"huge head":               {health{MaxLen: 8192, HeadMaxLen: 4096}, decision.Limits{MaxCandidates: 16, MaxStateChars: (8192 - 4096) * 3}},
	}
	for name, c := range cases {
		if got := limitsOf(c.h); got != c.want {
			t.Errorf("%s: limitsOf = %+v, want %+v", name, got, c.want)
		}
	}
}

// A model that is not up is reported as unavailable rather than dialled: the
// caller's fallback is the point, and a refused connection per decision is a
// slow way to reach it.
func TestDecideIsUnavailableWhileTheModelIsDown(t *testing.T) {
	b := New(Config{Port: freePort(t), Command: os.Args[0]}, t.TempDir())
	_, err := b.Decide(context.Background(), &decision.Request{
		Questions: map[string]decision.Question{"x": {Criteria: map[string]any{"a": 1, "b": 2}}},
	})
	if !errors.Is(err, decision.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "not started") {
		t.Errorf("err does not say why: %v", err)
	}
	var nilBackend *Backend
	if nilBackend.Healthy() || nilBackend.Down() != "not configured" || nilBackend.Limits() != (decision.Limits{}) {
		t.Error("a nil backend must read as off")
	}
	nilBackend.Start(context.Background()) // nil-safe, like every other method
	nilBackend.Stop()
}

// A child that dies is restarted, and the supervisor says so meanwhile.
func TestBackendRestartsAChildThatDies(t *testing.T) {
	b := backendFor(t, "exit")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)
	waitFor(t, "the exit to be reported", func() bool { return strings.Contains(b.Down(), "exited") })
	if b.Healthy() {
		t.Error("a model that exits at once reads as healthy")
	}
	if !strings.Contains(b.Down(), "layaserve.log") {
		t.Errorf("the failure does not say where to look: %q", b.Down())
	}
}

// A child that never answers is given up on rather than waited out forever.
func TestBackendGivesUpOnAChildThatNeverAnswers(t *testing.T) {
	b := backendFor(t, "hang")
	b.probeInterval = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b.Start(ctx)
	time.Sleep(300 * time.Millisecond)
	if b.Healthy() {
		t.Error("a model that never answers reads as healthy")
	}
	b.Stop() // must return: the child is killed rather than waited on
}

func TestConfigDefaults(t *testing.T) {
	var c Config
	if c.port() != DefaultPort || c.BaseURL() != fmt.Sprintf("http://127.0.0.1:%d", DefaultPort) {
		t.Errorf("port = %d url = %q", c.port(), c.BaseURL())
	}
	if !c.autoInstall() {
		t.Error("auto-install should default on")
	}
	no := false
	c.AutoInstall = &no
	if c.autoInstall() {
		t.Error("auto-install was not switched off")
	}
}

func TestWriteScriptIsIdempotentAndCarriesTheServer(t *testing.T) {
	home := t.TempDir()
	path := ScriptPath(home)
	if err := WriteScript(path); err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/v1/systemone", "/health", "system_one", "multilingual"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the embedded server lacks %q", want)
		}
	}
	time.Sleep(10 * time.Millisecond)
	if err := WriteScript(path); err != nil {
		t.Fatal(err)
	}
	again, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.ModTime().Equal(first.ModTime()) {
		t.Error("an unchanged script was rewritten, which disturbs a running child")
	}
}

// The embedded server is Python that never runs in CI, so at minimum it must
// be Python that parses.
func TestEmbeddedServerIsValidPython(t *testing.T) {
	python, err := systemPython()
	if err != nil {
		t.Skipf("no usable interpreter: %v", err)
	}
	path := ScriptPath(t.TempDir())
	if err := WriteScript(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, python, "-m", "py_compile", path).CombinedOutput(); err != nil {
		t.Fatalf("the embedded server does not compile: %v\n%s", err, out)
	}
}

func TestFindPythonNeedsLayaImportable(t *testing.T) {
	home := t.TempDir()
	if _, ok := FindPython(home); ok {
		t.Error("an empty home reported an interpreter")
	}
	// A virtualenv that exists but has no Laya is a half-finished install.
	bin := filepath.Dir(venvPython(home))
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(venvPython(home), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := FindPython(home); ok {
		t.Error("a virtualenv without Laya reported an interpreter")
	}
}

func TestResolveCommandRefusesWhenTheInstallIsOff(t *testing.T) {
	no := false
	b := New(Config{AutoInstall: &no}, t.TempDir())
	_, err := b.resolveCommand(context.Background())
	if err == nil || !strings.Contains(err.Error(), "auto_install is off") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(err.Error(), PackageSpec) {
		t.Errorf("the refusal does not say how to install it by hand: %v", err)
	}
	// A configured command that does not exist is named rather than guessed at.
	b = New(Config{Command: "definitely-not-an-interpreter"}, t.TempDir())
	if _, err := b.resolveCommand(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "decision.local.command") {
		t.Errorf("err = %v", err)
	}
}

func TestInstallHintNamesTheVenvAndThePin(t *testing.T) {
	hint := InstallHint()
	if !strings.Contains(hint, "decision-venv") || !strings.Contains(hint, PackageSpec) {
		t.Errorf("hint = %q", hint)
	}
	if !strings.HasSuffix(VenvDir("/home/x/.factor"), filepath.Join(".factor", "decision-venv")) {
		t.Errorf("VenvDir = %q", VenvDir("/home/x/.factor"))
	}
}

// Installing pulls torch behind it, so it is never begun by a process that
// may be about to exit: the gateway permits it, and so does the first
// decision actually asked for.
func TestTheInstallIsNotStartedUnasked(t *testing.T) {
	// No interpreter anywhere, so a permitted install fails at once instead
	// of actually fetching a few hundred megabytes of torch into a test.
	t.Setenv("PATH", t.TempDir())
	b := New(Config{Port: freePort(t)}, t.TempDir())
	_, err := b.resolveCommand(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not installed yet") {
		t.Fatalf("an unasked-for install was begun, or misreported: %v", err)
	}

	// A decision asked for is the permission a terminal session gives.
	_, _ = b.Decide(context.Background(), &decision.Request{
		Questions: map[string]decision.Question{"x": {Criteria: map[string]any{"a": 1, "b": 2}}},
	})
	_, err = b.resolveCommand(context.Background())
	if err == nil || strings.Contains(err.Error(), "not installed yet") {
		t.Errorf("a decision did not permit the install: %v", err)
	}
	if !strings.Contains(err.Error(), "Python") {
		t.Errorf("the install was permitted but failed for the wrong reason: %v", err)
	}

	// Provision is the permission the gateway gives, and it never runs when
	// the install is switched off or an interpreter was named by hand.
	no := false
	off := New(Config{AutoInstall: &no}, t.TempDir())
	off.Provision(context.Background())
	if off.installOK.Load() {
		t.Error("Provision permitted an install that config had switched off")
	}
	named := New(Config{Command: os.Args[0]}, t.TempDir())
	named.Provision(context.Background())
	if named.installOK.Load() {
		t.Error("Provision permitted an install over a configured interpreter")
	}
	var none *Backend
	none.Provision(context.Background()) // nil-safe like the rest
}
