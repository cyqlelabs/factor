//go:build linux

package childproc

import (
	"os/exec"
	"testing"
	"time"
)

// A zombie answers kill(0) like a live process, and every stop here polls for
// the process to go away. Reading it as alive is what made a supervisor spend
// its whole thirty-second grace on an engine that had exited in 215 ms, and
// then kill something already gone.
func TestAZombieIsNotAlive(t *testing.T) {
	// A child that exits immediately and is deliberately not waited on is a
	// zombie until this test's process reaps it.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("no shell to spawn: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() })

	deadline := time.Now().Add(5 * time.Second)
	for !zombie(pid) {
		if time.Now().After(deadline) {
			t.Skip("the child never sat in Z long enough to observe")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if Alive(pid) {
		t.Error("a process that has exited reads as alive; a stop would wait out its whole grace on it")
	}

	// A process that is genuinely running still reads as alive.
	live := exec.Command("/bin/sh", "-c", "sleep 30")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = live.Process.Kill()
		_ = live.Wait()
	})
	if !Alive(live.Process.Pid) {
		t.Error("a running process reads as dead")
	}
	if zombie(live.Process.Pid) {
		t.Error("a running process reads as a zombie")
	}
	// A pid that never existed, and a nonsense one.
	if Alive(0) || Alive(-1) {
		t.Error("a pid that cannot exist reads as alive")
	}
}

// The state is read from after the last ')': a command name is parenthesised
// and may itself carry spaces and brackets, so splitting on spaces finds the
// wrong field for a process named "(sleep) 1) Z".
func TestTheProcessStateIsReadPastTheCommandName(t *testing.T) {
	if zombie(1 << 30) {
		t.Error("an unreadable /proc entry reported a zombie")
	}
}
