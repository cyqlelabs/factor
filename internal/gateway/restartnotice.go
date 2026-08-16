package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/upgrade"
	"github.com/cyqlelabs/factor/internal/version"
)

// restartRequest is one ask to reload: why, and the chat that asked for it.
type restartRequest struct {
	reason string
	target upgrade.Target
}

// A restart leaves a note on disk for the process that comes next. The exec
// keeps the pid and nothing else, so the new binary has no idea it was asked
// to come back, let alone who is waiting to hear that it did — and the user
// who asked over Telegram is owed that line. Written once the conversation
// that asked has been answered, read and deleted on the way up.
func restartNoticePath() string { return filepath.Join(config.Home(), "restart-notice.json") }

type restartNotice struct {
	Channel string    `json:"channel"`
	ChatID  string    `json:"chat_id"`
	Reason  string    `json:"reason"`
	From    string    `json:"from_version"`
	At      time.Time `json:"at"`
}

// How long coming back is still news. A note older than this belongs to a
// restart the user has long stopped waiting on — the machine was off, or the
// exec failed and someone started Factor again days later.
const restartNoticeTTL = 15 * time.Minute

// noteRestart records who to report back to. The chat that asked wins, as
// long as a connector can reach it: a SIGHUP asked from a terminal, and a
// cron job asked from a session that is nobody's inbox. Those follow the
// user to the chat they last used.
func noteRestart(req restartRequest, last func() (string, string, bool), serves func(string) bool) {
	target := req.target
	if !serves(target.Channel) {
		ch, chat, ok := last()
		if !ok {
			slog.Info("restarting with no one to report back to", "reason", req.reason)
			return
		}
		target = upgrade.Target{Channel: ch, ChatID: chat}
	}
	data, _ := json.Marshal(restartNotice{
		Channel: target.Channel,
		ChatID:  target.ChatID,
		Reason:  req.reason,
		From:    version.Version,
		At:      time.Now(),
	})
	if err := os.WriteFile(restartNoticePath(), data, 0o600); err != nil {
		slog.Warn("leaving a restart notice for the next process", "error", err)
	}
}

// announceRestart delivers the note the previous process left behind, and
// clears it either way: being back is news exactly once.
func announceRestart(publish func(bus.OutboundMessage) bool) {
	data, err := os.ReadFile(restartNoticePath())
	if err != nil {
		return // the ordinary start: nobody asked for this one
	}
	if err := os.Remove(restartNoticePath()); err != nil {
		slog.Warn("clearing the restart notice", "error", err)
	}
	var note restartNotice
	if err := json.Unmarshal(data, &note); err != nil {
		slog.Warn("ignoring an unreadable restart notice", "error", err)
		return
	}
	if !bus.External(note.Channel) {
		return
	}
	if age := time.Since(note.At); age > restartNoticeTTL {
		slog.Info("skipping a stale restart notice", "age", age.Round(time.Second), "reason", note.Reason)
		return
	}
	slog.Info("reporting back after a restart", "channel", note.Channel, "reason", note.Reason)
	publish(bus.OutboundMessage{Channel: note.Channel, ChatID: note.ChatID, Content: restartMessage(note)})
}

func restartMessage(note restartNotice) string {
	if note.From != "" && note.From != version.Version {
		return fmt.Sprintf("Back up — factor %s → %s.", note.From, version.Version)
	}
	return fmt.Sprintf("Back up on factor %s.", version.Version)
}
