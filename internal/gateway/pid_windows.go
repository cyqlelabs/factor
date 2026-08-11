//go:build windows

package gateway

import "os"

func pidAlive(pid int) bool {
	_, err := os.FindProcess(pid)
	return err == nil
}
