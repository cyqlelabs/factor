package childproc

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// sleeper starts a child that will not stop on its own.
func sleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	argv := []string{"sleep", "60"}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/c", "timeout", "/t", "60", "/nobreak"}
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		t.Skipf("no %s on this machine", argv[0])
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

func TestStopAndWaitEndsTheProcess(t *testing.T) {
	cmd := sleeper(t)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	start := time.Now()
	_ = StopAndWait(cmd.Process, done, 5*time.Second)
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("stopping took %s: the grace period was waited out", elapsed)
	}
	// A process that took a signal reports Exited() false, so the state
	// existing at all is what says it is over.
	if cmd.ProcessState == nil {
		t.Fatal("the process is still running")
	}
}

// A process that ignores the request is killed rather than waited on forever.
func TestStopAndWaitKillsWhatWillNotStop(t *testing.T) {
	cmd := sleeper(t)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	if err := StopAndWait(cmd.Process, done, 50*time.Millisecond); err == nil {
		// A killed process reports an error; a cooperative one may not.
		if cmd.ProcessState == nil {
			t.Fatal("the process never ended")
		}
	}
}

// Stop must report what it actually did: a caller that waits on a request
// nobody delivered spends the whole grace period for nothing.
func TestStopReportsWhetherItAskedOrKilled(t *testing.T) {
	cmd := sleeper(t)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	graceful := Stop(cmd.Process)
	if graceful != (runtime.GOOS != "windows") {
		t.Fatalf("Stop reported graceful=%v on %s", graceful, runtime.GOOS)
	}
	<-done
}

func TestStopIgnoresANilProcess(t *testing.T) {
	if Stop(nil) {
		t.Fatal("a nil process cannot be asked anything")
	}
	done := make(chan error, 1)
	done <- errors.New("already gone")
	if err := StopAndWait(nil, done, time.Second); err == nil {
		t.Fatal("the wait result should come back untouched")
	}
	_ = os.Getpid()
}

func TestAliveKnowsThisProcess(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatal("this process is running")
	}
	if Alive(0) || Alive(-1) {
		t.Fatal("a pid that cannot exist is not alive")
	}
}

// The whole point of the Windows implementation: a process that has exited
// must read as gone even while something still holds a handle to it.
func TestAliveReportsAnExitedProcess(t *testing.T) {
	cmd := sleeper(t)
	pid := cmd.Process.Pid
	_ = cmd.Process.Kill()
	_ = cmd.Wait() // the handle os/exec holds stays open until the Cmd is done

	deadline := time.Now().Add(5 * time.Second)
	for Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if Alive(pid) {
		t.Fatal("an exited process still reads as alive")
	}
}

func TestImageNameAnswersForThisProcessOrSaysItCannot(t *testing.T) {
	name, ok := ImageName(os.Getpid())
	if runtime.GOOS != "windows" {
		if ok {
			t.Fatalf("ImageName = %q, want unreadable off Windows", name)
		}
		if !IsImage(os.Getpid(), "anything at all") {
			t.Fatal("an unreadable image must not contradict the caller")
		}
		return
	}
	if !ok || name == "" {
		t.Fatal("this process has an image name")
	}
	if !IsImage(os.Getpid(), name) {
		t.Fatalf("IsImage(%q) disagreed with ImageName", name)
	}
	if IsImage(os.Getpid(), "definitely-not-this.exe") {
		t.Fatal("a different program must not match")
	}
}
