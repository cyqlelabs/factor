package config

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sort"
	"time"
)

// The config watcher is what makes editing config.json enough: the gateway
// holds its configuration immutable in memory (concurrent turns read it
// lock-free), so a change on disk cannot be patched in — instead the watcher
// notices it and the caller reloads the process the way an upgrade does. An
// mtime poll, like the prompt cache and the release watcher, because a
// dependency-free stat every few seconds is all this needs.

// Watch polls the file baseline was loaded from and calls onChange with the
// freshly loaded configuration — and the names of the sections that differ —
// whenever its content changes into something loadable. A file that cannot
// be loaded (a half-saved edit, a syntax error) is warned about once and
// retried on the next tick, so a bad save never takes the running config
// down. Blocks until ctx ends; run it on its own goroutine.
func Watch(ctx context.Context, baseline *Config, every time.Duration, onChange func(*Config, []string)) {
	path := baseline.Path()
	// last starts unset, so the first tick loads and compares content no
	// matter when the file last changed: an edit racing the watcher's start
	// is still an edit, and the baseline config is what it is compared to.
	var last, warned stamp
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		current := fileStamp(path)
		if current == last {
			continue
		}
		next, err := Load(path)
		if err != nil {
			if current != warned {
				warned = current
				slog.Warn("the config on disk cannot be loaded; keeping the running one", "error", err)
			}
			continue
		}
		last = current
		sections := ChangedSections(baseline, next)
		if len(sections) == 0 {
			continue
		}
		baseline = next
		onChange(next, sections)
	}
}

// stamp is what "the file changed" means to the poller.
type stamp struct {
	modTime int64
	size    int64
	missing bool
}

func fileStamp(path string) stamp {
	info, err := os.Stat(path)
	if err != nil {
		return stamp{missing: true}
	}
	return stamp{modTime: info.ModTime().UnixNano(), size: info.Size()}
}

// ChangedSections names the top-level sections whose content differs —
// "provider", "channels.voice" — which is what the reload log and the
// "config change applied" notice report back.
func ChangedSections(a, b *Config) []string {
	before := sectionsOf(a)
	after := sectionsOf(b)
	var changed []string
	for key := range union(before, after) {
		if key == "channels" {
			continue // named per channel below
		}
		if !bytes.Equal(before[key], after[key]) {
			changed = append(changed, key)
		}
	}
	beforeChannels := channelsOf(before["channels"])
	afterChannels := channelsOf(after["channels"])
	for name := range union(beforeChannels, afterChannels) {
		if !bytes.Equal(beforeChannels[name], afterChannels[name]) {
			changed = append(changed, "channels."+name)
		}
	}
	sort.Strings(changed)
	return changed
}

func sectionsOf(c *Config) map[string]json.RawMessage {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	var sections map[string]json.RawMessage
	_ = json.Unmarshal(raw, &sections)
	return sections
}

func channelsOf(raw json.RawMessage) map[string]json.RawMessage {
	var channels map[string]json.RawMessage
	_ = json.Unmarshal(raw, &channels)
	return channels
}

func union(a, b map[string]json.RawMessage) map[string]struct{} {
	keys := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	return keys
}
