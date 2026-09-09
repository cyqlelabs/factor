package memory

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// hangingEngine starts a process that never exits on its own, the way a served
// engine does, and records it as the engine Factor spawned.
func hangingEngine(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "FACTOR_TEST_SMRTI_MODE=hang")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }() // reaped, so the pid stops existing
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	writeEnginePid(cmd.Process.Pid)
	return cmd.Process.Pid
}

func TestStopEngineStopsTheEngineFactorSpawned(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	prev := engineStopWait
	engineStopWait = 5 * time.Second
	t.Cleanup(func() { engineStopWait = prev })

	// Nothing recorded: nothing to stop, and that is an answer rather than a
	// failure — the next start is what loads the new code.
	stopped, err := StopEngine(context.Background(), 0)
	if stopped != 0 || err != nil {
		t.Fatalf("stopped = %v, err = %v", stopped, err)
	}

	pid := hangingEngine(t)
	stopped, err = StopEngine(context.Background(), 0)
	if stopped != pid || err != nil {
		t.Fatalf("stopped = %v, want %d, err = %v", stopped, pid, err)
	}
	if runtime.GOOS != "windows" && pidAlive(pid) {
		t.Error("the engine is still running")
	}
	if _, err := os.Stat(enginePidPath()); !os.IsNotExist(err) {
		t.Errorf("the pid file outlived the engine: %v", err)
	}
}

func TestStopEngineIgnoresAPidThatIsGone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows cannot tell a live pid from a dead one")
	}
	t.Setenv("FACTOR_HOME", t.TempDir())

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "FACTOR_TEST_SMRTI_MODE=exit")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	writeEnginePid(pid)

	stopped, err := StopEngine(context.Background(), 0)
	if stopped != 0 || err != nil {
		t.Fatalf("a pid file left by an engine that died stops nothing: %v, %v", stopped, err)
	}
}

func TestStopEngineIgnoresAnUnreadablePidFile(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	if err := os.WriteFile(enginePidPath(), []byte("not a pid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stopped, err := StopEngine(context.Background(), 0); stopped != 0 || err != nil {
		t.Fatalf("stopped = %v, err = %v", stopped, err)
	}
}

func TestClearEnginePidOnlyForgetsTheEngineItNames(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	pid := hangingEngine(t)

	// A supervisor that has already recorded the replacement must keep it.
	clearEnginePid(pid + 100000)
	if recorded, _ := readEnginePid(); recorded != pid {
		t.Fatalf("the running engine was forgotten: %d", recorded)
	}
	clearEnginePid(pid)
	if _, err := os.Stat(enginePidPath()); !os.IsNotExist(err) {
		t.Errorf("the pid file survived the engine it named: %v", err)
	}
}

func TestListenerPidNamesTheProcessOnThePort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	pid, ok := ListenerPid(port)
	if !ok || pid != os.Getpid() {
		t.Fatalf("ListenerPid(%d) = %d, %v; want this process (%d)", port, pid, ok, os.Getpid())
	}
	if _, ok := ListenerPid(freePort(t)); ok {
		t.Error("a port nothing listens on named a process")
	}
	if _, ok := ListenerPid(0); ok {
		t.Error("port 0 named a process")
	}
}

// The engine that matters is the one on the port, whoever started it: an
// engine restarted by hand after an upgrade is in no pid file, and leaving it
// alone is how a machine ran two versions behind with the new one installed.
func TestStopEngineStopsAnEngineNobodyRecorded(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	prev := engineStopWait
	engineStopWait = 5 * time.Second
	t.Cleanup(func() { engineStopWait = prev })

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
		t.Fatalf("port line %q: %v", line, err)
	}

	stopped, err := StopEngine(context.Background(), port)
	if err != nil || stopped != cmd.Process.Pid {
		t.Fatalf("stopped = %d, err = %v; want %d", stopped, err, cmd.Process.Pid)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the engine on the port is still running")
	}
}

// EnginePid is how a caller tells a machine whose supervisor put a
// replacement in place from one where nothing will: the exported reading has
// to be the recorded pid and whether that process is still alive, not just
// whether a file exists.
func TestEnginePidReportsTheRecordedEngineAndItsLiveness(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	if pid, alive := EnginePid(); pid != 0 || alive {
		t.Errorf("with no engine recorded: %d, %v", pid, alive)
	}

	writeEnginePid(os.Getpid())
	pid, alive := EnginePid()
	if pid != os.Getpid() || !alive {
		t.Errorf("EnginePid() = %d, %v; want this process, running", pid, alive)
	}

	// A pid nothing is using any more names the engine that was stopped,
	// and says it is gone.
	if runtime.GOOS == "windows" {
		return // windows cannot tell a live pid from a dead one
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "FACTOR_TEST_SMRTI_MODE=exit")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	gone := cmd.Process.Pid
	_ = cmd.Wait()
	writeEnginePid(gone)
	if pid, alive := EnginePid(); pid != gone || alive {
		t.Errorf("for a stopped engine: %d, %v; want %d and not alive", pid, alive, gone)
	}
}

// An engine adopted across an in-place reload is still this process's child,
// and one nothing waits on stays in the process table as a zombie — which
// also answers kill -0, so the stop used to spend its whole grace period on
// a process that had already exited.
func TestStopEngineReapsAnEngineItInherited(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("zombies are a unix notion")
	}
	t.Setenv("FACTOR_HOME", t.TempDir())
	prev := engineStopWait
	engineStopWait = 5 * time.Second
	t.Cleanup(func() { engineStopWait = prev })

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "FACTOR_TEST_SMRTI_MODE=hang")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	// Deliberately no cmd.Wait: this process holds the child the way the
	// gateway holds an engine it inherited across exec, with nothing waiting.
	pid := cmd.Process.Pid
	writeEnginePid(pid)

	started := time.Now()
	stopped, err := StopEngine(context.Background(), 0)
	if err != nil || stopped != pid {
		t.Fatalf("stopped = %d, err = %v; want %d", stopped, err, pid)
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Errorf("the stop waited %s on a process that had exited", took)
	}
	deadline := time.Now().Add(5 * time.Second)
	for pidAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatal("the stopped engine is still in the process table: nothing reaped it")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
