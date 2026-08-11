//go:build windows

package memory

import "os/exec"

func detachSidecar(_ *exec.Cmd) {}
