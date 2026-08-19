//go:build windows

package autostart

import (
	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// installWindows records the gateway under the user's Run key. Nothing
// supervises a Run entry, so the gateway detaches itself with -d.
func installWindows(exe, configPath string) (Entry, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return Entry{}, err
	}
	defer key.Close()
	if err := key.SetStringValue("Factor", gatewayCommand(exe, configPath, true)); err != nil {
		return Entry{}, err
	}
	return Entry{Mechanism: "registry Run key", Path: `HKCU\` + runKey + `\Factor`}, nil
}

func uninstallWindows() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.DeleteValue("Factor"); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

func installedWindows() (Entry, bool) {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return Entry{}, false
	}
	defer key.Close()
	if _, _, err := key.GetStringValue("Factor"); err != nil {
		return Entry{}, false
	}
	return Entry{Mechanism: "registry Run key", Path: `HKCU\` + runKey + `\Factor`}, true
}
