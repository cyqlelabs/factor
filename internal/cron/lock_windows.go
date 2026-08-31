//go:build windows

package cron

import (
	"os"

	"golang.org/x/sys/windows"
)

// overlapped is the zero region every lock here covers: one byte from the
// start of the file, which is all an advisory lock needs to name.
func region() *windows.Overlapped { return &windows.Overlapped{} }

func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, region())
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, region())
}
