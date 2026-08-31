package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cyqlelabs/factor/internal/provider"
)

// countingChat records which chain a call landed on.
type countingChat struct {
	mu    sync.Mutex
	calls int
	reply string
}

func (c *countingChat) Chat(_ context.Context, _ *provider.Request) (*provider.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return &provider.Response{Content: c.reply, FinishReason: "stop", Model: "utility-model"}, nil
}

func (c *countingChat) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// Unconfigured, everything runs where it always did. This is the property that
// makes the split safe to ship: a summary is what replaces a session's
// history, so nobody should inherit a weaker model for it by upgrading.
func TestUtilityChatFallsBackToTheMainChain(t *testing.T) {
	h := newHarness(t)
	if h.loop.utilityChat() != h.loop.chat {
		t.Error("with no utility chain configured, housekeeping must use the main one")
	}
}

func TestWithUtilityRoutesHousekeeping(t *testing.T) {
	h := newHarness(t)
	util := &countingChat{reply: "SKIP"}
	h.loop.WithUtility(util)
	if h.loop.utilityChat() != ChatProvider(util) {
		t.Error("utilityChat did not return the configured chain")
	}
	if h.loop.chat == ChatProvider(util) {
		t.Error("the utility chain must not replace the conversation's own")
	}
}

// The summary is a housekeeping call: it runs on the utility chain when one is
// set, and the conversation's chain must not see it.
func TestCompactionSummaryUsesTheUtilityChain(t *testing.T) {
	h := newHarness(t)
	util := &countingChat{reply: "a summary of what came before"}
	h.loop.WithUtility(util)

	for i := 0; i < 8; i++ {
		if err := h.loop.sessions.Append("cli:x", provider.Message{
			Role: "user", Content: strings.Repeat("filler ", 400),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.loop.compact(context.Background(), "cli:x"); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if util.count() == 0 {
		t.Error("the compaction summary did not reach the utility chain")
	}
}

// WithUtility(nil) is the shape app.go passes when nothing is configured, and
// must not leave a typed-nil provider behind for the loop to call into.
func TestWithUtilityNilKeepsTheFallback(t *testing.T) {
	h := newHarness(t)
	h.loop.WithUtility(nil)
	if h.loop.utilityChat() != h.loop.chat {
		t.Error("a nil utility chain must fall back rather than panic later")
	}
}
