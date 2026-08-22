//go:build !windows

package tray

// underWine is only ever true in a Windows build running on Wine.
func underWine() bool { return false }
