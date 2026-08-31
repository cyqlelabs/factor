//go:build unix

package cron

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive advisory lock without waiting. The lock belongs
// to the open file description, so two handles in one process contend exactly
// as two processes do.
func lockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
