//go:build unix

package tools

import (
	"context"
	"os/exec"
	"syscall"
)

// platformDenyPatterns holds the deny patterns that only mean something on
// this OS. The POSIX shell disasters are in defaultDenyPatterns already.
var platformDenyPatterns []string

// shellName is what the tool description calls the shell it runs commands in.
const shellName = "sh -c"

// shellCommand runs a command line through the platform shell.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command)
}

// ownConsole is a Windows concern: elsewhere a dialog's terminal settings
// are its own already.
func ownConsole(*exec.Cmd) {}

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
