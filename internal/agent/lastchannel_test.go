package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cyqlelabs/factor/internal/bus"
)

// restarted builds the Loop the next process would build: same FACTOR_HOME,
// nothing carried over in memory.
func restarted(t *testing.T, h *harness) *Loop {
	t.Helper()
	return NewLoop(h.loop.cfg, bus.New(), h.chat, h.registry, h.store, nil, nil)
}

func TestLastChannelSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	h.loop.recordLastChannel(bus.InboundMessage{Channel: "telegram", ChatID: "42"})

	// Heartbeat results, cron replies and the restart notice all go here, and
	// all of them can happen before the user's first message of a run.
	ch, chat, ok := restarted(t, h).LastChannel()
	if !ok || ch != "telegram" || chat != "42" {
		t.Errorf("LastChannel after a restart = %q %q %v", ch, chat, ok)
	}
}

func TestLastChannelIgnoresLocalConversations(t *testing.T) {
	h := newHarness(t)
	h.loop.recordLastChannel(bus.InboundMessage{Channel: "cli", ChatID: "main"})
	h.loop.recordLastChannel(bus.InboundMessage{Channel: "system", ChatID: "heartbeat"})

	if _, err := os.Stat(lastChannelPath()); !os.IsNotExist(err) {
		t.Errorf("a local conversation was recorded as a delivery address: %v", err)
	}
	if _, _, ok := restarted(t, h).LastChannel(); ok {
		t.Error("a restarted loop claimed an address it was never given")
	}
}

func TestLastChannelIgnoresAnUnusableRecord(t *testing.T) {
	h := newHarness(t)
	for _, bad := range []string{"{not json", `{"channel":"cli","chat_id":"main"}`, `{"chat_id":"42"}`} {
		if err := os.WriteFile(lastChannelPath(), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if ch, chat, ok := restarted(t, h).LastChannel(); ok {
			t.Errorf("record %q was used as a delivery address (%q %q)", bad, ch, chat)
		}
	}
}

func TestLastChannelKeptInMemoryWhenTheDiskRefuses(t *testing.T) {
	h := newHarness(t)
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FACTOR_HOME", filepath.Join(blocker, "home"))

	// A home that cannot be written is a lost address after the next restart,
	// never a lost message in this one.
	h.loop.recordLastChannel(bus.InboundMessage{Channel: "telegram", ChatID: "42"})
	if ch, chat, ok := h.loop.LastChannel(); !ok || ch != "telegram" || chat != "42" {
		t.Errorf("LastChannel = %q %q %v after a failed write", ch, chat, ok)
	}
}
