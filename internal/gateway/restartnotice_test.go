package gateway

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/upgrade"
	"github.com/cyqlelabs/factor/internal/version"
)

// collector stands in for the outbound bus.
func collector(sent *[]bus.OutboundMessage) func(bus.OutboundMessage) bool {
	return func(msg bus.OutboundMessage) bool {
		*sent = append(*sent, msg)
		return true
	}
}

func nobodySpoke() (string, string, bool) { return "", "", false }

// serving stands in for the connectors a daemon is actually running.
func serving(names ...string) func(string) bool {
	return func(name string) bool { return slices.Contains(names, name) }
}

func TestRestartReportsBackToTheChatThatAsked(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())

	// The upgrade came from one chat while the user's last message came from
	// another: whoever asked is owed the answer.
	noteRestart(restartRequest{
		reason: "installed factor v9.9.9",
		target: upgrade.Target{Channel: "telegram", ChatID: "42"},
	}, func() (string, string, bool) { return "telegram", "7", true }, serving("telegram"))

	var sent []bus.OutboundMessage
	announceRestart(collector(&sent))
	if len(sent) != 1 {
		t.Fatalf("restart notices sent = %+v", sent)
	}
	if sent[0].Channel != "telegram" || sent[0].ChatID != "42" {
		t.Errorf("notice went to %s:%s", sent[0].Channel, sent[0].ChatID)
	}
	if !strings.Contains(sent[0].Content, version.Version) {
		t.Errorf("notice does not say what is running now: %q", sent[0].Content)
	}

	// Coming back is news exactly once: an ordinary start afterwards is silent.
	sent = nil
	announceRestart(collector(&sent))
	if len(sent) != 0 {
		t.Errorf("a plain start announced a restart: %+v", sent)
	}
}

func TestRestartFollowsTheUserWhenTheAskerCannotBeReached(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	lastChat := func() (string, string, bool) { return "telegram", "7", true }

	// `factor upgrade` in a terminal carries no conversation at all, and a
	// cron session is nobody's inbox: both follow the user to the chat they
	// last used rather than announcing into a void.
	for _, req := range []restartRequest{
		{reason: "SIGHUP"},
		{reason: "installed factor v9.9.9", target: upgrade.Target{Channel: "cron", ChatID: "job-3"}},
	} {
		noteRestart(req, lastChat, serving("telegram"))

		var sent []bus.OutboundMessage
		announceRestart(collector(&sent))
		if len(sent) != 1 || sent[0].Channel != "telegram" || sent[0].ChatID != "7" {
			t.Fatalf("restart notices for %+v = %+v", req, sent)
		}
	}
}

func TestRestartWithNoOneToTellLeavesNoNote(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())

	// A box that has never been spoken to has nowhere to report back to, and
	// a CLI chat is gone with the process that held it.
	noteRestart(restartRequest{reason: "SIGHUP"}, nobodySpoke, serving("telegram"))
	noteRestart(restartRequest{reason: "SIGHUP", target: upgrade.Target{Channel: "cli", ChatID: "main"}},
		nobodySpoke, serving("telegram"))

	if _, err := os.Stat(restartNoticePath()); !os.IsNotExist(err) {
		t.Errorf("a restart with no audience left a note: %v", err)
	}
	var sent []bus.OutboundMessage
	announceRestart(collector(&sent))
	if len(sent) != 0 {
		t.Errorf("announced a restart nobody was waiting for: %+v", sent)
	}
}

// writeNotice plants a notice as some earlier process would have left it.
func writeNotice(t *testing.T, note restartNotice) {
	t.Helper()
	data, err := json.Marshal(note)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(restartNoticePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRestartNoticeNamesBothVersions(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	writeNotice(t, restartNotice{Channel: "telegram", ChatID: "42", From: "v0.0.1", At: time.Now()})

	var sent []bus.OutboundMessage
	announceRestart(collector(&sent))
	if len(sent) != 1 {
		t.Fatalf("restart notices sent = %+v", sent)
	}
	if !strings.Contains(sent[0].Content, "v0.0.1") || !strings.Contains(sent[0].Content, version.Version) {
		t.Errorf("an upgrade restart does not name what changed: %q", sent[0].Content)
	}
}

func TestStaleAndUnusableRestartNoticesAreDropped(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())

	// The machine was off for a week: nobody is still waiting on that restart.
	writeNotice(t, restartNotice{Channel: "telegram", ChatID: "42", At: time.Now().Add(-restartNoticeTTL - time.Minute)})
	var sent []bus.OutboundMessage
	announceRestart(collector(&sent))
	if len(sent) != 0 {
		t.Errorf("a stale notice was delivered: %+v", sent)
	}

	for _, bad := range []string{"{not json", `{"channel":"cli","chat_id":"main"}`} {
		if err := os.WriteFile(restartNoticePath(), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		announceRestart(collector(&sent))
		if len(sent) != 0 {
			t.Errorf("notice %q was delivered: %+v", bad, sent)
		}
	}
}
