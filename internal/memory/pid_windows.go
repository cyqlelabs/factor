//go:build windows

package memory

import "os"

func pidAlive(pid int) bool {
	// Windows has no signal 0: FindProcess is as far as this goes, and it
	// succeeds for a pid that has already exited.
	_, err := os.FindProcess(pid)
	return err == nil
}

// terminateProcess stops the engine. Windows has no graceful signal to send a
// process that owns no console of ours, so this is the blunt one.
func terminateProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func killProcess(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
