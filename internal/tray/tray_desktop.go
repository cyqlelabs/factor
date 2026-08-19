//go:build linux || windows

package tray

import (
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"fyne.io/systray"
)

// started remembers whether the systray loop was really entered: its Quit
// must not be called for a loop that never ran.
var started atomic.Bool

// overviewRows caps the status overview. The rows are created up front and
// hidden while blank, because a menu item added later would land below the
// quit item — so a source reporting more lines than this has the extras
// dropped rather than misplaced.
const overviewRows = 4

// refreshEvery is the floor on staleness for hosts that never report the
// menu opening; an open refreshes immediately.
const refreshEvery = 10 * time.Second

// Run shows the icon and blocks until Quit is called. status feeds the
// overview the menu shows — clicking the icon opens it, and every open
// re-asks so the rows say what is true now, not at startup. onQuit runs when
// the user picks "Quit Factor". Where no session can carry an icon — a
// headless box, an ssh login — Run returns at once and the gateway simply
// runs without one.
func Run(version string, status func() []string, onQuit func()) {
	if !canTray() {
		return
	}
	// fyne.io/systray v1.12.2 panics on its way out when the D-Bus connect
	// failed at startup (nativeEnd closes a nil conn). A session advertising
	// a dead bus must cost the gateway its icon, not its clean shutdown.
	defer func() { _ = recover() }()
	started.Store(true)
	stop := make(chan struct{})
	systray.Run(func() {
		systray.SetIcon(icon())
		systray.SetTooltip("Factor " + version + " — running")
		rows := make([]*systray.MenuItem, overviewRows)
		for i := range rows {
			rows[i] = systray.AddMenuItem("", "")
			rows[i].Disable()
			rows[i].Hide()
		}
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit Factor", "Stop the Factor gateway")
		go func() {
			<-quit.ClickedCh
			onQuit()
		}()
		go refreshOverview(rows, status, stop)
	}, func() { close(stop) })
}

// refreshOverview keeps the status rows current: once at startup, on every
// menu open, and on a slow tick. Only a changed line touches its row, so an
// idle gateway sends no bus traffic.
func refreshOverview(rows []*systray.MenuItem, status func() []string, stop <-chan struct{}) {
	shown := make([]string, len(rows))
	update := func() {
		lines := status()
		for i, row := range rows {
			line := ""
			if i < len(lines) {
				line = lines[i]
			}
			if line == shown[i] {
				continue
			}
			if line == "" {
				row.Hide()
			} else {
				row.SetTitle(line)
				row.Show()
			}
			shown[i] = line
		}
	}
	update()
	tick := time.NewTicker(refreshEvery)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-systray.TrayOpenedCh:
		case <-tick.C:
		}
		update()
	}
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
