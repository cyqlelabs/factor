//go:build unix

package memory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// An installer here is always a wrapper: pipx and uv both shell out to the pip
// that does the downloading, and stopping the wrapper alone leaves that pip
// running — into the same virtualenv the next attempt starts writing to. This
// is what the process group is for, so the test is the grandchild: it must be
// gone once the command's context ends.
func TestStoppingAnInstallerTakesTheProcessBehindItToo(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no shell here: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	// A wrapper that spawns the long-running worker and waits on it, which is
	// the shape of every installer this package runs.
	script := "sh -c 'while :; do sleep 1; done' & echo $! > " + pidFile + "; wait"

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, sh, "-c", script)
	cmd.WaitDelay = 5 * time.Second
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	worker := waitForPid(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(worker, syscall.SIGKILL) })

	cancel()
	_ = cmd.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for alive(worker) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d outlived the installer that started it", worker)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForPid reads the pid the stub wrote once it is there and parsable.
func waitForPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the stub never reported its child")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// alive reports whether a pid still exists. Signal 0 runs every check the
// kernel would run for a real signal and delivers none.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }
