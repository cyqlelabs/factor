//go:build !windows

package cost

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive lock on path, creating it if needed, and gives
// up after within. The lock is advisory and only holds against other holders
// of the same lock — which is every Factor process, since they all come
// through here.
func lockFile(path string, within time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(within)
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
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
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	_ = f.Close()
}
