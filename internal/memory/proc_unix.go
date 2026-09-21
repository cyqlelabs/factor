//go:build unix

package memory

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes an installer a process-group leader and kills the whole
// group when its context ends, so the pip behind it dies with it.
//
// The default is to signal the direct child alone, and every installer here is
// a wrapper around another process: pipx and uv both shell out to a pip that
// does the downloading. Measured on a slow install, killing the wrapper on a
// timeout left that pip running for another twenty minutes, downloading into
// the same virtualenv the next attempt then started writing to — two installers
// in one environment, neither aware of the other, from what looked like one
// abandoned upgrade.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
