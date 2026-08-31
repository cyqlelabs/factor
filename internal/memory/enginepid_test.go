package memory

import (
	"context"
	"os"
	"os/exec"
	"runtime"
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
	stopped, err := StopEngine(context.Background())
	if stopped != 0 || err != nil {
		t.Fatalf("stopped = %v, err = %v", stopped, err)
	}

	pid := hangingEngine(t)
	stopped, err = StopEngine(context.Background())
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

	stopped, err := StopEngine(context.Background())
	if stopped != 0 || err != nil {
		t.Fatalf("a pid file left by an engine that died stops nothing: %v, %v", stopped, err)
	}
}

func TestStopEngineIgnoresAnUnreadablePidFile(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	if err := os.WriteFile(enginePidPath(), []byte("not a pid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stopped, err := StopEngine(context.Background()); stopped != 0 || err != nil {
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
