package gateway

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// statusSource is set while serve runs, so the tray menu can show what the
// daemon knows about itself. A mutex rather than a channel: the tray asks at
// its own rhythm, and the answer must never block the loop.
var (
	statusMu     sync.Mutex
	statusSource func() []string
)

func setStatusSource(fn func() []string) {
	statusMu.Lock()
	statusSource = fn
	statusMu.Unlock()
}

// StatusLines reports the running daemon's health as tray-menu rows. Before
// serve has come up — the tray starts beside it, not after it — there is only
// the one line to say.
func StatusLines() []string {
	statusMu.Lock()
	fn := statusSource
	statusMu.Unlock()
	if fn == nil {
		return []string{"gateway starting…"}
	}
	return fn()
}

// statusLines renders the overview: what is running, whether memory answers,
// and which connectors are up — the three facts that say "everything is fine"
// or name what is not.
func statusLines(version string, up time.Duration, memOn, memHealthy bool, channels []string) []string {
	mem := "memory: off"
	if memOn {
		if mem = "memory: healthy"; !memHealthy {
			mem = "memory: unreachable"
		}
	}
	chs := "channels: none"
	if len(channels) > 0 {
		chs = "channels: " + strings.Join(channels, ", ")
	}
	return []string{
		fmt.Sprintf("factor %s — up %s", version, upWords(up)),
		mem,
		chs,
	}
}

// upWords says how long the daemon has been up, at the precision a glance
// wants: no seconds ticking by, days once hours stop meaning much.
func upWords(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moments"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}
