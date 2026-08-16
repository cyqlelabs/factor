package agent

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
)

// Everything Factor says on its own initiative — a heartbeat result, a cron
// job started from the CLI, a new release, a restart notice — goes to the
// chat the user spoke in last, and a process that just started has heard no
// one. Keeping that address on disk is what makes those senders work before
// the first message of a run rather than after it.
func lastChannelPath() string { return filepath.Join(config.Home(), "last-channel.json") }

type lastChannelRecord struct {
	Channel string `json:"channel"`
	ChatID  string `json:"chat_id"`
}

func loadLastChannel() bus.InboundMessage {
	data, err := os.ReadFile(lastChannelPath())
	if err != nil {
		return bus.InboundMessage{}
	}
	var rec lastChannelRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		slog.Warn("ignoring an unreadable last-channel record", "path", lastChannelPath(), "error", err)
		return bus.InboundMessage{}
	}
	if !bus.External(rec.Channel) {
		return bus.InboundMessage{}
	}
	return bus.InboundMessage{Channel: rec.Channel, ChatID: rec.ChatID}
}

func saveLastChannel(msg bus.InboundMessage) {
	data, _ := json.Marshal(lastChannelRecord{Channel: msg.Channel, ChatID: msg.ChatID})
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		slog.Warn("recording the last active chat", "error", err)
		return
	}
	if err := os.WriteFile(lastChannelPath(), data, 0o600); err != nil {
		slog.Warn("recording the last active chat", "error", err)
	}
}
