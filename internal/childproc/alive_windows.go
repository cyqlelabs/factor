//go:build windows

package childproc

import (
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Alive reports whether a process with this pid is running.
//
// os.FindProcess is not that question on Windows. It opens a handle, and a
// handle outlives the process it names: one held anywhere — by this process,
// by a debugger, by a parent that has not waited — keeps the object answering
// long after the program behind it exited. So an exited engine read as alive,
// and the loop that waits for a stopped process to go away never ended. The
// wait is the answer: a live process never signals, so a zero-length wait that
// times out is the one state that means running.
func Alive(pid int) bool {
	h, err := open(pid)
	if err != nil {
		// A process another account owns, or one Windows protects, refuses the
		// handle rather than denying it exists. Reading that as dead is how a
		// second gateway starts beside an elevated one.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	state, err := windows.WaitForSingleObject(h, 0)
	return err == nil && state == uint32(windows.WAIT_TIMEOUT)
}

func open(pid int) (windows.Handle, error) {
	if pid <= 0 {
		return 0, windows.ERROR_INVALID_PARAMETER
	}
	return windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
}

// ImageName reports the program the process at pid is running, as a bare file
// name, and whether that could be read at all.
//
// A pid on its own is not an identity. Windows hands them out in small
// multiples of four and recycles them quickly, so a pid file that outlived a
// crash names somebody else's process within hours of a reboot — and the
// callers here do not merely report on that pid, they refuse to start beside
// it and they terminate it. The image name is what makes the pid a claim that
// can be checked. A process this one cannot open reads as unknown rather than
// as a mismatch: refusing to act on a process we cannot see is not the same as
// deciding it is the wrong one.
func ImageName(pid int) (string, bool) {
	h, err := open(pid)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", false
	}
	return filepath.Base(windows.UTF16ToString(buf[:size])), true
}

// IsImage reports whether the process at pid runs the named program, and says
// yes whenever it cannot tell.
func IsImage(pid int, name string) bool {
	got, ok := ImageName(pid)
	return !ok || strings.EqualFold(got, name)
}
