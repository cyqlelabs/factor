//go:build windows

package jobs

import "os/exec"

func setProcessGroup(_ *exec.Cmd) {}
