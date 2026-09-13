//go:build unix

package childproc

import (
	"os"
	"syscall"
)

// Stop asks the process to shut down cleanly and reports whether the request
// was delivered, so the caller knows there is something to wait for.
func Stop(p *os.Process) bool {
	if p == nil {
		return false
	}
	return p.Signal(syscall.SIGTERM) == nil
}
