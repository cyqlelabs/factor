package channel

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
)

// scriptedChannel records every Send attempt (failed ones included) and can be
// scripted to fail Start, Stop, or every Send.
type scriptedChannel struct {
	name     string
	maxLen   int
	startErr error
	stopErr  error
	sendErr  error // non-nil means every Send fails

	mu       sync.Mutex
	attempts []string
	sent     []string
	starts   int
	stops    int
}

func (s *scriptedChannel) Name() string          { return s.name }
func (s *scriptedChannel) MaxMessageLength() int { return s.maxLen }

func (s *scriptedChannel) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts++
	return s.startErr
}

func (s *scriptedChannel) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
	return s.stopErr
}

func (s *scriptedChannel) Send(_ context.Context, msg bus.OutboundMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts = append(s.attempts, msg.Content)
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sent = append(s.sent, msg.Content)
	return nil
}

func (s *scriptedChannel) snapshot() (attempts, sent []string, starts, stops int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.attempts...), append([]string(nil), s.sent...), s.starts, s.stops
}

func TestManagerNamesListsEveryChannel(t *testing.T) {
	m := NewManager(bus.New(), []Channel{
		&scriptedChannel{name: "beta"},
		&scriptedChannel{name: "alpha"},
	})
	got := m.Names()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("Names() = %v, want [alpha beta]", got)
	}
	if names := NewManager(bus.New(), nil).Names(); len(names) != 0 {
		t.Errorf("Names() with no channels = %v, want empty", names)
	}
}

func TestManagerStopStopsEveryChannelDespiteAnError(t *testing.T) {
	failing := &scriptedChannel{name: "failing", stopErr: errors.New("stop refused")}
	healthy := &scriptedChannel{name: "healthy"}
	m := NewManager(bus.New(), []Channel{failing, healthy})

	m.Stop()

	for _, ch := range []*scriptedChannel{failing, healthy} {
		if _, _, _, stops := ch.snapshot(); stops != 1 {
			t.Errorf("channel %s stopped %d times, want 1", ch.name, stops)
		}
	}
}

func TestManagerStartStartsHealthyChannelsWhenOneFails(t *testing.T) {
	failing := &scriptedChannel{name: "failing", startErr: errors.New("start refused")}
	healthy := &scriptedChannel{name: "healthy"}
	m := NewManager(bus.New(), []Channel{failing, healthy})

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	cancel()
	m.Stop()

	for _, ch := range []*scriptedChannel{failing, healthy} {
		if _, _, starts, _ := ch.snapshot(); starts != 1 {
			t.Errorf("channel %s started %d times, want 1", ch.name, starts)
		}
	}
}

// typingChannel is a scriptedChannel that can also show a typing indicator.
type typingChannel struct {
	scriptedChannel
	mu    sync.Mutex
	calls []string // "chatID:on" / "chatID:off"
}

func (c *typingChannel) SetTyping(chatID string, on bool) {
	state := "off"
	if on {
		state = "on"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, chatID+":"+state)
}

func (c *typingChannel) typingCalls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func TestSetTypingRoutesSessionKeysToTheirChannel(t *testing.T) {
	typer := &typingChannel{scriptedChannel: scriptedChannel{name: "telegram"}}
	plain := &scriptedChannel{name: "sms"} // no Typer: must be skipped, not panic
	m := NewManager(bus.New(), []Channel{typer, plain})

	m.SetTyping("telegram:42", true)
	m.SetTyping("telegram:42", false)
	m.SetTyping("sms:9", true)
	m.SetTyping("ghost:1", true) // a channel this manager does not own
	m.SetTyping("no-chat-id", true)

	got := typer.typingCalls()
	want := []string{"42:on", "42:off"}
	if len(got) != len(want) {
		t.Fatalf("typing calls = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("typing call %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSetTypingSplitsOnTheFirstColonOnly(t *testing.T) {
	typer := &typingChannel{scriptedChannel: scriptedChannel{name: "telegram"}}
	m := NewManager(bus.New(), []Channel{typer})

	m.SetTyping("telegram:-100:99", true) // supergroup ids carry their own colons

	if got := typer.typingCalls(); len(got) != 1 || got[0] != "-100:99:on" {
		t.Errorf("typing calls = %v, want the full chat id after the first colon", got)
	}
}

func TestDeliverDropsMessagesForUnknownChannels(t *testing.T) {
	known := &scriptedChannel{name: "known"}
	m := NewManager(bus.New(), []Channel{known})
	m.sleep = func(context.Context, time.Duration) { t.Error("unknown channel should not trigger a retry sleep") }

	m.deliver(context.Background(), bus.OutboundMessage{Channel: "ghost", ChatID: "1", Content: "hello"})

	if attempts, _, _, _ := known.snapshot(); len(attempts) != 0 {
		t.Errorf("known channel received %v for an unknown-channel message", attempts)
	}
}

func TestDeliverStopsSendingChunksOnceRetriesAreExhausted(t *testing.T) {
	ch := &scriptedChannel{name: "flaky", maxLen: 6, sendErr: errors.New("permanent failure")}
	m := NewManager(bus.New(), []Channel{ch})
	var slept []time.Duration
	m.sleep = func(_ context.Context, d time.Duration) { slept = append(slept, d) }

	m.deliver(context.Background(), bus.OutboundMessage{
		Channel: "flaky", ChatID: "1", Content: "aaaaa bbbbb ccccc",
	})

	attempts, sent, _, _ := ch.snapshot()
	if len(attempts) != 3 {
		t.Fatalf("attempts = %v, want 3 attempts at the first chunk only", attempts)
	}
	for i, a := range attempts {
		if a != "aaaaa" {
			t.Errorf("attempt %d = %q, want the first chunk %q (later chunks must not be sent)", i, a, "aaaaa")
		}
	}
	if len(sent) != 0 {
		t.Errorf("sent = %v, want nothing delivered", sent)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 6 * time.Second}
	if len(slept) != len(want) {
		t.Fatalf("backoff sleeps = %v, want %v", slept, want)
	}
	for i, d := range slept {
		if d != want[i] {
			t.Errorf("backoff sleep %d = %v, want %v", i, d, want[i])
		}
	}
}

func TestDeliverAbandonsRetriesWhenContextIsCancelled(t *testing.T) {
	ch := &scriptedChannel{name: "flaky", sendErr: errors.New("permanent failure")}
	m := NewManager(bus.New(), []Channel{ch})
	m.sleep = func(context.Context, time.Duration) { t.Error("slept after the context was cancelled") }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.deliver(ctx, bus.OutboundMessage{Channel: "flaky", ChatID: "1", Content: "hello"})

	if attempts, _, _, _ := ch.snapshot(); len(attempts) != 1 {
		t.Errorf("attempts = %v, want a single attempt before the cancelled context aborts the retry loop", attempts)
	}
}

func TestSleepCtxReturnsImmediatelyOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	sleepCtx(ctx, time.Hour)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("sleepCtx waited %v on a cancelled context, want an immediate return", elapsed)
	}
}

func TestSleepCtxReturnsWhenTheTimerFires(t *testing.T) {
	start := time.Now()
	sleepCtx(context.Background(), time.Millisecond)
	if elapsed := time.Since(start); elapsed < time.Millisecond {
		t.Errorf("sleepCtx returned after %v, want at least 1ms", elapsed)
	}
}

func TestSplitMessageEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		content string
		limit   int
		want    []string
	}{
		{"zero limit means unlimited", "any content at all", 0, []string{"any content at all"}},
		{"negative limit means unlimited", "any content at all", -10, []string{"any content at all"}},
		{"content exactly at the limit is one chunk", "12345", 5, []string{"12345"}},
		{"content one over the limit splits", "123456", 5, []string{"12345", "6"}},
		{"empty content is one empty chunk", "", 10, []string{""}},
		{"a single word longer than the limit is hard-cut", "abcdefghij", 4, []string{"abcd", "efgh", "ij"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitMessage(tc.content, tc.limit)
			if len(got) != len(tc.want) {
				t.Fatalf("SplitMessage(%q, %d) = %q, want %q", tc.content, tc.limit, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("chunk %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
			if joined := strings.Join(got, ""); tc.limit > 0 && !strings.Contains(tc.content, " ") && joined != tc.content {
				t.Errorf("rejoined = %q, want the original content %q", joined, tc.content)
			}
		})
	}
}

func TestBuildSkipsChannelsWhoseFactoryFails(t *testing.T) {
	Register("build-err", func(json.RawMessage, *bus.MessageBus) (Channel, error) {
		return nil, errors.New("missing credentials")
	})
	Register("build-ok", func(json.RawMessage, *bus.MessageBus) (Channel, error) {
		return &scriptedChannel{name: "build-ok"}, nil
	})

	built := Build(map[string]json.RawMessage{
		"build-err": json.RawMessage(`{}`),
		"build-ok":  json.RawMessage(`{}`),
	}, bus.New())

	if len(built) != 1 || built[0].Name() != "build-ok" {
		t.Errorf("Build() = %v, want only the channel whose factory succeeded", built)
	}
}

func TestBuildSkipsMalformedConfigSections(t *testing.T) {
	Register("build-strict", func(raw json.RawMessage, _ *bus.MessageBus) (Channel, error) {
		var cfg struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		return &scriptedChannel{name: "build-strict"}, nil
	})

	built := Build(map[string]json.RawMessage{
		"build-strict": json.RawMessage(`{"token":`),
	}, bus.New())

	if len(built) != 0 {
		t.Errorf("Build() = %v, want nothing built from a malformed config section", built)
	}
}

func TestRegisteredIncludesRegisteredFactories(t *testing.T) {
	Register("build-listed", func(json.RawMessage, *bus.MessageBus) (Channel, error) {
		return &scriptedChannel{name: "build-listed"}, nil
	})
	for _, n := range Registered() {
		if n == "build-listed" {
			return
		}
	}
	t.Errorf("Registered() = %v, want it to include build-listed", Registered())
}
