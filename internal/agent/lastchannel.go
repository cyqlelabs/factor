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

// chatAddr is one chat: the channel and the chat on it a message is
// published to.
type chatAddr struct {
	Channel string `json:"channel"`
	ChatID  string `json:"chat_id"`
}

// lastChannelRecord is the last chat the user used, and behind it the last
// chat on every other channel, most recent first: a message the user should
// read looks past a spoken conversation to the last chat they wrote in.
type lastChannelRecord struct {
	chatAddr
	Before []chatAddr `json:"before,omitempty"`
}

func loadLastChannel() (bus.InboundMessage, []chatAddr) {
	data, err := os.ReadFile(lastChannelPath())
	if err != nil {
		return bus.InboundMessage{}, nil
	}
	var rec lastChannelRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		slog.Warn("ignoring an unreadable last-channel record", "path", lastChannelPath(), "error", err)
		return bus.InboundMessage{}, nil
	}
	if !bus.External(rec.Channel) {
		return bus.InboundMessage{}, nil
	}
	chats := []chatAddr{rec.chatAddr}
	for _, c := range rec.Before {
		if bus.External(c.Channel) && c.ChatID != "" {
			chats = withLast(chats, c)
		}
	}
	return bus.InboundMessage{Channel: rec.Channel, ChatID: rec.ChatID}, chats
}

// withFirst puts c at the head of chats, in place of the chat it replaces on
// its channel.
func withFirst(chats []chatAddr, c chatAddr) []chatAddr {
	out := []chatAddr{c}
	for _, old := range chats {
		if old.Channel != c.Channel {
			out = append(out, old)
		}
	}
	return out
}

// withLast appends c unless its channel already has a chat in chats.
func withLast(chats []chatAddr, c chatAddr) []chatAddr {
	for _, old := range chats {
		if old.Channel == c.Channel {
			return chats
		}
	}
	return append(chats, c)
}

func saveLastChannel(chats []chatAddr) {
	data, _ := json.Marshal(lastChannelRecord{chatAddr: chats[0], Before: chats[1:]})
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		slog.Warn("recording the last active chat", "error", err)
		return
	}
	if err := os.WriteFile(lastChannelPath(), data, 0o600); err != nil {
		slog.Warn("recording the last active chat", "error", err)
	}
}
