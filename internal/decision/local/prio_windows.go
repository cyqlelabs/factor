//go:build windows

package local

import "golang.org/x/sys/windows"

// lowerPriority puts the model below the rest of the machine. Windows sets a
// priority class on a process handle rather than a nice value on a pid, and
// BELOW_NORMAL is the class that leaves the desktop responsive while a
// background job runs flat out. A failure is nothing to report: the model runs
// either way.
func lowerPriority(pid int) {
	h, err := windows.OpenProcess(windows.PROCESS_SET_INFORMATION, false, uint32(pid))
	if err != nil {
		return
	}
	defer func() { _ = windows.CloseHandle(h) }()
	_ = windows.SetPriorityClass(h, windows.BELOW_NORMAL_PRIORITY_CLASS)
}
