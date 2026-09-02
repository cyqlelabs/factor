package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// The address the user last wrote from is only an address while a connector
// is running for it. A gateway started with that channel switched off must
// not hand it out: everything downstream would report a delivery that the
// outbound pump then drops.
func TestLastChannelWithholdsAChannelNothingServes(t *testing.T) {
	h := newHarness(t)
	h.loop.recordLastChannel(bus.InboundMessage{Channel: "telegram", ChatID: "42"})

	running := map[string]bool{"voice": true}
	h.loop.SetReachable(func(name string) bool { return running[name] })
	if ch, chat, ok := h.loop.LastChannel(); ok {
		t.Errorf("LastChannel = %q %q while no connector serves it", ch, chat)
	}

	running["telegram"] = true
	if ch, chat, ok := h.loop.LastChannel(); !ok || ch != "telegram" || chat != "42" {
		t.Errorf("LastChannel = %q %q %v once its connector is up", ch, chat, ok)
	}
}

// A spoken conversation is the user's last chat as much as a written one:
// the heartbeat and the cron results follow them there. The terminal is not,
// since nothing proactive can be delivered to a one-shot that has exited.
func TestDirectTurnsMoveTheLastChat(t *testing.T) {
	h := newHarness(t, final("hola"), final("hola"))
	h.loop.recordLastChannel(bus.InboundMessage{Channel: "telegram", ChatID: "42"})

	if _, err := h.loop.ProcessDirectNotice(context.Background(), "hola", "voice:local", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if ch, chat, ok := h.loop.LastChannel(); !ok || ch != "voice" || chat != "local" {
		t.Errorf("LastChannel after a spoken turn = %q %q %v, want voice local", ch, chat, ok)
	}
	if _, err := h.loop.ProcessDirect(context.Background(), "hola", "cli:main"); err != nil {
		t.Fatal(err)
	}
	if ch, chat, ok := h.loop.LastChannel(); !ok || ch != "voice" || chat != "local" {
		t.Errorf("LastChannel after a terminal turn = %q %q %v, want voice local still", ch, chat, ok)
	}
}

// The heartbeat has no user message to take the medium or the language from,
// so it is briefed for the chat its reply is delivered to: the last one the
// user used, in the language the voice there speaks. With no such chat there
// is nothing to brief for.
func TestEphemeralTurnIsBriefedForTheChatItLandsIn(t *testing.T) {
	spanish := func(name string) string {
		if name == "voice" {
			return "es"
		}
		return ""
	}

	h := newHarness(t, final("HEARTBEAT_OK"))
	h.loop.SetLanguage(spanish)
	if _, err := h.loop.ProcessEphemeral(context.Background(), "# Heartbeat\ncheck"); err != nil {
		t.Fatal(err)
	}
	if block := turnBlock(t, h.chat.requests[0].Messages); strings.Contains(block, "spoken aloud") {
		t.Errorf("a heartbeat with nowhere to go was briefed for the speakers: %q", block)
	}

	h = newHarness(t, final("HEARTBEAT_OK"))
	h.loop.SetLanguage(spanish)
	h.loop.recordLastChannel(bus.InboundMessage{Channel: "voice", ChatID: "local"})
	if _, err := h.loop.ProcessEphemeral(context.Background(), "# Heartbeat\ncheck"); err != nil {
		t.Fatal(err)
	}
	block := turnBlock(t, h.chat.requests[0].Messages)
	for _, want := range []string{"spoken aloud on the user's speakers", `code "es"`} {
		if !strings.Contains(block, want) {
			t.Errorf("the heartbeat headed for the speakers was not told %q: %q", want, block)
		}
	}
}

// A scheduled task runs under its own cron session and reports to a chat the
// user reads; the reply is composed for that chat.
func TestScheduledTurnIsBriefedForItsOutlet(t *testing.T) {
	h := newHarness(t, final("listo"))
	h.loop.SetLanguage(func(name string) string { return map[string]string{"voice": "es"}[name] })
	if _, err := h.loop.ProcessScheduled(context.Background(), "recordame la pastilla", "cron:j1", "voice"); err != nil {
		t.Fatal(err)
	}
	block := turnBlock(t, h.chat.requests[0].Messages)
	for _, want := range []string{"nobody watching", "spoken aloud", `code "es"`} {
		if !strings.Contains(block, want) {
			t.Errorf("the scheduled turn headed for the speakers was not told %q: %q", want, block)
		}
	}
}
