//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes the command a process-group leader and kills the
// whole group on cancel, so grandchildren die with the shell.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
