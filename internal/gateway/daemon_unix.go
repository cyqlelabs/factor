//go:build unix

package gateway

import (
	"os/exec"
	"syscall"
)

// detach gives the child its own session, so closing the terminal that ran
// `factor gateway -d` does not take the daemon with it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// hideConsole has nothing to hide: a unix terminal is not opened for a
// program the way a Windows console is.
func hideConsole() {}
