//go:build windows

package tray

import "golang.org/x/sys/windows"

// underWine reports whether this process is running on Wine rather than on
// Windows. Wine runs an explorer stub that lets the systray loop start but
// has no shell notification area behind it, so the icon never registers and
// the loop does not end the way it does on a real desktop.
func underWine() bool {
	ntdll, err := windows.LoadLibrary("ntdll.dll")
	if err != nil {
		return false
	}
	defer windows.FreeLibrary(ntdll)
	_, err = windows.GetProcAddress(ntdll, "wine_get_version")
	return err == nil
}
