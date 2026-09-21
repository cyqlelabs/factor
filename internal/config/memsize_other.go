//go:build !linux

package config

// totalMemoryMB has no answer here, which reads as "use the floor". Reading
// it on macOS and Windows needs a syscall table this package has no other
// use for, and the machines the ceiling was mis-sized for run Linux.
var totalMemoryMB = func() int { return 0 }
