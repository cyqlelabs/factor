//go:build windows

package upgrade

import (
	"os"
	"os/exec"
)

// Relaunch starts the binary now on disk and leaves this process to exit.
// Windows has no exec, so the new Factor is a new pid: a service manager
// sees the old one stop and is expected to be the thing that starts it.
func Relaunch() error {
	exe, err := selfPath()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Start()
}
