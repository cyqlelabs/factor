package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
)

func filled(t *testing.T, key string, n int) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if err := s.Append(key, provider.Message{Role: "user", Content: strings.Repeat("m", i+1)}); err != nil {
			t.Fatal(err)
		}
	}
	return s, dir
}

// What replaces a compacted turn in context is a summary written by a model,
// and that is the one part of this system that cannot be trusted to be
// complete. The raw turns are worth a few kilobytes of disk.
func TestCompactionMovesHistoryToTheArchiveInsteadOfDestroyingIt(t *testing.T) {
	s, dir := filled(t, "cli:main", 6)
	if err := s.SetSummary("cli:main", "what happened", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact("cli:main"); err != nil {
		t.Fatal(err)
	}

	live, err := s.History("cli:main")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 {
		t.Fatalf("live history = %d messages, want 2", len(live))
	}
	data, err := os.ReadFile(filepath.Join(dir, "cli_main.archive.jsonl"))
	if err != nil {
		t.Fatalf("the compacted turns are gone: %v", err)
	}
	if got := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; got != 4 {
		t.Errorf("archive holds %d messages, want the 4 that left the live history", got)
	}
	if !strings.Contains(string(data), `"mmm"`) {
		t.Errorf("a compacted message is not in the archive:\n%s", data)
	}

	// A second compaction appends rather than replacing: the archive is the
	// whole past, not the most recent slice of it.
	for i := range 6 {
		if err := s.Append("cli:main", provider.Message{Role: "user", Content: strings.Repeat("z", i+1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetSummary("cli:main", "more", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact("cli:main"); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(filepath.Join(dir, "cli_main.archive.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(again), `"mmm"`) || !strings.Contains(string(again), `"zzz"`) {
		t.Errorf("the second compaction overwrote the first one's archive:\n%s", again)
	}
}

// The archive shares the session's extension so the same tools read it. List
// has to know the difference, or every compacted session doubles.
func TestListIgnoresArchivesAndClearRemovesThem(t *testing.T) {
	s, dir := filled(t, "cli:main", 4)
	if err := s.SetSummary("cli:main", "s", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact("cli:main"); err != nil {
		t.Fatal(err)
	}

	keys, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "cli_main" {
		t.Errorf("List = %v, want just the session", keys)
	}
	if err := s.Clear("cli:main"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cli_main.archive.jsonl")); !os.IsNotExist(err) {
		t.Error("clearing a session left its archive behind")
	}
}

// Compaction is housekeeping, and housekeeping runs when nobody is there. If
// it counted as activity, a session tidied at 3am would look like a
// conversation that had just happened.
func TestCompactionDoesNotMakeASessionLookRecent(t *testing.T) {
	s, dir := filled(t, "cli:main", 6)
	long := time.Now().Add(-5 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "cli_main.jsonl"), long, long); err != nil {
		t.Fatal(err)
	}

	before, ok := s.LastActivity("cli:main")
	if !ok {
		t.Fatal("LastActivity found no session")
	}
	if time.Since(before) < 4*time.Hour {
		t.Fatalf("LastActivity = %s, want the backdated time", before)
	}
	if err := s.SetSummary("cli:main", "s", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact("cli:main"); err != nil {
		t.Fatal(err)
	}
	after, ok := s.LastActivity("cli:main")
	if !ok {
		t.Fatal("LastActivity lost the session to compaction")
	}
	if !after.Equal(before) {
		t.Errorf("compaction reset the clock: %s -> %s", before, after)
	}
}

func TestLastActivityOnAnUnknownSession(t *testing.T) {
	s, _ := filled(t, "cli:main", 1)
	if _, ok := s.LastActivity("cli:never"); ok {
		t.Error("a session that was never written reported activity")
	}
}
