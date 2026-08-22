//go:build !windows

package desktop

// underWine is only ever true in a Windows build running on Wine.
func underWine() bool { return false }
