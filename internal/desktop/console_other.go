//go:build !windows

package desktop

import "os/exec"

// ownConsole is a Windows concern: elsewhere a helper's terminal settings are
// its own already.
func ownConsole(*exec.Cmd) {}
