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
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
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
	case "serve", "wedge":
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
	// A server that answers for a while and then stops, without exiting:
	// the shape a model takes when it wedges rather than dies, which the
	// supervisor has to notice by asking rather than by waiting on the
	// process.
	wedgeAfter, _ := strconv.Atoi(os.Getenv("FACTOR_TEST_LAYA_WEDGE"))
	var probes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		if n := probes.Add(1); wedgeAfter > 0 && int(n) > wedgeAfter {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "wedged"})
			return
		}
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

// Laya asks only for "torch>=2.0.0", and on Linux and Windows pip answers
// that with the CUDA build: measured here, a plain install lays down 5.6 GB,
// 3.2 GB of it NVIDIA runtime, on a machine that may have no GPU. The CPU
// wheel is fetched first so the dependency is already satisfied — unless the
// user asked for CUDA, in which case they have the hardware and said so.
func TestTheCPUBuildIsPreferredUnlessCUDAWasAskedFor(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("the published wheels are CPU-only on this platform")
	}
	for device, want := range map[string]bool{
		"":       false,
		"cpu":    false,
		"cuda":   true,
		" CUDA ": true,
		"cuda:0": true,
		"mps":    false,
	} {
		if got := wantsCUDA(device); got != want {
			t.Errorf("wantsCUDA(%q) = %v, want %v", device, got, want)
		}
	}
	// The hint a user follows by hand installs the same thing Factor does.
	hint := InstallHint()
	if !strings.Contains(hint, TorchCPUIndex) || !strings.Contains(hint, PackageSpec) {
		t.Errorf("hint = %q", hint)
	}
}

// The install paths, without ever performing one. What they mostly have to
// get right is failing clearly: this is the code a user meets when their
// machine cannot run the model, and "it did not work" is not an answer.
func TestInstallReportsWhatItCouldNotDo(t *testing.T) {
	// No interpreter at all.
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	var said []string
	_, err := Install(context.Background(), home, "cpu", func(format string, args ...any) {
		said = append(said, fmt.Sprintf(format, args...))
	})
	if err == nil || !strings.Contains(err.Error(), "Python 3.") {
		t.Fatalf("err = %v, want it to name the interpreter it wanted", err)
	}
	if _, statErr := os.Stat(VenvDir(home)); statErr == nil {
		t.Error("a virtualenv was built without an interpreter to build it with")
	}

	// An interpreter that is too old is named as such rather than ignored.
	bin := t.TempDir()
	old := filepath.Join(bin, "python3")
	if err := os.WriteFile(old, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if _, err := systemPython(); err == nil || !strings.Contains(err.Error(), "python3") {
		t.Errorf("err = %v, want it to name what it rejected", err)
	}

	// EnsureLaya answers from the virtualenv when one is already good,
	// without consulting PATH or installing anything.
	ready := t.TempDir()
	plantVenv(t, ready)
	path, installed, err := EnsureLaya(context.Background(), ready, "", true, nil)
	if err != nil || installed || path != venvPython(ready) {
		t.Errorf("path=%q installed=%v err=%v", path, installed, err)
	}
	// And refuses, with the manual command, when it may not install.
	if _, _, err := EnsureLaya(context.Background(), t.TempDir(), "", false, nil); err == nil ||
		!strings.Contains(err.Error(), InstallHint()) {
		t.Errorf("err = %v, want the hand-install command", err)
	}
}

// plantVenv makes a virtualenv that looks installed: an interpreter that
// exits 0, so the Laya import probe succeeds.
func plantVenv(t *testing.T, home string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the shell stub is not an interpreter on Windows")
	}
	dir := filepath.Dir(venvPython(home))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(venvPython(home), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestVenvPathsAndScriptFailures(t *testing.T) {
	home := "/home/x/.factor"
	if !strings.HasPrefix(venvPip(home), VenvDir(home)) || !strings.Contains(venvPip(home), "pip") {
		t.Errorf("venvPip = %q", venvPip(home))
	}
	if !strings.Contains(venvPython(home), "python") {
		t.Errorf("venvPython = %q", venvPython(home))
	}
	if ScriptPath(home) != filepath.Join(home, "layaserve.py") {
		t.Errorf("ScriptPath = %q", ScriptPath(home))
	}
	// A path that cannot be written is reported rather than swallowed: the
	// supervisor turns it into the reason the model is down.
	taken := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(taken, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteScript(filepath.Join(taken, "layaserve.py")); err == nil {
		t.Error("writing into a file as if it were a directory succeeded")
	}
}

func TestInterpreterResolution(t *testing.T) {
	// A path is taken as a path.
	bin := filepath.Join(t.TempDir(), "python")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveInterpreter(bin); err != nil || got != bin {
		t.Errorf("got %q err %v", got, err)
	}
	// A directory is not an interpreter.
	if _, err := resolveInterpreter(t.TempDir()); err == nil {
		t.Error("a directory resolved as an interpreter")
	}
	// A bare name is looked up on PATH.
	dir := t.TempDir()
	name := "factor-test-python"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got, err := resolveInterpreter(name); err != nil || got == "" {
		t.Errorf("got %q err %v", got, err)
	}
	if len(pythonCandidates()) == 0 {
		t.Error("no interpreter names to look for")
	}
}

func TestLastLinesKeepsTheTailOfAFailure(t *testing.T) {
	if got := lastLines("a\nb\nc\nd\ne\n", 2); got != "d\ne" {
		t.Errorf("lastLines = %q", got)
	}
	if got := lastLines("only", 5); got != "only" {
		t.Errorf("lastLines = %q", got)
	}
	if got := lastLines("", 3); got != "" {
		t.Errorf("lastLines = %q", got)
	}
}

func TestRunCmdReportsOutputAndFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/sh here")
	}
	out, err := runCmd(context.Background(), []string{"/bin/sh", "-c", "echo hello"})
	if err != nil || !strings.Contains(out, "hello") {
		t.Errorf("out=%q err=%v", out, err)
	}
	if _, err := runCmd(context.Background(), []string{"/bin/sh", "-c", "exit 7"}); err == nil {
		t.Error("a failing command reported success")
	}
}

// The environment the server is born with says no to Hugging Face's usage
// reporting, which a personal agent's sidecar has no business doing.
func TestServerEnvironmentSwitchesOffTelemetry(t *testing.T) {
	env := serverEnv()
	var found bool
	for _, kv := range env {
		if kv == "HF_HUB_DISABLE_TELEMETRY=1" {
			found = true
		}
	}
	if !found {
		t.Error("the server would report usage to Hugging Face")
	}
}

// A model that wedges — its process alive, its answers gone — has to be
// noticed by asking, because waiting on the process would wait forever. It
// is stopped and started again, and while it is down every decision falls
// back instead of spending its whole timeout.
func TestAWedgedModelIsNoticedAndRestarted(t *testing.T) {
	t.Setenv("FACTOR_TEST_LAYA_WEDGE", "2")
	b := backendFor(t, "wedge")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)
	waitFor(t, "the model to become healthy", b.Healthy)

	waitFor(t, "the wedge to be noticed", func() bool { return !b.Healthy() })
	if !strings.Contains(b.Down(), "stopped answering") && !strings.Contains(b.Down(), "exited") {
		t.Errorf("Down() = %q", b.Down())
	}
	// And a decision asked while it is down is an immediate fallback.
	_, err := b.Decide(ctx, &decision.Request{
		Questions: map[string]decision.Question{"x": {Criteria: map[string]any{"a": 1, "b": 2}}},
	})
	if !errors.Is(err, decision.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
}

// An unset probe interval is the production one rather than zero, which
// would spin.
func TestDefaultProbeInterval(t *testing.T) {
	b := New(Config{}, t.TempDir())
	if b.reprobeInterval() != probeEvery {
		t.Errorf("reprobeInterval = %s, want %s", b.reprobeInterval(), probeEvery)
	}
}

// Provision is the gateway's permission, and it starts the install itself
// when the model is missing. Here there is no interpreter, so it fails and
// says so rather than leaving the caller waiting.
func TestProvisionStartsAndReportsTheInstall(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	b := New(Config{Port: freePort(t)}, t.TempDir())
	b.Provision(context.Background())
	if !b.installOK.Load() {
		t.Fatal("Provision did not permit the install")
	}
	// The install runs in the background; it must not wedge the supervisor.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := FindPython(b.home); !ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// An already-installed model is not installed again.
	ready := t.TempDir()
	plantVenv(t, ready)
	b2 := New(Config{Port: freePort(t)}, ready)
	b2.Provision(context.Background())
	if !b2.installOK.Load() {
		t.Error("Provision did not permit an already-installed model")
	}
}

// fakePython stands in for an interpreter: it answers the version check, and
// `-m venv DIR` lays down a virtualenv whose python is itself and whose pip
// records every argument it was given. That recording is the point — what
// Factor asks pip for is what decides whether a user ends up with 600 MB or
// 5.6 GB on their disk.
// shellTools are the commands a stub script needs, resolved before PATH is
// narrowed to the stub itself.
type shellTools struct{ mkdir, cp, chmod string }

func fakePython(t *testing.T) (pathDir, pipLog string, tools shellTools) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stubs are shell scripts")
	}
	// Resolved before PATH is replaced: the stub is the only thing on it
	// afterwards, so everything the script runs has to be named in full.
	tool := func(name string) string {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s is not on this machine: %v", name, err)
		}
		return path
	}
	tools = shellTools{mkdir: tool("mkdir"), cp: tool("cp"), chmod: tool("chmod")}

	dir := t.TempDir()
	pipLog = filepath.Join(dir, "pip.log")
	python := filepath.Join(dir, "python3")
	script := `#!/bin/sh
case "$1" in
  -c) exit 0 ;;
  -m)
    if [ "$2" = "venv" ]; then
      ` + tools.mkdir + ` -p "$3/bin"
      ` + tools.cp + ` "$0" "$3/bin/python"
      printf '#!/bin/sh\necho "$@" >> %s\nexit %s\n' "` + pipLog + `" "$FACTOR_TEST_PIP_EXIT" > "$3/bin/pip"
      ` + tools.chmod + ` +x "$3/bin/pip"
      exit 0
    fi
    ;;
esac
exit 0
`
	if err := os.WriteFile(python, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("FACTOR_TEST_PIP_EXIT", "0")
	return dir, pipLog, tools
}

// fakeOldPython plants the machine Factor's own live box actually is: a
// python3 that is real but too old, and a uv that can fetch a current one.
// uvLog records what uv was asked for; pipLog what the virtualenv it built
// then installed.
func fakeOldPython(t *testing.T) (pathDir, uvLog, pipLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stubs are shell scripts")
	}
	tool := func(name string) string {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s is not on this machine: %v", name, err)
		}
		return path
	}
	mkdir, chmod := tool("mkdir"), tool("chmod")

	dir := t.TempDir()
	uvLog = filepath.Join(dir, "uv.log")
	pipLog = filepath.Join(dir, "pip.log")
	// Real, and too old: it answers the version probe with a refusal.
	if err := os.WriteFile(filepath.Join(dir, "python3"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// uv builds the virtualenv the interpreter could not, seeding it with the
	// pip everything downstream installs through.
	uv := `#!/bin/sh
echo "$@" >> ` + uvLog + `
for arg in "$@"; do target="$arg"; done
` + mkdir + ` -p "$target/bin"
printf '#!/bin/sh\nexit 0\n' > "$target/bin/python"
if [ "$FACTOR_TEST_UV_SEED" != "no" ]; then
  printf '#!/bin/sh\necho "$@" >> %s\nexit 0\n' "` + pipLog + `" > "$target/bin/pip"
  ` + chmod + ` +x "$target/bin/pip"
fi
` + chmod + ` +x "$target/bin/python"
exit ${FACTOR_TEST_UV_EXIT:-0}
`
	if err := os.WriteFile(filepath.Join(dir, "uv"), []byte(uv), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return dir, uvLog, pipLog
}

// A Python too old to build the virtualenv is not a machine that cannot run
// the model. Factor's live box answers python3 with 3.8 and has no package
// manager to raise that with, while uv is already there running the memory
// engine on a 3.12 it fetched itself — so the install asks uv for an
// interpreter rather than reporting that the machine is too old.
func TestUVSuppliesAnInterpreterTheMachineHasNot(t *testing.T) {
	_, uvLog, pipLog := fakeOldPython(t)
	home := t.TempDir()
	var said []string
	path, err := Install(context.Background(), home, "cpu", func(format string, args ...any) {
		said = append(said, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if path != venvPython(home) {
		t.Errorf("path = %q", path)
	}
	asked, err := os.ReadFile(uvLog)
	if err != nil {
		t.Fatalf("uv was never asked for an interpreter: %v", err)
	}
	for _, want := range []string{"venv", "--seed", "--python", UVPython, VenvDir(home)} {
		if !strings.Contains(string(asked), want) {
			t.Errorf("uv was asked %q, want it to carry %q", strings.TrimSpace(string(asked)), want)
		}
	}
	// And the virtualenv uv built is the one the install then fills: torch
	// first, Laya second, exactly as on a machine with its own interpreter.
	log, err := os.ReadFile(pipLog)
	if err != nil {
		t.Fatalf("nothing was installed into the virtualenv uv built: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], TorchCPUIndex) || !strings.Contains(lines[1], PackageSpec) {
		t.Errorf("pip was asked for:\n%s", log)
	}
	// The user is told why a Python is being downloaded, since it is the one
	// step here that takes minutes on a slow line.
	if !strings.Contains(strings.Join(said, "\n"), UVPython) {
		t.Errorf("the install said %q, want the fetched interpreter named", said)
	}
}

// numpy's wheels have targeted SSE4.2 since 2.0, and below that baseline
// importing it is an illegal instruction rather than a slow import — measured
// here, torch runs fine on such a box and Laya dies the moment its import
// reaches numpy. The ceiling therefore rides the same pip command, on the
// machines that need it and nowhere else.
func TestNumpyIsCappedOnCPUsThatCannotRunItsWheels(t *testing.T) {
	for _, old := range []bool{true, false} {
		t.Run(fmt.Sprintf("old=%v", old), func(t *testing.T) {
			_, pipLog, _ := fakePython(t)
			restore := needsNumpyPin
			needsNumpyPin = func() bool { return old }
			t.Cleanup(func() { needsNumpyPin = restore })

			if _, err := Install(context.Background(), t.TempDir(), "cuda", nil); err != nil {
				t.Fatalf("install: %v", err)
			}
			log, err := os.ReadFile(pipLog)
			if err != nil {
				t.Fatal(err)
			}
			// One line: the CUDA device skips the torch step, so what is left
			// is the Laya install the constraint has to ride on.
			line := strings.TrimSpace(string(log))
			if !strings.Contains(line, PackageSpec) {
				t.Fatalf("pip was asked %q", line)
			}
			if got := strings.Contains(line, NumpyConstraint); got != old {
				t.Errorf("pip was asked %q; numpy pinned = %v, want %v", line, got, old)
			}
		})
	}
}

// The two ways the fallback itself fails are reported rather than left to
// surface as a missing file three commands later.
func TestUVFailuresAreReportedWithWhatWasWrong(t *testing.T) {
	_, _, _ = fakeOldPython(t)
	t.Setenv("FACTOR_TEST_UV_EXIT", "1")
	_, err := Install(context.Background(), t.TempDir(), "cpu", nil)
	// Both halves: the interpreter that was rejected, and uv's own failure.
	if err == nil || !strings.Contains(err.Error(), "python3") || !strings.Contains(err.Error(), "uv could not supply one") {
		t.Fatalf("err = %v, want both the rejected interpreter and uv's failure", err)
	}

	// A virtualenv with no pip in it cannot be filled, and says so.
	t.Setenv("FACTOR_TEST_UV_EXIT", "0")
	t.Setenv("FACTOR_TEST_UV_SEED", "no")
	if _, err := Install(context.Background(), t.TempDir(), "cpu", nil); err == nil ||
		!strings.Contains(err.Error(), "without a pip") {
		t.Fatalf("err = %v, want the missing pip named", err)
	}
}

// The install asks for the CPU build of torch before it asks for Laya, which
// is the whole difference between a 600 MB virtualenv and a 5.6 GB one on a
// machine with no GPU.
func TestInstallTakesTheCPUWheelFirst(t *testing.T) {
	_, pipLog, _ := fakePython(t)
	home := t.TempDir()
	var progress []string
	path, err := Install(context.Background(), home, "", func(format string, args ...any) {
		progress = append(progress, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if path != venvPython(home) {
		t.Errorf("path = %q", path)
	}
	log, err := os.ReadFile(pipLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 2 {
		t.Fatalf("pip was called %d times:\n%s", len(lines), log)
	}
	if !strings.Contains(lines[0], TorchCPUIndex) || !strings.Contains(lines[0], "torch") {
		t.Errorf("the first install was not the CPU build of torch: %q", lines[0])
	}
	if !strings.Contains(lines[1], PackageSpec) {
		t.Errorf("the second install was not Laya: %q", lines[1])
	}
	if strings.Contains(lines[1], TorchCPUIndex) {
		t.Errorf("Laya was fetched from the torch index: %q", lines[1])
	}
	if len(progress) == 0 {
		t.Error("the install said nothing while it ran")
	}

	// Asking for a GPU leaves the resolution alone: that machine has the
	// hardware and said so.
	cuda := t.TempDir()
	if _, err := Install(context.Background(), cuda, "cuda", nil); err != nil {
		t.Fatal(err)
	}
	log, _ = os.ReadFile(pipLog)
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n")[2:] {
		if strings.Contains(line, TorchCPUIndex) {
			t.Errorf("a CUDA install still took the CPU index: %q", line)
		}
	}
}

// A pip that fails is reported with the tail of what it said, because that
// is where the reason is.
func TestInstallReportsWhatPipSaid(t *testing.T) {
	_, _, _ = fakePython(t)
	t.Setenv("FACTOR_TEST_PIP_EXIT", "1")
	_, err := Install(context.Background(), t.TempDir(), "cuda", nil)
	if err == nil || !strings.Contains(err.Error(), PackageSpec) {
		t.Fatalf("err = %v, want it to name what it could not install", err)
	}
}

// An install that reports success but leaves nothing importable is a failure,
// not a success: the interpreter probe is what decides.
func TestInstallRefusesAVirtualenvWithoutLaya(t *testing.T) {
	dir, _, tools := fakePython(t)
	// A python whose import probe fails, so FindPython says no afterwards.
	broken := `#!/bin/sh
case "$1" in
  -c) [ "$2" = "import laya" ] && exit 1 ; exit 0 ;;
  -m) if [ "$2" = "venv" ]; then ` + tools.mkdir + ` -p "$3/bin"; ` + tools.cp + ` "$0" "$3/bin/python"; printf '#!/bin/sh\nexit 0\n' > "$3/bin/pip"; ` + tools.chmod + ` +x "$3/bin/pip"; exit 0; fi ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "python3"), []byte(broken), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Install(context.Background(), t.TempDir(), "cuda", nil)
	if err == nil || !strings.Contains(err.Error(), "importable") {
		t.Errorf("err = %v, want the missing import reported", err)
	}
}

// The adoption path: a model already answering on the port is used as it is,
// never started a second time, and is re-probed for as long as it serves.
// This is what lets somebody run Laya themselves, and what the app tests
// stand a fake on.
func TestAnAlreadyRunningModelIsAdoptedAndWatched(t *testing.T) {
	port := freePort(t)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("the port was taken: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "max_len": 1024, "head_max_len": 192})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()

	// No interpreter and no install allowed: if it tried to start one of its
	// own, it could not, and the test would fail rather than pass quietly.
	no := false
	b := New(Config{Port: port, AutoInstall: &no}, t.TempDir())
	b.probeInterval = 30 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)
	t.Cleanup(b.Stop)
	waitFor(t, "the running model to be adopted", b.Healthy)
	if l := b.Limits(); l.MaxCandidates != 16 {
		t.Errorf("limits = %+v", l)
	}

	// Take it away: the poll notices, and the supervisor goes back to trying.
	_ = srv.Close()
	waitFor(t, "the model to be noticed gone", func() bool { return !b.Healthy() })
}
