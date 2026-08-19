//go:build linux || windows

package tray

import (
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"

	"fyne.io/systray"
)

// started remembers whether the systray loop was really entered: its Quit
// must not be called for a loop that never ran.
var started atomic.Bool

// Run shows the icon and blocks until Quit is called; onQuit runs when the
// user picks "Quit Factor" from the menu. Where no session can carry an icon
// — a headless box, an ssh login — it returns at once and the gateway simply
// runs without one.
func Run(version string, onQuit func()) {
	if !canTray() {
		return
	}
	// fyne.io/systray v1.12.2 panics on its way out when the D-Bus connect
	// failed at startup (nativeEnd closes a nil conn). A session advertising
	// a dead bus must cost the gateway its icon, not its clean shutdown.
	defer func() { _ = recover() }()
	started.Store(true)
	systray.Run(func() {
		systray.SetIcon(icon())
		systray.SetTooltip("Factor " + version + " — running")
		quit := systray.AddMenuItem("Quit Factor", "Stop the Factor gateway")
		go func() {
			<-quit.ClickedCh
			onQuit()
		}()
	}, func() {})
}

// Quit ends Run, which the gateway's own shutdown triggers so the icon never
// outlives the daemon it stands for.
func Quit() {
	if started.Load() {
		systray.Quit()
	}
}

// canTray reports whether this session can carry an icon at all: Windows
// always runs a shell, Linux needs a session bus for the StatusNotifierItem
// registration. No StatusNotifier host on that bus is fine — systray idles
// and registers the moment one appears — but no bus means nothing to idle on.
func canTray() bool {
	if runtime.GOOS == "windows" {
		return true
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		return true
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "bus"))
	return err == nil
}
