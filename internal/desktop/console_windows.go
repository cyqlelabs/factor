//go:build windows

package desktop

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// ownConsole gives a helper a console of its own, without a window. Every
// PowerShell script switches its console's output code page to UTF-8
// (psUTF8), and a console is shared with whoever started the process: in a
// terminal session that is the user's own, which would be left on UTF-8
// after Factor exits.
func ownConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}
