//go:build !linux && !windows

package tray

// Run returns at once on platforms whose tray would need cgo (macOS): the
// gateway runs without an icon rather than costing the build its static,
// cross-compiled binary.
func Run(string, func() []string, func()) {}

// Quit has no loop to end.
func Quit() {}
