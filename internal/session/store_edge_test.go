package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/provider"
)

// seed appends n numbered user messages to key.
func seed(t *testing.T, s *Store, key string, n int) {
	t.Helper()
	for i := range n {
		if err := s.Append(key, provider.Message{Role: "user", Content: fmt.Sprintf("m%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNewStoreFailsOnUnusableDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(filepath.Join(file, "sessions"))
	if err == nil {
		t.Fatal("want an error when the parent path is a regular file")
	}
	if s != nil {
		t.Errorf("store = %v, want nil alongside the error", s)
	}
}

func TestSetSummaryAtUsesAbsoluteOffset(t *testing.T) {
	s := newStore(t)
	key := "k"
	seed(t, s, key, 5)

	if err := s.SetSummaryAt(key, "earlier turns", 2); err != nil {
		t.Fatal(err)
	}
	h, err := s.History(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 3 || h[0].Content != "m2" {
		t.Fatalf("history = %+v, want the tail from index 2", h)
	}
	if s.Summary(key) != "earlier turns" {
		t.Errorf("summary = %q", s.Summary(key))
	}
	if s.Skip(key) != 2 {
		t.Errorf("skip = %d, want 2", s.Skip(key))
	}
}

func TestSetSummaryAtOffsetSurvivesConcurrentAppend(t *testing.T) {
	// The offset is physical, so a message appended after the snapshot was
	// taken must stay live rather than shifting the cut.
	s := newStore(t)
	key := "k"
	seed(t, s, key, 4)
	snapshotSkip := 2
	if err := s.Append(key, provider.Message{Role: "user", Content: "late"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSummaryAt(key, "sum", snapshotSkip); err != nil {
		t.Fatal(err)
	}
	h, _ := s.History(key)
	if len(h) != 3 || h[2].Content != "late" {
		t.Fatalf("history = %+v, want m2, m3, late", h)
	}
}

func TestSetSummaryAtClampsNegativeOffset(t *testing.T) {
	s := newStore(t)
	key := "k"
	seed(t, s, key, 2)
	if err := s.SetSummaryAt(key, "sum", -10); err != nil {
		t.Fatal(err)
	}
	if s.Skip(key) != 0 {
		t.Errorf("skip = %d, want a negative offset clamped to 0", s.Skip(key))
	}
	h, _ := s.History(key)
	if len(h) != 2 {
		t.Errorf("history len = %d, want the whole log", len(h))
	}
}

func TestSkipIsZeroForFreshSession(t *testing.T) {
	s := newStore(t)
	if got := s.Skip("never-used"); got != 0 {
		t.Errorf("skip = %d, want 0", got)
	}
}

func TestSetSkipKeepsSummary(t *testing.T) {
	s := newStore(t)
	key := "k"
	seed(t, s, key, 6)
	if err := s.SetSummaryAt(key, "the story so far", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSkip(key, 4); err != nil {
		t.Fatal(err)
	}
	if s.Skip(key) != 4 {
		t.Errorf("skip = %d, want 4", s.Skip(key))
	}
	if s.Summary(key) != "the story so far" {
		t.Errorf("summary = %q, want it preserved across SetSkip", s.Summary(key))
	}
	h, _ := s.History(key)
	if len(h) != 2 || h[0].Content != "m4" {
		t.Fatalf("history = %+v", h)
	}
}

func TestSetSkipOnFreshSession(t *testing.T) {
	s := newStore(t)
	if err := s.SetSkip("brand-new", 3); err != nil {
		t.Fatal(err)
	}
	if s.Skip("brand-new") != 3 {
		t.Errorf("skip = %d, want 3", s.Skip("brand-new"))
	}
}

func TestWriteMetaRoundTripsSummaryText(t *testing.T) {
	s := newStore(t)
	key := "k"
	summary := "line one\nline two\t— café 🙂 \"quoted\""
	if err := s.SetSummaryAt(key, summary, 0); err != nil {
		t.Fatal(err)
	}
	if got := s.Summary(key); got != summary {
		t.Errorf("summary = %q, want %q", got, summary)
	}
}

func TestClearRemovesHistoryAndMeta(t *testing.T) {
	s := newStore(t)
	key := "k"
	seed(t, s, key, 3)
	if err := s.SetSummaryAt(key, "sum", 1); err != nil {
		t.Fatal(err)
	}

	if err := s.Clear(key); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.historyPath(key), s.metaPath(key)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after Clear", p)
		}
	}
	h, err := s.History(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 0 {
		t.Errorf("history = %+v, want empty", h)
	}
	if s.Summary(key) != "" {
		t.Errorf("summary = %q, want empty", s.Summary(key))
	}
}

func TestClearMissingSessionSucceeds(t *testing.T) {
	s := newStore(t)
	if err := s.Clear("never-existed"); err != nil {
		t.Errorf("Clear on a missing session = %v, want nil", err)
	}
}

func TestListEmptyDirectory(t *testing.T) {
	s := newStore(t)
	keys, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("keys = %v, want none", keys)
	}
}

func TestListReportsOnlyHistoryFiles(t *testing.T) {
	s := newStore(t)
	seed(t, s, "alpha", 1)
	if err := s.SetSummaryAt("alpha", "sum", 0); err != nil {
		t.Fatal(err)
	}
	keys, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "alpha" {
		t.Errorf("keys = %v, want just [alpha] (meta sidecars excluded)", keys)
	}
}

func TestListPropagatesDirectoryError(t *testing.T) {
	s := newStore(t)
	if err := os.RemoveAll(s.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil {
		t.Error("want an error when the session directory is gone")
	}
}

func TestSanitizeKeyFallsBackToDefault(t *testing.T) {
	s := newStore(t)
	if err := s.Append("...", provider.Message{Role: "user", Content: "a"}); err != nil {
		t.Fatal(err)
	}
	keys, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "default" {
		t.Errorf("keys = %v, want [default] for a key with no usable characters", keys)
	}
}

func TestAppendRejectsUnencodableMessage(t *testing.T) {
	s := newStore(t)
	err := s.Append("k", provider.Message{
		Role:      "assistant",
		ToolCalls: []provider.ToolCall{{ID: "c1", Name: "t", Args: map[string]any{"ch": make(chan int)}}},
	})
	if err == nil {
		t.Fatal("want an error for a message that cannot be encoded")
	}
	if _, statErr := os.Stat(s.historyPath("k")); !os.IsNotExist(statErr) {
		t.Error("a rejected message must not create the history file")
	}
}

func TestAppendFailsWhenLogPathIsNotAFile(t *testing.T) {
	s := newStore(t)
	key := "k"
	if err := os.Mkdir(s.historyPath(key), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(key, provider.Message{Role: "user", Content: "x"}); err == nil {
		t.Error("want an error when the log path cannot be opened for writing")
	}
}

func TestReadAllPropagatesOpenError(t *testing.T) {
	s := newStore(t)
	// A directory where the log file belongs: opening it succeeds and reading
	// it fails, on every platform. A too-long filename would not do — Windows
	// reports that as "path not found", which readAll deliberately tolerates.
	key := "unreadable"
	if err := os.Mkdir(s.historyPath(key), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.History(key); err == nil {
		t.Error("want an error when the log path cannot be read")
	}
}

func TestSetSummaryClampsWhenKeepLastExceedsHistory(t *testing.T) {
	s := newStore(t)
	key := "k"
	seed(t, s, key, 3)
	if err := s.SetSummary(key, "sum", 100); err != nil {
		t.Fatal(err)
	}
	if s.Skip(key) != 0 {
		t.Errorf("skip = %d, want 0 when keepLast exceeds the history", s.Skip(key))
	}
	h, _ := s.History(key)
	if len(h) != 3 {
		t.Errorf("history len = %d, want the whole log kept", len(h))
	}
}

func TestSetSkipPropagatesMetaWriteError(t *testing.T) {
	s := newStore(t)
	key := "k"
	// A directory where the meta file belongs: the atomic rename cannot land.
	if err := os.Mkdir(s.metaPath(key), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSkip(key, 2); err == nil {
		t.Error("want an error when the meta sidecar cannot be replaced")
	}
}

func TestClearPropagatesRemoveError(t *testing.T) {
	s := newStore(t)
	key := "k"
	if err := os.Mkdir(s.historyPath(key), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.historyPath(key), "blocker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(key); err == nil {
		t.Error("want an error when a session path cannot be removed")
	}
}

func TestCompactPropagatesTempWriteError(t *testing.T) {
	s := newStore(t)
	key := "k"
	seed(t, s, key, 4)
	if err := s.SetSummaryAt(key, "sum", 2); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.historyPath(key)+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}

	if err := s.Compact(key); err == nil {
		t.Fatal("want an error when the temporary log cannot be written")
	}
	total, _ := s.TotalLen(key)
	if total != 4 {
		t.Errorf("physical len = %d, want the original log left intact", total)
	}
}

func TestCompactReportsMetaFailureAfterRewrite(t *testing.T) {
	s := newStore(t)
	key := "k"
	seed(t, s, key, 4)
	if err := s.SetSummaryAt(key, "sum", 2); err != nil {
		t.Fatal(err)
	}
	// Block the meta sidecar's temp file so the rewrite lands but the offset
	// reset does not.
	if err := os.Mkdir(s.metaPath(key)+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}

	err := s.Compact(key)
	if err == nil {
		t.Fatal("want an error when the log is rewritten but the meta is not")
	}
	if !strings.Contains(err.Error(), "compact wrote history but not meta") {
		t.Errorf("err = %v, want it to name the inconsistent state", err)
	}
	total, _ := s.TotalLen(key)
	if total != 2 {
		t.Errorf("physical len = %d, want the rewritten log", total)
	}
}

func TestCompactIsNoopWhenNothingTruncated(t *testing.T) {
	s := newStore(t)
	key := "k"
	seed(t, s, key, 3)
	before, err := os.ReadFile(s.historyPath(key))
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Compact(key); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(s.historyPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("Compact rewrote the log with skip 0:\nbefore %q\nafter  %q", before, after)
	}
	total, _ := s.TotalLen(key)
	if total != 3 {
		t.Errorf("physical len = %d, want 3", total)
	}
}

func TestCompactClampsSkipBeyondMessageCount(t *testing.T) {
	s := newStore(t)
	key := "k"
	seed(t, s, key, 3)
	if err := s.SetSummaryAt(key, "sum", 99); err != nil {
		t.Fatal(err)
	}

	if err := s.Compact(key); err != nil {
		t.Fatal(err)
	}
	total, err := s.TotalLen(key)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Errorf("physical len = %d, want 0 after compacting past the end", total)
	}
	if s.Skip(key) != 0 {
		t.Errorf("skip = %d, want it reset by compaction", s.Skip(key))
	}
	if s.Summary(key) != "sum" {
		t.Errorf("summary = %q, want it preserved", s.Summary(key))
	}
	if err := s.Append(key, provider.Message{Role: "user", Content: "next"}); err != nil {
		t.Fatal(err)
	}
	h, _ := s.History(key)
	if len(h) != 1 || h[0].Content != "next" {
		t.Fatalf("history after a post-compaction append = %+v", h)
	}
}

func TestReadAllSkipsCorruptAndTornLines(t *testing.T) {
	s := newStore(t)
	key := "k"
	content := `{"role":"user","content":"good1"}
this line is not json at all

{"role":"user","content":"good2"}
{"role":"user","conte`
	if err := os.WriteFile(s.historyPath(key), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := s.History(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 || h[0].Content != "good1" || h[1].Content != "good2" {
		t.Fatalf("history = %+v, want the two intact messages", h)
	}
}

func TestHistoryIsEmptyWhenSkipCoversEverything(t *testing.T) {
	s := newStore(t)
	key := "k"
	seed(t, s, key, 3)
	for _, skip := range []int{3, 10} {
		if err := s.SetSkip(key, skip); err != nil {
			t.Fatal(err)
		}
		h, err := s.History(key)
		if err != nil {
			t.Fatal(err)
		}
		if len(h) != 0 {
			t.Errorf("skip %d → history %+v, want empty", skip, h)
		}
	}
}

func TestHistoryAndSummaryOnUnknownSession(t *testing.T) {
	s := newStore(t)
	h, err := s.History("never-existed")
	if err != nil {
		t.Fatalf("History on a missing session = %v, want no error", err)
	}
	if len(h) != 0 {
		t.Errorf("history = %+v, want empty", h)
	}
	if got := s.Summary("never-existed"); got != "" {
		t.Errorf("summary = %q, want empty", got)
	}
	total, err := s.TotalLen("never-existed")
	if err != nil || total != 0 {
		t.Errorf("TotalLen = %d, %v; want 0, nil", total, err)
	}
}

func TestHistoryPropagatesUnreadableLog(t *testing.T) {
	s := newStore(t)
	key := "k"
	// A directory where the log should be: open succeeds, reading does not.
	if err := os.Mkdir(s.historyPath(key), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.History(key); err == nil {
		t.Error("want an error when the history log cannot be read")
	}
	if _, err := s.TotalLen(key); err == nil {
		t.Error("want TotalLen to surface the same read error")
	}
	if err := s.SetSummary(key, "sum", 2); err == nil {
		t.Error("want SetSummary to surface the same read error")
	}
	if err := s.Compact(key); err == nil {
		t.Error("want Compact to surface the same read error")
	}
}
