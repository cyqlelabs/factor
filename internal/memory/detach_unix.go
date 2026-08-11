//go:build unix

package memory

import (
	"os/exec"
	"syscall"
)

// detachSidecar puts the sidecar in its own session so it is not killed by
// the terminal or Factor's process group ending.
func detachSidecar(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
