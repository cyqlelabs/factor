package gateway

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/cyqlelabs/factor/internal/childproc"
)

// pidAlive reports whether the pid in the pid file is this daemon still
// running. A pid on its own does not say so: a gateway that was killed or lost
// to a power cut leaves its file behind, and the pid in it is handed to
// somebody else soon enough — on Windows within hours of a reboot. Believing
// it refuses the next `factor gateway` with "already running (pid N)" and
// reports a daemon nobody can find, so where the program behind a pid can be
// read, it is asked before the pid is taken as a claim on this one.
func pidAlive(pid int) bool {
	if !childproc.Alive(pid) {
		return false
	}
	name, ok := childproc.ImageName(pid)
	if !ok {
		return true
	}
	self, err := os.Executable()
	if err != nil {
		return true
	}
	return sameBinary(name, filepath.Base(self))
}

// sameBinary compares two executable names, ignoring what an upgrade leaves
// behind. Installing a new Factor renames the running one aside, so between
// the swap and the reload the live gateway is executing a file called
// factor.exe.old while the terminal asking after it is factor.exe — and
// reading that as a different program would start a second gateway on top of
// the first.
func sameBinary(running, want string) bool {
	trim := func(s string) string {
		if i := strings.Index(strings.ToLower(s), ".old"); i >= 0 {
			return s[:i]
		}
		return s
	}
	return strings.EqualFold(trim(running), trim(want))
}
