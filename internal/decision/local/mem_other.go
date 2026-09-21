//go:build !linux

package local

// availableMB has no answer here. Reading free memory on macOS and Windows
// needs either cgo or a syscall table this package has no other use for, and
// the machines the floor exists to protect — a few gigabytes, an old CPU, no
// package manager — run Linux. Unknown reads as "do not refuse", which leaves
// those platforms exactly as they were.
var availableMB = func() (int, bool) { return 0, false }
