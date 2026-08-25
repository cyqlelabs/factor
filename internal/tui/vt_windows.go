//go:build windows

package tui

import (
	"os"

	"golang.org/x/sys/windows"
)

// EnableVT asks the Windows console behind f to interpret VT escape
// sequences. Windows Terminal has them on by default, but the classic
// console host — what a double-clicked .exe opens — prints them literally
// until this flag is set, and x/term's MakeRaw only configures the input
// half of the console. Reports whether escapes will be understood; false
// means styled output must stay plain.
func EnableVT(f *os.File) bool {
	handle := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
