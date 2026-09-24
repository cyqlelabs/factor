//go:build !linux

package local

// hasAVX2 is unknown off Linux, and unknown runs Laya.
var hasAVX2 = func() bool { return true }
