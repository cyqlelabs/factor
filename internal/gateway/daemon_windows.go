//go:build windows

package gateway

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// detach keeps the child off this console: no window of its own, and no
// Ctrl-C aimed at the parent's console group reaching it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
	}
}

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	user32                    = windows.NewLazySystemDLL("user32.dll")
	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	procShowWindowAsync       = user32.NewProc("ShowWindowAsync")
)

// swHide is the ShowWindow command that takes a window off the screen.
const swHide = 0

// hideConsole hides the console this process was given, but only if it was
// given one of its own.
//
// factor.exe is a console program, so the login entry starting `factor gateway
// -d` makes Windows open a console for it: a window flashes up at every login
// and stays there until the detached child has claimed the pid file. The same
// command typed into a terminal must keep that terminal, though, and the
// console's process list is what tells the two apart — one process on it means
// the console was made for this program and belongs to nobody else.
func hideConsole() {
	var pids [4]uint32
	count, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if count != 1 {
		return // a shell is on it too, and that window is the user's
	}
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd == 0 {
		return
	}
	_, _, _ = procShowWindowAsync.Call(hwnd, swHide)
}
