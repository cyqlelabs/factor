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

// statusLines renders the overview: what is running and for how long, whether
// memory answers, which connectors are up, and what the models have cost —
// the facts that say "everything is fine" or name what is not, one per row.
// An empty spend line is left out rather than shown as zero: nothing counted
// is not the same fact as nothing spent.
func statusLines(version string, up time.Duration, memOn, memHealthy bool, channels []string, spend string) []string {
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
	lines := []string{
		"factor " + version,
		"up " + upWords(up),
		mem,
		chs,
	}
	if spend != "" {
		lines = append(lines, spend)
	}
	return lines
}

// upWords says how long the daemon has been up, at the precision a glance
// wants: seconds only in the first minute, days once hours stop meaning much.
func upWords(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}
