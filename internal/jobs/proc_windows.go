//go:build windows

package jobs

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

// shellCommand runs a job's payload through cmd.exe. The command line is
// handed over verbatim so a payload carrying its own quotes survives; see
// tools.shellCommand for why Args cannot express that.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	shell := os.Getenv("COMSPEC")
	if shell == "" {
		shell = `C:\Windows\System32\cmd.exe`
	}
	cmd := exec.CommandContext(ctx, shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       syscall.EscapeArg(shell) + ` /d /s /c "` + command + `"`,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	return cmd
}

// setProcessGroup kills the job's whole process tree on cancel; Windows has no
// process group to signal, so taskkill walks it.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		kill.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := kill.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
