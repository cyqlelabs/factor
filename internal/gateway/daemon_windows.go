//go:build windows

package gateway

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach keeps the child off this console: no window of its own, and no
// Ctrl-C aimed at the parent's console group reaching it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
	}
}
