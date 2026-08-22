//go:build windows

package gateway

import (
	"testing"

	"golang.org/x/sys/windows"
)

// Windows has no SIGHUP: notifyReload is a documented no-op there, so the
// reload is asked for by the upgrade tool or a config change instead.
const sighupSupported = false

// stopSelf ends the daemon. Windows never delivers SIGTERM, so the stop comes
// through the same door the tray's quit item uses and lands in the same
// select.
func stopSelf(t *testing.T) {
	t.Helper()
	RequestStop()
}

func hangupSelf(t *testing.T) {
	t.Helper()
	t.Fatal("hangupSelf is unreachable on windows; guard the test with requireSighup")
}

// killProcess ends a child a test spawned. Windows has no signal to send, so
// the process is terminated through a handle.
func killProcess(pid int) {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.TerminateProcess(h, 1)
}
