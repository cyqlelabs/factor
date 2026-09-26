package memory

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSettingsDriftNamesOnlyTheEngineSettingsThatChanged(t *testing.T) {
	have := engineSettings(parseEnviron([]string{
		"HOME=/home/x", "SMRTI_DB=/a", "SMRTI_API_KEY=old", "SMRTI_DECISIONS_TIMEOUT=4", "ORT_DISABLE_TELEMETRY=1", "junk",
	}))
	want := engineSettings(parseEnviron([]string{
		"HOME=/home/y", "PATH=/bin", "SMRTI_DB=/a", "SMRTI_API_KEY=new", "SMRTI_DECISIONS_URL=http://127.0.0.1:8731", "ORT_DISABLE_TELEMETRY=1",
	}))
	got := settingsDrift(want, have)
	exp := []string{"SMRTI_API_KEY", "SMRTI_DECISIONS_TIMEOUT", "SMRTI_DECISIONS_URL"}
	if !reflect.DeepEqual(got, exp) {
		t.Fatalf("drift = %v, want %v", got, exp)
	}
	if d := settingsDrift(want, want); len(d) != 0 {
		t.Fatalf("identical settings drift: %v", d)
	}
}

func TestEnvOfReadsThisProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		if _, ok := envOf(1); ok {
			t.Fatal("environments are only readable on linux")
		}
		return
	}
	env, ok := envOf(os.Getpid())
	if !ok || env["PATH"] != os.Getenv("PATH") {
		t.Fatalf("could not read this process's environment: %v", ok)
	}
	if _, ok := envOf(0); ok {
		t.Fatal("/proc/0 is nobody")
	}
}

// The supervisor restarts an adopted engine that was started on settings
// this binary would no longer hand it — the shape every upgrade leaves,
// since the outgoing binary respawns the engine before the new one execs.
func TestSupervisorRestartsAnAdoptedEngineOnStaleSettings(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("engine environments are read from /proc")
	}
	cfg := sidecarConfig(t, "serve")
	cfg.KeepAlive = true
	previous := &Sidecar{
		client:        NewClient(fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port), "", ""),
		cfg:           cfg,
		extract:       ExtractSettings{Mode: "local"},
		logDir:        t.TempDir(),
		probeInterval: 100 * time.Millisecond,
	}
	previous.start(context.Background())
	var eng Engine = previous
	if !waitHealthy(t, eng, 20*time.Second) {
		t.Fatal("engine never became healthy")
	}
	first, ok := readEnginePid()
	if !ok {
		t.Fatal("no engine pid recorded")
	}
	_ = eng.Close() // keep_alive: the engine stays up for the next Factor

	cfg.KeepAlive = false
	next := &Sidecar{
		client: NewClient(fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port), "", ""),
		cfg:    cfg,
		extract: ExtractSettings{
			Mode: "local", DecisionURL: "http://127.0.0.1:8731", DecisionTimeout: 4 * time.Second,
		},
		logDir:        t.TempDir(),
		probeInterval: 100 * time.Millisecond,
	}
	WatchSettings(next)
	next.start(context.Background())
	eng = next
	defer func() { _ = eng.Close() }()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if pid, ok := readEnginePid(); ok && pid != first && eng.Healthy() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	second, ok := readEnginePid()
	if !ok || second == first {
		t.Fatalf("the engine was not restarted for its settings: pid %d → %d", first, second)
	}
	env, ok := processEnv(second)
	if !ok || env["SMRTI_DECISIONS_URL"] != "http://127.0.0.1:8731" || env["SMRTI_DECISIONS_TIMEOUT"] != "4" {
		t.Fatalf("the replacement was not started on the new settings: %v", engineSettings(env))
	}
}

// portHolder starts a child that holds a port the way an engine does, and
// reports its pid and port.
func portHolder(t *testing.T) (int, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "FACTOR_TEST_SMRTI_MODE=listen")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("the child never reported its port: %v", err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid, port
}

// Left alone: an engine somebody else runs, a supervisor that is not the
// gateway, an engine whose environment cannot be read, one already checked,
// one that matches, one serving a turn, and one inside the restart gap.
func TestSettingsRestartIsHeldBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("port ownership of a test child")
	}
	cfg := sidecarConfig(t, "serve")
	_, cfg.Port = portHolder(t)
	saved, savedStop := processEnv, stopEngineIf
	t.Cleanup(func() { processEnv, stopEngineIf = saved, savedStop })
	stopEngineIf = func(context.Context, int, int) (int, error) {
		t.Fatal("stopped an engine it should have left alone")
		return 0, nil
	}
	mk := func(external bool) *Sidecar {
		s := &Sidecar{client: NewClient("http://127.0.0.1:1", "", ""), cfg: cfg, external: external}
		WatchSettings(s)
		return s
	}
	stale := func(int) (map[string]string, bool) { return map[string]string{"SMRTI_DB": "elsewhere"}, true }
	processEnv = stale
	if mk(true).restartForSettings(context.Background()) {
		t.Error("restarted an engine somebody else runs")
	}
	terminal := &Sidecar{client: NewClient("http://127.0.0.1:1", "", ""), cfg: cfg}
	if terminal.restartForSettings(context.Background()) {
		t.Error("a terminal session restarted the engine")
	}
	processEnv = func(int) (map[string]string, bool) { return nil, false }
	unreadable := mk(false)
	if unreadable.restartForSettings(context.Background()) {
		t.Error("restarted an engine whose environment cannot be read")
	}
	processEnv = stale
	if unreadable.restartForSettings(context.Background()) {
		t.Error("asked again about an engine already checked")
	}
	matching := mk(false)
	processEnv = func(int) (map[string]string, bool) { return parseEnviron(matching.buildEnv()), true }
	if matching.restartForSettings(context.Background()) {
		t.Error("restarted an engine on matching settings")
	}
	processEnv = stale
	busy := mk(false)
	busy.client.activity()() // a request just finished: the graph is not quiet
	if busy.restartForSettings(context.Background()) {
		t.Error("restarted an engine while a turn was using it")
	}
	recent := mk(false)
	recent.lastSettingsRestart.Store(time.Now().UnixNano())
	if recent.restartForSettings(context.Background()) {
		t.Error("restarted again inside the gap")
	}
}
