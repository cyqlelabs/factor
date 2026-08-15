//go:build unix

package upgrade

import (
	"os"
	"syscall"
)

// A seam: a test cannot exec away the process running it.
var execSelf = syscall.Exec

// Relaunch replaces this process with the binary now on disk, re-running the
// same command line. It is an exec rather than a spawn, so the pid, the open
// file descriptors, and the systemd unit all survive — whatever supervises
// Factor never sees it stop. On success it does not return.
func Relaunch() error {
	exe, err := selfPath()
	if err != nil {
		return err
	}
	return execSelf(exe, os.Args, os.Environ())
}
