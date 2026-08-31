package cron

// The cross-process half of the store's locking. jobs.json is one file that a
// gateway, a `factor chat` and a one-shot command all read, change and write
// back whole; this process's mutex says nothing about the other two. Every
// read-modify-write cycle therefore runs between lockStore and its release,
// so a reminder written by one process is never erased by another's save.
//
// The lock is advisory and only this package takes it, which is all that is
// needed: nothing else on the machine writes this file.

import (
	"log/slog"
	"os"
	"time"
)

// How long a cycle waits for another process to finish its own, and how often
// it looks. A scheduler that blocked here forever would stop running the jobs
// it holds because another process wedged, which is the failure this whole
// package exists to prevent — so the wait is bounded and the cycle goes ahead
// unlocked rather than not at all.
var (
	lockWait = 3 * time.Second
	lockPoll = 20 * time.Millisecond
)

// lockStore takes the store's cross-process lock and returns the release. It
// always returns a usable release, so callers need no second code path: when
// the lock could not be taken in time, the release is a no-op and the reason
// is in the log.
func (s *Service) lockStore() (release func()) {
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		slog.Warn("cron store lock could not be opened; writing without it",
			"path", s.path, "error", err)
		return func() {}
	}
	deadline := time.Now().Add(lockWait)
	for {
		if err = lockFile(f); err == nil {
			return func() {
				_ = unlockFile(f)
				_ = f.Close()
			}
		}
		if time.Now().After(deadline) {
			slog.Warn("another process is holding the cron store; writing without the lock",
				"path", s.path, "waited", lockWait, "error", err)
			_ = f.Close()
			return func() {}
		}
		time.Sleep(lockPoll)
	}
}
