//go:build unix

package memory

import "syscall"

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// terminateProcess asks the engine to stop and let its current epoch finish.
func terminateProcess(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func killProcess(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }
