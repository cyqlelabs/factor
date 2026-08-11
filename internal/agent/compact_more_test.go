package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/provider"
)

func TestEstimateTokens(t *testing.T) {
	if got := estimateTokens(nil); got != 0 {
		t.Errorf("empty history = %d", got)
	}
	small := estimateTokens([]provider.Message{{Role: "user", Content: "hello"}})
	big := estimateTokens([]provider.Message{{Role: "user", Content: strings.Repeat("x", 4000)}})
	if big <= small {
		t.Errorf("longer content must estimate higher: %d vs %d", big, small)
	}
	withTools := estimateTokens([]provider.Message{{
		Role:      "assistant",
		ToolCalls: []provider.ToolCall{{Name: "exec"}, {Name: "read_file"}},
	}})
	if withTools <= 8 {
		t.Errorf("tool calls not counted: %d", withTools)
	}
}

func TestNeedsCompaction(t *testing.T) {
	h := newHarness(t)
	h.loop.cfg.Agent.SummarizeAtMessages = 5
	h.loop.cfg.Agent.ContextWindowTokens = 1000
	h.loop.cfg.Agent.SummarizeAtPercent = 50

	short := make([]provider.Message, 3)
	if h.loop.needsCompaction(short) {
		t.Error("short history flagged for compaction")
	}

	many := make([]provider.Message, 6)
	if !h.loop.needsCompaction(many) {
		t.Error("message-count threshold not applied")
	}

	// token budget alone triggers it
	heavy := []provider.Message{{Role: "user", Content: strings.Repeat("x", 4000)}}
	if !h.loop.needsCompaction(heavy) {
		t.Error("token threshold not applied")
	}

	// a zero budget disables the token check without panicking
	h.loop.cfg.Agent.ContextWindowTokens = 0
	if h.loop.needsCompaction(heavy) {
		t.Error("zero context window should disable the token check")
	}
}

func TestCompactNoOpCases(t *testing.T) {
	h := newHarness(t)
	key := "cli:short"

	// nothing to compact: fewer messages than we keep
	for range 2 {
		_ = h.store.Append(key, provider.Message{Role: "user", Content: "hi"})
	}
	if err := h.loop.compact(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if h.store.Summary(key) != "" {
		t.Error("summarized a session shorter than the keep window")
	}
	if len(h.chat.requests) != 0 {
		t.Error("compaction called the LLM with nothing to summarize")
	}

	// no safe boundary: the tail has no user message to cut before
	other := "cli:nosafe"
	_ = h.store.Append(other, provider.Message{Role: "user", Content: "start"})
	for range 6 {
		_ = h.store.Append(other, provider.Message{Role: "assistant", Content: "thinking"})
	}
	h.loop.cfg.Agent.KeepRecentMessages = 1
	if err := h.loop.compact(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if h.store.Summary(other) != "" {
		t.Error("compacted at an unsafe boundary")
	}
}

func TestCompactPropagatesSummarizeFailure(t *testing.T) {
	h := newHarness(t)
	key := "cli:fail"
	for i := range 10 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		_ = h.store.Append(key, provider.Message{Role: role, Content: fmt.Sprintf("m%d", i)})
	}
	h.chat.script = []func(*provider.Request) (*provider.Response, error){
		func(*provider.Request) (*provider.Response, error) { return nil, errors.New("llm down") },
	}
	err := h.loop.compact(context.Background(), key)
	if err == nil || !strings.Contains(err.Error(), "summarize") {
		t.Fatalf("err = %v", err)
	}
	if h.store.Summary(key) != "" {
		t.Error("failed summarization still wrote a summary")
	}
	history, _ := h.store.History(key)
	if len(history) != 10 {
		t.Errorf("history truncated despite a failed summary: %d", len(history))
	}
}

func TestCompactFoldsPriorSummary(t *testing.T) {
	h := newHarness(t)
	key := "cli:folded"
	for i := range 12 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		_ = h.store.Append(key, provider.Message{Role: role, Content: fmt.Sprintf("m%d", i)})
	}
	if err := h.store.SetSummaryAt(key, "the earlier summary", 0); err != nil {
		t.Fatal(err)
	}
	h.chat.script = []func(*provider.Request) (*provider.Response, error){
		func(req *provider.Request) (*provider.Response, error) {
			if !strings.Contains(req.Messages[0].Content, "the earlier summary") {
				t.Error("prior summary not folded into the summarize prompt")
			}
			if !strings.Contains(req.Messages[1].Content, "user: m0") {
				t.Errorf("transcript missing: %q", req.Messages[1].Content)
			}
			return &provider.Response{Content: "merged summary"}, nil
		},
	}
	if err := h.loop.compact(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if h.store.Summary(key) != "merged summary" {
		t.Errorf("summary = %q", h.store.Summary(key))
	}
}

func TestCompactIncludesToolNamesInTranscript(t *testing.T) {
	h := newHarness(t)
	key := "cli:tools"
	_ = h.store.Append(key, provider.Message{Role: "user", Content: "do it"})
	_ = h.store.Append(key, provider.Message{Role: "assistant",
		ToolCalls: []provider.ToolCall{{ID: "1", Name: "exec"}}})
	_ = h.store.Append(key, provider.Message{Role: "tool", ToolCallID: "1", Content: "output"})
	_ = h.store.Append(key, provider.Message{Role: "assistant", Content: "done"})
	for i := range 4 {
		_ = h.store.Append(key, provider.Message{Role: "user", Content: fmt.Sprintf("more %d", i)})
	}
	h.loop.cfg.Agent.KeepRecentMessages = 2
	h.chat.script = []func(*provider.Request) (*provider.Response, error){
		func(req *provider.Request) (*provider.Response, error) {
			if !strings.Contains(req.Messages[1].Content, "assistant used tool: exec") {
				t.Errorf("tool usage missing from transcript: %q", req.Messages[1].Content)
			}
			return &provider.Response{Content: "s"}, nil
		},
	}
	if err := h.loop.compact(context.Background(), key); err != nil {
		t.Fatal(err)
	}
}

func TestMaybeCompactAsyncSkipsEphemeralAndShortSessions(t *testing.T) {
	h := newHarness(t)
	h.loop.maybeCompactAsync(turnInput{sessionKey: "cli:x", ephemeral: true})
	h.loop.maybeCompactAsync(turnInput{sessionKey: "cli:does-not-exist"})
	h.loop.WaitBackground(2 * time.Second)
	if len(h.chat.requests) != 0 {
		t.Errorf("compaction ran when it should not have: %d LLM calls", len(h.chat.requests))
	}
}

func TestMaybeCompactAsyncRunsWhenOverThreshold(t *testing.T) {
	h := newHarness(t)
	key := "cli:big"
	h.loop.cfg.Agent.SummarizeAtMessages = 4
	h.loop.cfg.Agent.KeepRecentMessages = 2
	for i := range 10 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		_ = h.store.Append(key, provider.Message{Role: role, Content: fmt.Sprintf("m%d", i)})
	}
	h.chat.script = []func(*provider.Request) (*provider.Response, error){final("async summary")}

	h.loop.maybeCompactAsync(turnInput{sessionKey: key})
	h.loop.WaitBackground(10 * time.Second)

	if h.store.Summary(key) != "async summary" {
		t.Errorf("summary = %q", h.store.Summary(key))
	}
}

func TestWaitBackgroundTimesOut(t *testing.T) {
	h := newHarness(t)
	block := make(chan struct{})
	h.loop.wg.Add(1)
	go func() {
		defer h.loop.wg.Done()
		<-block
	}()
	start := time.Now()
	h.loop.WaitBackground(150 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("WaitBackground blocked for %s despite its timeout", elapsed)
	}
	close(block)
	h.loop.WaitBackground(2 * time.Second)
}

func TestLastChannel(t *testing.T) {
	h := newHarness(t)
	if _, _, ok := h.loop.LastChannel(); ok {
		t.Error("LastChannel reported a target before any message")
	}

	// internal channels are never delivery targets
	h.loop.recordLastChannel(bus.InboundMessage{Channel: "cli", ChatID: "main"})
	h.loop.recordLastChannel(bus.InboundMessage{Channel: "system", ChatID: "heartbeat"})
	if _, _, ok := h.loop.LastChannel(); ok {
		t.Error("cli/system recorded as an external channel")
	}

	h.loop.recordLastChannel(bus.InboundMessage{Channel: "telegram", ChatID: "42"})
	ch, chat, ok := h.loop.LastChannel()
	if !ok || ch != "telegram" || chat != "42" {
		t.Errorf("LastChannel = %q %q %v", ch, chat, ok)
	}

	h.loop.recordLastChannel(bus.InboundMessage{Channel: "telegram", ChatID: "99"})
	if _, chat, _ := h.loop.LastChannel(); chat != "99" {
		t.Errorf("later message did not update the target: %q", chat)
	}
}

func TestTurnErrorSurfacesToTheUser(t *testing.T) {
	h := newHarness(t)
	h.chat.script = []func(*provider.Request) (*provider.Response, error){
		func(*provider.Request) (*provider.Response, error) { return nil, errors.New("provider exploded") },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.loop.Run(ctx)

	h.bus.PublishInbound(bus.InboundMessage{Channel: "telegram", ChatID: "1", Content: "hi"})
	select {
	case out := <-h.bus.Outbound():
		if !strings.Contains(out.Content, "Something went wrong") || !strings.Contains(out.Content, "provider exploded") {
			t.Errorf("outbound = %q", out.Content)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no error reply delivered")
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.loop.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestSteeringQueueOverflowDropsInsteadOfBlocking(t *testing.T) {
	h := newHarness(t)
	key := "telegram:overflow"
	t2, ok := h.loop.claim(key, nil)
	if !ok {
		t.Fatal("claim failed on an idle session")
	}
	for i := range steeringBuffer + 5 {
		h.loop.claim(key, &bus.InboundMessage{Channel: "telegram", ChatID: "overflow",
			Content: fmt.Sprintf("m%d", i)})
	}
	leftover := h.loop.release(key, t2)
	if len(leftover) != steeringBuffer {
		t.Errorf("drained %d steering messages, want the buffer size %d", len(leftover), steeringBuffer)
	}
}

func TestReleaseRepublishesLateSteering(t *testing.T) {
	h := newHarness(t)
	h.chat.script = []func(*provider.Request) (*provider.Response, error){final("first"), final("second")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.loop.Run(ctx)

	// A message that lands after the final drain must be answered, not lost.
	key := "telegram:late"
	turnHandle, ok := h.loop.claim(key, nil)
	if !ok {
		t.Fatal("claim failed")
	}
	h.loop.claim(key, &bus.InboundMessage{Channel: "telegram", ChatID: "late", Content: "too late"})
	for _, missed := range h.loop.release(key, turnHandle) {
		h.bus.PublishInbound(missed)
	}
	select {
	case out := <-h.bus.Outbound():
		if out.ChatID != "late" {
			t.Errorf("outbound = %+v", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late steering message was never answered")
	}
}

func TestProcessDirectRespectsCancellationWhileWaiting(t *testing.T) {
	h := newHarness(t)
	key := "cli:busy-cancel"
	held, ok := h.loop.claim(key, nil)
	if !ok {
		t.Fatal("claim failed")
	}
	defer h.loop.release(key, held)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	if _, err := h.loop.ProcessDirect(ctx, "hello", key); err == nil {
		t.Error("ProcessDirect returned success despite cancellation")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("ProcessDirect ignored cancellation for too long")
	}
}
