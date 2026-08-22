//go:build unix

package gateway

import (
	"os"
	"syscall"
	"testing"
)

// sighupSupported reports whether this platform can carry a restart request
// from one process to another as a signal.
const sighupSupported = true

// stopSelf ends the daemon the way a service manager does.
func stopSelf(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
}

// hangupSelf asks for a restart the way `factor upgrade` does from a terminal.
func hangupSelf(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
}

// killProcess ends a child a test spawned, whatever state it is in.
func killProcess(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }
