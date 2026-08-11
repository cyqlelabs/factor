package session

import (
	"os"
	"testing"

	"github.com/cyqlelabs/factor/internal/provider"
)

// A hand-edited or corrupt meta sidecar must never make History slice out
// of range.
func TestNegativeSkipIsClamped(t *testing.T) {
	s := newStore(t)
	key := "cli:corrupt"
	for range 3 {
		if err := s.Append(key, provider.Message{Role: "user", Content: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(s.metaPath(key), []byte(`{"skip":-5,"summary":"s"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	history, err := s.History(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Errorf("history = %d messages, want all 3", len(history))
	}
	if s.Skip(key) != 0 {
		t.Errorf("Skip() = %d, want 0 after clamping", s.Skip(key))
	}
	if err := s.Compact(key); err != nil {
		t.Errorf("Compact on a corrupt sidecar: %v", err)
	}
}

func TestSetSkipRejectsNegativeOffsets(t *testing.T) {
	s := newStore(t)
	key := "cli:neg"
	if err := s.Append(key, provider.Message{Role: "user", Content: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSkip(key, -1); err != nil {
		t.Fatal(err)
	}
	if got := s.Skip(key); got != 0 {
		t.Errorf("SetSkip(-1) stored %d, want 0", got)
	}
	history, err := s.History(key)
	if err != nil || len(history) != 1 {
		t.Errorf("History after a negative skip = %v, %v", history, err)
	}
}
