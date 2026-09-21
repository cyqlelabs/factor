//go:build linux

package childproc

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
)

// Alive reports whether pid is a process that is still running.
//
// A zombie is not. It has exited and is waiting to be reaped, and it answers
// kill(0) exactly as a live process does — which is how a stop that polls for
// the process to go away spends its whole grace period on one that died in
// milliseconds. Measured on the live box: smrti exits 215 ms after SIGTERM,
// and the supervisor went on waiting for it for the full thirty seconds
// before killing something that was already gone, with every recall failing
// meanwhile. /proc has the answer and costs one read.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil && err != syscall.EPERM {
		return false
	}
	return !zombie(pid)
}

// zombie reads the process state out of /proc. The state follows the command
// name, which is parenthesised and may itself contain spaces and brackets, so
// it is found from the last ')' rather than by splitting on spaces.
func zombie(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false // unreadable is not evidence of anything
	}
	close := bytes.LastIndexByte(raw, ')')
	if close < 0 || close+2 >= len(raw) {
		return false
	}
	return raw[close+2] == 'Z'
}
