//go:build windows

package tools

import "os/exec"

func setProcessGroup(_ *exec.Cmd) {}
