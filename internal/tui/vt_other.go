//go:build !windows

package tui

import "os"

// EnableVT reports whether the terminal behind f interprets VT escape
// sequences. Outside Windows every terminal does; the Windows build has to
// ask the console host to switch them on first.
func EnableVT(*os.File) bool { return true }
