//go:build unix

package childproc

import "syscall"

// Alive reports whether a process with this pid is running. Signal 0 is the
// question without the signal: it succeeds for a live process, and reports
// EPERM rather than ESRCH for one this user may not signal, which is alive.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
