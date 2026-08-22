package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/provider"
)

// This is the whole reason the per-turn block moved out of the system prompt.
//
// A prompt cache reuses the longest byte-identical prefix of a request, so
// wherever the first difference between two turns falls, everything after it
// is prefilled again from scratch. With memory recall inside the system
// message that difference fell in the first few hundred tokens, which put the
// entire conversation behind it — and the cost of that grew with every turn
// the session survived. The recalled memories differ here on purpose: this
// test is only worth anything if the volatile part really is volatile.
func TestTheRequestPrefixSurvivesTheNextTurn(t *testing.T) {
	h := newHarness(t, final("first"), final("second"), final("third"))

	for i, msg := range []string{"hola", "otra cosa", "y otra más"} {
		h.engine.recalls = []memory.Memory{{Content: recallTexts[i], Confidence: 0.9}}
		if _, err := h.loop.ProcessDirect(context.Background(), msg, "cli:test"); err != nil {
			t.Fatal(err)
		}
	}

	// The first request has no history behind it; the interesting comparison
	// is between two turns that both carry a conversation.
	first, second := h.chat.requests[1].Messages, h.chat.requests[2].Messages
	shared := indexOfTurnBlock(t, first)
	if shared < 3 {
		t.Fatalf("the earlier turn had no history before its per-turn block to share: %d messages", shared)
	}
	if turnBlock(t, first) == turnBlock(t, second) {
		t.Fatal("both turns recalled the same thing, so a stable prefix proves nothing")
	}
	if len(second) <= shared {
		t.Fatalf("the second request is shorter than the first request's prefix")
	}
	for i := range shared {
		if first[i].Role != second[i].Role || first[i].Content != second[i].Content {
			t.Fatalf("message %d of the prefix changed between turns, so everything after it is prefilled again\nfirst:  %.120q\nsecond: %.120q",
				i, first[i].Content, second[i].Content)
		}
	}
}

// The block goes after the conversation so far — where it costs the cache
// nothing — and before what the user just said, which stays the last thing
// the model reads.
func TestThePerTurnBlockSitsBetweenTheHistoryAndTheNewMessage(t *testing.T) {
	h := newHarness(t, final("one"), final("two"))
	for _, msg := range []string{"primera", "segunda"} {
		if _, err := h.loop.ProcessDirect(context.Background(), msg, "cli:test"); err != nil {
			t.Fatal(err)
		}
	}

	sent := h.chat.requests[1].Messages
	at := indexOfTurnBlock(t, sent)
	if last := sent[len(sent)-1]; last.Role != "user" || last.Content != "segunda" {
		t.Errorf("the user's own message is not the last thing the model reads: %q from %q", last.Content, last.Role)
	}
	if at != len(sent)-2 {
		t.Errorf("the per-turn block is at %d of %d, want it immediately before the new message", at, len(sent))
	}
	var before []string
	for _, m := range sent[:at] {
		before = append(before, m.Content)
	}
	if !strings.Contains(strings.Join(before, "\n"), "primera") {
		t.Errorf("the conversation so far is not in front of the block: %q", before)
	}
}

// A session picked up hours later is not the same conversation continuing.
// Saying so is the difference between answering the question asked and
// resuming a task the user has forgotten about.
func TestATurnAfterALongSilenceIsToldTimePassed(t *testing.T) {
	h := newHarness(t, final("one"), final("two"))
	if _, err := h.loop.ProcessDirect(context.Background(), "seguimos con el informe", "cli:test"); err != nil {
		t.Fatal(err)
	}
	if got := turnBlock(t, h.chat.requests[0].Messages); strings.Contains(got, "have passed since") {
		t.Errorf("a fresh session was told the user had been away: %q", got)
	}

	backdate(t, h, "cli:test", 6*time.Hour)
	if _, err := h.loop.ProcessDirect(context.Background(), "cuánto está el dólar", "cli:test"); err != nil {
		t.Fatal(err)
	}
	got := turnBlock(t, h.chat.requests[1].Messages)
	if !strings.Contains(got, "About 6 hours have passed") {
		t.Errorf("the returning turn was not told how long the session had been cold: %q", got)
	}
	if !strings.Contains(got, "do not resume the earlier task") {
		t.Errorf("the notice says time passed but not what to do about it: %q", got)
	}
}

func TestGapNoticeWording(t *testing.T) {
	for _, tc := range []struct {
		gap  time.Duration
		want string
	}{
		{5 * time.Minute, ""},
		{90 * time.Minute, ""},
		{3 * time.Hour, "About 3 hours"},
		{26 * time.Hour, "About a day"},
		{72 * time.Hour, "About 3 days"},
	} {
		got := gapNotice(tc.gap)
		if tc.want == "" && got != "" {
			t.Errorf("gap of %s produced %q, want nothing", tc.gap, got)
		}
		if tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("gap of %s = %q, want it to mention %q", tc.gap, got, tc.want)
		}
	}
}

// Compacting on the way back in makes the user wait for housekeeping. The
// quiet hours before they return are free, so that is when it happens.
func TestAQuietSessionIsCompactedBeforeTheUserComesBack(t *testing.T) {
	h := newHarness(t, final("noted"), final("idle summary"))
	if _, err := h.loop.ProcessDirect(context.Background(), "hola", "cli:test"); err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		if err := h.store.Append("cli:test", provider.Message{Role: "user", Content: strings.Repeat("x", 400+i)}); err != nil {
			t.Fatal(err)
		}
	}
	h.loop.cfg.Agent.MaxContextTokens = 1
	h.loop.cfg.Agent.KeepRecentMessages = 2

	// Still warm: nobody touches a conversation that just happened.
	h.loop.compactIdleSessions(context.Background())
	h.loop.WaitBackground(2 * time.Second)
	if got := h.store.Summary("cli:test"); got != "" {
		t.Fatalf("a live session was compacted underneath the user: %q", got)
	}

	backdate(t, h, "cli:test", 2*time.Hour)
	h.loop.compactIdleSessions(context.Background())
	h.loop.WaitBackground(5 * time.Second)
	if got := h.store.Summary("cli:test"); got != "idle summary" {
		t.Errorf("summary = %q, want the idle sweep to have written one", got)
	}
}

// recallTexts differ per turn so the block in front of the history really is
// volatile — a stable prefix past a constant block would prove nothing.
var recallTexts = []string{
	"the user is called Nico",
	"the user prefers the terminal",
	"the user works on Factor",
}

// indexOfTurnBlock finds the per-turn block's position in a request.
func indexOfTurnBlock(t *testing.T, msgs []provider.Message) int {
	t.Helper()
	for i, m := range msgs {
		if strings.HasPrefix(m.Content, turnContextHeader) {
			return i
		}
	}
	t.Fatal("no per-turn block in the request")
	return -1
}

// backdate ages a session on disk, which is how the loop reads elapsed time.
func backdate(t *testing.T, h *harness, key string, by time.Duration) {
	t.Helper()
	path := filepath.Join(h.loop.cfg.Agent.Workspace, "sessions", strings.ReplaceAll(key, ":", "_")+".jsonl")
	when := time.Now().Add(-by)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}
