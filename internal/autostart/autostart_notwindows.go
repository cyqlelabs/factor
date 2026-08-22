//go:build !windows

package autostart

import (
	"errors"
	"fmt"
)

// The registry only exists on Windows; these stubs keep the GOOS dispatch
// compiling everywhere, and are only reachable from a test that fakes
// env.GOOS.
var errNotWindows = errors.New("the Windows registry is not available in this build")

func installWindows(string, string) (Entry, error) { return Entry{}, errNotWindows }
func uninstallWindows() error                      { return errNotWindows }
func installedWindows() (Entry, bool)              { return Entry{}, false }

// quotePath renders a path for a unit's ExecStart or a .desktop Exec line.
// Both treat a backslash inside quotes as an escape, which is exactly what %q
// produces.
func quotePath(p string) string { return fmt.Sprintf("%q", p) }
