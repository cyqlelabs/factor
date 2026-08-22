//go:build windows

package upgrade

import (
	"os"
	"os/exec"
)

// A seam: a test cannot spawn a replacement for the process running it.
// Windows has no exec, so the new Factor is a new pid — but the seam keeps
// the same shape as the unix one so the restart path is tested on both.
var execSelf = func(path string, argv, env []string) error {
	cmd := exec.Command(path, argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Start()
}

// Relaunch starts the binary now on disk and leaves this process to exit.
// A service manager sees the old pid stop and is expected to be the thing
// that starts it again.
func Relaunch() error {
	exe, err := selfPath()
	if err != nil {
		return err
	}
	return execSelf(exe, os.Args, os.Environ())
}
