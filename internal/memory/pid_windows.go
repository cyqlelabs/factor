//go:build windows

package memory

import (
	"fmt"

	"golang.org/x/sys/windows"

	"github.com/cyqlelabs/factor/internal/childproc"
)

// isEngine guards the two calls that end a process: the pid file naming one
// may have outlived a crash, and a recycled pid would make these the calls
// that kill something else.
func isEngine(pid int) bool {
	return childproc.Alive(pid) && childproc.IsImage(pid, BinaryName())
}

// terminateProcess stops the engine. Windows has no graceful signal for a
// process holding a console of its own, so this is the blunt one — but it is
// aimed carefully: the pid is checked against the engine's image first, since
// the file naming it may have outlived a crash and a recycled pid would make
// this the call that kills something else.
func terminateProcess(pid int) error {
	if !isEngine(pid) {
		return fmt.Errorf("pid %d is not %s any more", pid, BinaryName())
	}
	return terminate(pid)
}

func killProcess(pid int) {
	if isEngine(pid) {
		_ = terminate(pid)
	}
}

func terminate(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}
