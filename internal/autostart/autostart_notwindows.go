//go:build !windows

package autostart

import "errors"

// The registry only exists on Windows; these stubs keep the GOOS dispatch
// compiling everywhere, and are only reachable from a test that fakes
// env.GOOS.
var errNotWindows = errors.New("the Windows registry is not available in this build")

func installWindows(string, string) (Entry, error) { return Entry{}, errNotWindows }
func uninstallWindows() error                      { return errNotWindows }
func installedWindows() (Entry, bool)              { return Entry{}, false }
