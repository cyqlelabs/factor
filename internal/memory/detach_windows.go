//go:build windows

package memory

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachSidecar gives the engine a console of its own so it outlives the
// terminal Factor was typed into, which is the whole of what keep_alive
// promises. Spawned plainly it shared this process's console: Ctrl-C in
// `factor chat` reached it as a KeyboardInterrupt and closing the window
// killed it outright, so the engine a later run was meant to find warm was
// gone with the shell that started it. CREATE_NO_WINDOW is the console
// nobody sees; CREATE_NEW_PROCESS_GROUP keeps our own Ctrl-C off it.
func detachSidecar(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
	}
}
