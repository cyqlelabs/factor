package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
)

type fakeChannel struct {
	name   string
	maxLen int
	mu     sync.Mutex
	sent   []string
	fails  int // fail this many sends before succeeding
}

func (f *fakeChannel) Name() string                { return f.name }
func (f *fakeChannel) Start(context.Context) error { return nil }
func (f *fakeChannel) Stop() error                 { return nil }
func (f *fakeChannel) MaxMessageLength() int       { return f.maxLen }
func (f *fakeChannel) Send(_ context.Context, msg bus.OutboundMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return fmt.Errorf("transient send failure")
	}
	f.sent = append(f.sent, msg.Content)
	return nil
}

func TestSplitMessage(t *testing.T) {
	if got := SplitMessage("short", 100); len(got) != 1 || got[0] != "short" {
		t.Errorf("short = %v", got)
	}
	long := strings.Repeat("word ", 100) // 500 chars
	chunks := SplitMessage(long, 120)
	if len(chunks) < 4 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 120 {
			t.Errorf("chunk %d over limit: %d", i, len(c))
		}
		if strings.HasPrefix(c, " ") {
			t.Errorf("chunk %d starts with space", i)
		}
	}
	if rejoined := strings.Join(chunks, " "); strings.ReplaceAll(rejoined, " ", "") != strings.ReplaceAll(long, " ", "") {
		t.Error("content lost in split")
	}
	// newline-preferring split
	nl := SplitMessage("line one\nline two\nline three", 12)
	if nl[0] != "line one" {
		t.Errorf("newline split = %q", nl[0])
	}
}

func TestBuildGatesAndSkips(t *testing.T) {
	Register("fake", func(raw json.RawMessage, b *bus.MessageBus) (Channel, error) {
		return &fakeChannel{name: "fake"}, nil
	})
	cfgs := map[string]json.RawMessage{
		"fake":    json.RawMessage(`{}`),
		"unknown": json.RawMessage(`{}`),
		"fake2":   json.RawMessage(`{"enabled":false}`),
	}
	Register("fake2", func(raw json.RawMessage, b *bus.MessageBus) (Channel, error) {
		t.Error("disabled channel factory called")
		return nil, nil
	})
	built := Build(cfgs, bus.New())
	if len(built) != 1 || built[0].Name() != "fake" {
		t.Errorf("built = %v", built)
	}
}

func TestManagerChunksAndRetries(t *testing.T) {
	b := bus.New()
	ch := &fakeChannel{name: "fake", maxLen: 10, fails: 1}
	m := NewManager(b, []Channel{ch})
	m.sleep = func(context.Context, time.Duration) {}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)

	b.PublishOutbound(bus.OutboundMessage{Channel: "fake", ChatID: "1", Content: "aaaa bbbb cccc"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		ch.mu.Lock()
		n := len(ch.sent)
		ch.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	m.Stop()
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if len(ch.sent) < 2 {
		t.Fatalf("sent = %v (retry or chunking failed)", ch.sent)
	}
	for _, s := range ch.sent {
		if len(s) > 10 {
			t.Errorf("chunk over channel limit: %q", s)
		}
	}
}
