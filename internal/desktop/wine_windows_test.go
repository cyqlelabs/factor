//go:build windows

package desktop

import "golang.org/x/sys/windows"

// underWine reports whether this process is running on Wine rather than on
// Windows. Wine ships the helper binaries the probe looks for — a PowerShell
// stub among them — while having no desktop behind them: no screen to
// measure, no clipboard service, nothing to capture. The live test would
// therefore fail on a machine that simply cannot host it, which is the same
// reason the X11 live test skips without an X server.
func underWine() bool {
	ntdll, err := windows.LoadLibrary("ntdll.dll")
	if err != nil {
		return false
	}
	defer windows.FreeLibrary(ntdll)
	_, err = windows.GetProcAddress(ntdll, "wine_get_version")
	return err == nil
}
