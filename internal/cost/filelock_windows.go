//go:build windows

package cost

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on path, creating it if needed, and gives
// up after within. LockFileEx is mandatory rather than advisory on Windows,
// which is stricter than the unix side and equally correct here: nothing but
// this package opens the lock file.
func lockFile(path string, within time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	handle := windows.Handle(f.Fd())
	deadline := time.Now().Add(within)
	for {
		var overlapped windows.Overlapped
		err = windows.LockFileEx(handle,
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0, 1, 0, &overlapped)
		if err == nil {
			return f, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("waiting for the lock on %s: %w", path, err)
		}
		time.Sleep(lockPoll)
	}
}

func unlockFile(f *os.File) {
	var overlapped windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped)
	_ = f.Close()
}
