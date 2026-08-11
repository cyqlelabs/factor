package session

import (
	"fmt"
	"sync"
	"testing"

	"github.com/cyqlelabs/factor/internal/provider"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAppendAndHistory(t *testing.T) {
	s := newStore(t)
	key := "cli:main"
	for i := range 3 {
		if err := s.Append(key, provider.Message{Role: "user", Content: fmt.Sprintf("m%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := s.History(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 3 || h[2].Content != "m2" {
		t.Fatalf("history = %+v", h)
	}
}

func TestToolCallsSurviveRoundTrip(t *testing.T) {
	s := newStore(t)
	msg := provider.Message{
		Role:      "assistant",
		ToolCalls: []provider.ToolCall{{ID: "c1", Name: "exec", Args: map[string]any{"command": "ls"}}},
	}
	if err := s.Append("k", msg); err != nil {
		t.Fatal(err)
	}
	h, _ := s.History("k")
	if len(h) != 1 || len(h[0].ToolCalls) != 1 || h[0].ToolCalls[0].Args["command"] != "ls" {
		t.Fatalf("round trip lost tool calls: %+v", h)
	}
}

func TestSummaryTruncation(t *testing.T) {
	s := newStore(t)
	key := "k"
	for i := range 10 {
		_ = s.Append(key, provider.Message{Role: "user", Content: fmt.Sprintf("m%d", i)})
	}
	if err := s.SetSummary(key, "the story so far", 4); err != nil {
		t.Fatal(err)
	}
	h, _ := s.History(key)
	if len(h) != 4 || h[0].Content != "m6" {
		t.Fatalf("history after truncate = %+v", h)
	}
	if s.Summary(key) != "the story so far" {
		t.Errorf("summary = %q", s.Summary(key))
	}
	// appends continue after truncation
	_ = s.Append(key, provider.Message{Role: "user", Content: "m10"})
	h, _ = s.History(key)
	if len(h) != 5 {
		t.Fatalf("history len = %d", len(h))
	}
}

func TestCompactRewrites(t *testing.T) {
	s := newStore(t)
	key := "k"
	for i := range 6 {
		_ = s.Append(key, provider.Message{Role: "user", Content: fmt.Sprintf("m%d", i)})
	}
	_ = s.SetSummary(key, "sum", 2)
	if err := s.Compact(key); err != nil {
		t.Fatal(err)
	}
	total, _ := s.TotalLen(key)
	if total != 2 {
		t.Errorf("physical len = %d, want 2", total)
	}
	h, _ := s.History(key)
	if len(h) != 2 || h[0].Content != "m4" {
		t.Fatalf("history = %+v", h)
	}
	if s.Summary(key) != "sum" {
		t.Error("summary lost in compact")
	}
}

func TestSanitizeKeyIsolation(t *testing.T) {
	s := newStore(t)
	_ = s.Append("telegram:123", provider.Message{Role: "user", Content: "a"})
	_ = s.Append("../../etc/passwd", provider.Message{Role: "user", Content: "b"})
	keys, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k == "" || k[0] == '/' || k[0] == '.' && len(k) > 1 && k[1] == '.' {
			t.Errorf("unsafe key on disk: %q", k)
		}
	}
}

func TestConcurrentAppends(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = s.Append("k", provider.Message{Role: "user", Content: fmt.Sprintf("m%d", n)})
		}(i)
	}
	wg.Wait()
	h, err := s.History("k")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 20 {
		t.Errorf("len = %d, want 20", len(h))
	}
}
