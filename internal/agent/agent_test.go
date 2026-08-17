package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/session"
	"github.com/cyqlelabs/factor/internal/skills"
	"github.com/cyqlelabs/factor/internal/tools"
)

// --- fakes ---

type scriptedChat struct {
	mu       sync.Mutex
	requests []*provider.Request
	script   []func(req *provider.Request) (*provider.Response, error)
	call     int
}

func (s *scriptedChat) Chat(_ context.Context, req *provider.Request) (*provider.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	if s.call >= len(s.script) {
		return &provider.Response{Content: "default final"}, nil
	}
	fn := s.script[s.call]
	s.call++
	return fn(req)
}

type fakeEngine struct {
	mu         sync.Mutex
	remembered []string
	spaces     []string
	recalls    []memory.Memory
	healthy    bool
}

func (f *fakeEngine) Remember(_ context.Context, req memory.RememberRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remembered = append(f.remembered, req.Content)
	f.spaces = append(f.spaces, req.Space)
	return "atom", nil
}
func (f *fakeEngine) Recall(context.Context, string, int, float64, memory.Scope) ([]memory.Memory, error) {
	return f.recalls, nil
}
func (f *fakeEngine) Forget(context.Context, string, string, string) error { return nil }
func (f *fakeEngine) Reflect(context.Context) (map[string]any, error)      { return nil, nil }
func (f *fakeEngine) Status(context.Context) (map[string]any, error)       { return nil, nil }
func (f *fakeEngine) Enabled() bool                                        { return true }
func (f *fakeEngine) Healthy() bool                                        { return f.healthy }
func (f *fakeEngine) Close() error                                         { return nil }

type recordTool struct {
	mu    sync.Mutex
	calls []map[string]any
	block chan struct{} // when set, Execute blocks until closed
}

func (r *recordTool) Name() string        { return "probe" }
func (r *recordTool) Description() string { return "test probe" }
func (r *recordTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
		"required":   []any{"value"},
	}
}
func (r *recordTool) Execute(_ context.Context, args map[string]any) *tools.Result {
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, args)
	return tools.Textf("probe saw %v", args["value"])
}

type harness struct {
	loop     *Loop
	chat     *scriptedChat
	engine   *fakeEngine
	store    *session.Store
	bus      *bus.MessageBus
	registry *tools.Registry
	tool     *recordTool
}

func newHarness(t *testing.T, script ...func(req *provider.Request) (*provider.Response, error)) *harness {
	t.Helper()
	t.Setenv("FACTOR_HOME", t.TempDir()) // the loop records the last chat under it
	cfg := config.Default()
	cfg.Agent.Workspace = t.TempDir()
	cfg.Agent.MaxToolIterations = 5
	cfg.Agent.SummarizeAtMessages = 1000
	cfg.Agent.KeepRecentMessages = 2
	if err := config.EnsureWorkspace(cfg.Agent.Workspace); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(cfg.Agent.Workspace, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	chat := &scriptedChat{script: script}
	engine := &fakeEngine{healthy: true}
	ambient := memory.NewAmbient(engine, 5, 0.3, 5, 500, 500, []string{"^HEARTBEAT_OK$"},
		memory.SpacePolicy{Strategy: "origin", Main: "main", System: "system"})
	registry := tools.NewRegistry(cfg.Tools.IsToolEnabled, nil)
	probe := &recordTool{}
	registry.Register(probe)
	builder := NewContextBuilder(cfg, skills.NewLoader(filepath.Join(cfg.Agent.Workspace, "skills")), ambient)
	b := bus.New()
	return &harness{
		loop:     NewLoop(cfg, b, chat, registry, store, builder, ambient),
		chat:     chat,
		engine:   engine,
		store:    store,
		bus:      b,
		registry: registry,
		tool:     probe,
	}
}

func final(content string) func(*provider.Request) (*provider.Response, error) {
	return func(*provider.Request) (*provider.Response, error) {
		return &provider.Response{Content: content}, nil
	}
}

func toolCall(name string, args map[string]any) func(*provider.Request) (*provider.Response, error) {
	return func(*provider.Request) (*provider.Response, error) {
		return &provider.Response{ToolCalls: []provider.ToolCall{{ID: "tc1", Name: name, Args: args}}}, nil
	}
}

// --- tests ---

func TestSimpleTurn(t *testing.T) {
	h := newHarness(t, final("hello there"))
	reply, err := h.loop.ProcessDirect(context.Background(), "hi", "cli:test")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "hello there" {
		t.Errorf("reply = %q", reply)
	}
	history, _ := h.store.History("cli:test")
	if len(history) != 2 || history[0].Role != "user" || history[1].Role != "assistant" {
		t.Fatalf("history = %+v", history)
	}
	// system prompt present and carries identity + memory section is separate
	req := h.chat.requests[0]
	if req.Messages[0].Role != "system" || !strings.Contains(req.Messages[0].Content, "You are Factor") {
		t.Errorf("system prompt = %q", req.Messages[0].Content[:80])
	}
}

func TestToolLoopTurn(t *testing.T) {
	h := newHarness(t,
		toolCall("probe", map[string]any{"value": "abc"}),
		func(req *provider.Request) (*provider.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if last.Role != "tool" || !strings.Contains(last.Content, "probe saw abc") {
				t.Errorf("tool result not fed back: %+v", last)
			}
			return &provider.Response{Content: "done with tools"}, nil
		},
	)
	reply, err := h.loop.ProcessDirect(context.Background(), "use the probe", "cli:test")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "done with tools" {
		t.Errorf("reply = %q", reply)
	}
	if len(h.tool.calls) != 1 || h.tool.calls[0]["value"] != "abc" {
		t.Errorf("tool calls = %+v", h.tool.calls)
	}
	history, _ := h.store.History("cli:test")
	// user, assistant(tool_calls), tool, assistant(final)
	if len(history) != 4 || history[2].Role != "tool" || history[2].ToolCallID != "tc1" {
		t.Fatalf("history = %+v", history)
	}
}

func TestMemoryInjectionAndStorage(t *testing.T) {
	h := newHarness(t, final("ok"))
	h.engine.recalls = []memory.Memory{{
		Content: "never force-push to main", Severity: memory.SeverityCriticalWarning, Confidence: 0.9,
	}}
	if _, err := h.loop.ProcessDirect(context.Background(), "deploy please", "cli:test"); err != nil {
		t.Fatal(err)
	}
	sys := h.chat.requests[0].Messages[0].Content
	if !strings.Contains(sys, "YOU MUST NOT repeat this past mistake") || !strings.Contains(sys, "never force-push") {
		t.Errorf("memory not injected:\n%s", sys)
	}
	// ambient storage is async
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.engine.mu.Lock()
		n := len(h.engine.remembered)
		h.engine.mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			if n != 2 {
				t.Fatalf("remembered %d items, want 2", n)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// a cli conversation writes to the main space
	h.engine.mu.Lock()
	spaces := append([]string(nil), h.engine.spaces...)
	h.engine.mu.Unlock()
	if len(spaces) != 2 || spaces[0] != "main" || spaces[1] != "main" {
		t.Errorf("cli turn stored into spaces %v, want [main main]", spaces)
	}
}

// Machine-originated turns must not pollute conversational memory: the loop
// hands the channel to StoreExchange, and the origin policy routes cron turns
// into the system space.
func TestCronTurnStoresIntoTheSystemSpace(t *testing.T) {
	h := newHarness(t, final("done"))
	if _, err := h.loop.ProcessDirect(context.Background(), "nightly digest ran", "cron:digest"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.engine.mu.Lock()
		spaces := append([]string(nil), h.engine.spaces...)
		h.engine.mu.Unlock()
		if len(spaces) == 2 {
			if spaces[0] != "system" || spaces[1] != "system" {
				t.Fatalf("cron turn stored into spaces %v, want [system system]", spaces)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stored %d memories, want 2", len(spaces))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// spinner is a scripted response that always asks for another tool call, so
// the turn walks straight into the iteration limit.
func spinner(*provider.Request) (*provider.Response, error) {
	return &provider.Response{ToolCalls: []provider.ToolCall{{ID: "x", Name: "probe", Args: map[string]any{"value": "again"}}}}, nil
}

func TestIterationLimitWrapsUp(t *testing.T) {
	h := newHarness(t) // empty script
	h.chat.script = nil
	for range 5 { // MaxToolIterations
		h.chat.script = append(h.chat.script, spinner)
	}
	h.chat.script = append(h.chat.script, func(req *provider.Request) (*provider.Response, error) {
		if len(req.Tools) != 0 {
			t.Errorf("wrap-up offered %d tools, want none", len(req.Tools))
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role != "user" || !strings.Contains(last.Content, "no further tool calls are possible") {
			t.Errorf("wrap-up nudge missing: %+v", last)
		}
		return &provider.Response{Content: "the probe kept saying again; nothing verified yet"}, nil
	})

	reply, err := h.loop.ProcessDirect(context.Background(), "loop forever", "cli:test")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "the probe kept saying again; nothing verified yet" {
		t.Errorf("reply = %q", reply)
	}
	if len(h.chat.requests) != 6 {
		t.Errorf("LLM called %d times, want 5 tool rounds plus one wrap-up", len(h.chat.requests))
	}
	history, _ := h.store.History("cli:test")
	last := history[len(history)-1]
	if last.Role != "assistant" || last.Content != reply {
		t.Errorf("wrap-up answer not persisted: %+v", last)
	}
}

func TestIterationLimitFallsBackWhenWrapUpFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wrapUp func(*provider.Request) (*provider.Response, error)
	}{
		{"provider error", func(*provider.Request) (*provider.Response, error) {
			return nil, errors.New("provider down")
		}},
		{"empty answer", func(*provider.Request) (*provider.Response, error) {
			return &provider.Response{Content: "  "}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.chat.script = nil
			for range 5 {
				h.chat.script = append(h.chat.script, spinner)
			}
			h.chat.script = append(h.chat.script, tc.wrapUp)

			reply, err := h.loop.ProcessDirect(context.Background(), "loop forever", "cli:test")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(reply, "tool-iteration limit") {
				t.Errorf("reply = %q", reply)
			}
		})
	}
}

func TestSteeringViaBus(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.tool.block = release
	h.chat.script = []func(*provider.Request) (*provider.Response, error){
		toolCall("probe", map[string]any{"value": "slow"}),
		func(req *provider.Request) (*provider.Response, error) {
			var steered bool
			for _, m := range req.Messages {
				if m.Role == "user" && m.Content == "also do this" {
					steered = true
				}
			}
			if !steered {
				t.Error("steering message not injected into live turn")
			}
			return &provider.Response{Content: "handled both"}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.loop.Run(ctx); close(done) }()

	h.bus.PublishInbound(bus.InboundMessage{Channel: "telegram", ChatID: "42", Content: "first task"})
	// wait until the turn is live (blocked inside the tool), then steer
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.loop.mu.Lock()
		_, live := h.loop.active["telegram:42"]
		h.loop.mu.Unlock()
		if live || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.bus.PublishInbound(bus.InboundMessage{Channel: "telegram", ChatID: "42", Content: "also do this"})
	time.Sleep(50 * time.Millisecond) // let dispatch route it to steering
	close(release)

	select {
	case out := <-h.bus.Outbound():
		if out.Content != "handled both" || out.Channel != "telegram" {
			t.Errorf("outbound = %+v", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no outbound reply")
	}
	cancel()
	<-done
}

func TestContextOverflowTriggersCompaction(t *testing.T) {
	h := newHarness(t)
	key := "cli:long"
	for i := range 8 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		_ = h.store.Append(key, provider.Message{Role: role, Content: strings.Repeat("x", 50)})
	}
	h.chat.script = []func(*provider.Request) (*provider.Response, error){
		func(*provider.Request) (*provider.Response, error) {
			return nil, &provider.APIError{Provider: "p", Reason: provider.ReasonContextOverflow, Status: 400}
		},
		final("summary of the past"), // compaction summarize call
		func(req *provider.Request) (*provider.Response, error) {
			for _, m := range req.Messages {
				if m.Role == "system" && strings.Contains(m.Content, "summary of the past") {
					return &provider.Response{Content: "recovered"}, nil
				}
			}
			t.Error("summary not included after compaction")
			return &provider.Response{Content: "recovered"}, nil
		},
	}
	reply, err := h.loop.ProcessDirect(context.Background(), "continue", key)
	if err != nil {
		t.Fatal(err)
	}
	if reply != "recovered" {
		t.Errorf("reply = %q", reply)
	}
	if h.store.Summary(key) != "summary of the past" {
		t.Errorf("summary = %q", h.store.Summary(key))
	}
	history, _ := h.store.History(key)
	if len(history) > 6 {
		t.Errorf("history not truncated: %d messages", len(history))
	}
}

func TestEphemeralTurnLeavesNoTrace(t *testing.T) {
	h := newHarness(t, final("HEARTBEAT_OK"))
	reply, err := h.loop.ProcessEphemeral(context.Background(), "# Heartbeat\ncheck tasks")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "HEARTBEAT_OK" {
		t.Errorf("reply = %q", reply)
	}
	keys, _ := h.store.List()
	if len(keys) != 0 {
		t.Errorf("ephemeral turn persisted sessions: %v", keys)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.remembered) != 0 {
		t.Errorf("ephemeral turn stored memories: %v", h.engine.remembered)
	}
}

func TestFindSafeBoundary(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user"}, // 0
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "a"}}}, // 1
		{Role: "tool", ToolCallID: "a"},                                // 2
		{Role: "assistant"},                                            // 3
		{Role: "user"},                                                 // 4
		{Role: "assistant"},                                            // 5
	}
	if got := findSafeBoundary(msgs, 2); got != 4 {
		t.Errorf("boundary from 2 = %d, want 4 (skip tool pair)", got)
	}
	if got := findSafeBoundary(msgs, 5); got != -1 {
		t.Errorf("boundary from 5 = %d, want -1", got)
	}
}

func TestContextBuilderCachingAndInstructions(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Workspace = t.TempDir()
	if err := config.EnsureWorkspace(cfg.Agent.Workspace); err != nil {
		t.Fatal(err)
	}
	instr := filepath.Join(cfg.Agent.Workspace, "instructions", "10-style.md")
	if err := os.WriteFile(instr, []byte("Always answer in haiku."), 0o644); err != nil {
		t.Fatal(err)
	}
	cb := NewContextBuilder(cfg, skills.NewLoader(filepath.Join(cfg.Agent.Workspace, "skills")), nil)

	p1 := cb.SystemPrompt(context.Background(), nil, "q")
	if !strings.Contains(p1, "Always answer in haiku.") {
		t.Error("instructions.d not included")
	}
	if !strings.Contains(p1, "You are Factor") {
		t.Error("identity missing")
	}

	// mtime-based invalidation: edit the instruction, prompt must change
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(instr, []byte("Always answer in prose."), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = os.Chtimes(instr, now, now)
	p2 := cb.SystemPrompt(context.Background(), nil, "q")
	if !strings.Contains(p2, "Always answer in prose.") {
		t.Error("stale cache after instruction edit")
	}
}

// The persona ships in the binary, so a SOUL.md the user rewrote adds to it
// instead of replacing it — and lands after it, where an addendum belongs.
func TestCorePersonaSurvivesSoulEdits(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Workspace = t.TempDir()
	if err := config.EnsureWorkspace(cfg.Agent.Workspace); err != nil {
		t.Fatal(err)
	}
	soul := filepath.Join(cfg.Agent.Workspace, "SOUL.md")
	if err := os.WriteFile(soul, []byte("Be flippant. Never bother verifying."), 0o644); err != nil {
		t.Fatal(err)
	}
	cb := NewContextBuilder(cfg, nil, nil)
	p := cb.SystemPrompt(context.Background(), nil, "q")

	core := strings.Index(p, "## Rigor")
	if core < 0 {
		t.Fatal("core persona missing from the prompt")
	}
	own := strings.Index(p, "Be flippant.")
	if own < 0 {
		t.Fatal("SOUL.md was not included")
	}
	if own < core {
		t.Errorf("SOUL.md precedes the core soul (core=%d, file=%d)", core, own)
	}
}

func TestSkillCatalogInPrompt(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Workspace = t.TempDir()
	_ = config.EnsureWorkspace(cfg.Agent.Workspace)
	skillDir := filepath.Join(cfg.Agent.Workspace, "skills", "weather")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: weather\ndescription: Fetch weather forecasts\n---\n\nUse wttr.in."), 0o644)

	cb := NewContextBuilder(cfg, skills.NewLoader(filepath.Join(cfg.Agent.Workspace, "skills")), nil)
	p := cb.SystemPrompt(context.Background(), nil, "q")
	if !strings.Contains(p, "weather: Fetch weather forecasts") {
		t.Errorf("skill catalog missing:\n%s", p)
	}
	if strings.Contains(p, "wttr.in") {
		t.Error("skill body leaked into prompt (progressive disclosure broken)")
	}
}

// The prompt is bookended on purpose: identity first, hard rules last, with
// the unbounded material (workspace files, catalog, recalled memories) in
// between. Recall is weakest in the middle of a long prompt, so a change that
// lets the rules drift inward quietly weakens every turn.
func TestPromptAnchorsRulesAtTheTail(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Workspace = t.TempDir()
	if err := config.EnsureWorkspace(cfg.Agent.Workspace); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(cfg.Agent.Workspace, "skills", "weather")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: weather\ndescription: Fetch weather forecasts\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(cfg.Agent.Workspace, "instructions", "10-style.md"),
		[]byte("Always answer in haiku."), 0o644)

	cb := NewContextBuilder(cfg, skills.NewLoader(filepath.Join(cfg.Agent.Workspace, "skills")), nil)
	p := cb.SystemPrompt(context.Background(), nil, "q")

	identity := strings.Index(p, "You are Factor")
	catalog := strings.Index(p, "weather: Fetch weather forecasts")
	rules := strings.Index(p, "Rules:")
	if identity != 0 {
		t.Errorf("identity starts at %d, want the very top of the prompt", identity)
	}
	if catalog < 0 || rules < 0 {
		t.Fatalf("prompt is missing the catalog (%d) or the rules (%d):\n%s", catalog, rules, p)
	}
	if rules < catalog {
		t.Error("rules appear before the skills catalog; they must close the prompt")
	}
	if tail := strings.TrimSpace(p[rules:]); strings.Count(tail, "\n\n") > 0 {
		t.Errorf("rules are not the final block; %q follows them", tail)
	}
}

// Compaction is where an agent forgets what it did. The transcript has to
// carry the identifiers a tool call produced — the path, the skill name — or
// the summary records that a file was written and not which one.
func TestSummarizeArgsKeepsIdentifiersAndBoundsBulk(t *testing.T) {
	got := summarizeArgs(map[string]any{
		"path":    "/root/.factor/workspace/skills/cast_media/SKILL.md",
		"content": strings.Repeat("x", 5000),
	})
	if !strings.Contains(got, "/root/.factor/workspace/skills/cast_media/SKILL.md") {
		t.Errorf("path was lost or truncated: %q", got)
	}
	if strings.Count(got, "x") > maxArgChars {
		t.Errorf("bulk content was not bounded: %d chars of it survived", strings.Count(got, "x"))
	}
	if len(got) > maxArgsPerCall+maxArgChars+64 {
		t.Errorf("rendering is unbounded at %d chars: %q", len(got), got)
	}

	// Stable ordering, so a summary diff reflects real change, not map order.
	for range 8 {
		if again := summarizeArgs(map[string]any{"b": "2", "a": "1", "c": "3"}); again != "(a=1, b=2, c=3)" {
			t.Fatalf("unstable or malformed rendering: %q", again)
		}
	}
	if got := summarizeArgs(nil); got != "" {
		t.Errorf("no args should render as nothing, got %q", got)
	}
	if got := summarizeArgs(map[string]any{"empty": ""}); got != "" {
		t.Errorf("empty values should be dropped, got %q", got)
	}
	// Newlines would break the one-line-per-call transcript format.
	if got := summarizeArgs(map[string]any{"cmd": "line one\nline two"}); strings.Contains(got, "\n") {
		t.Errorf("newline leaked into the transcript line: %q", got)
	}
}

func TestCompactionSurvivesConcurrentAppends(t *testing.T) {
	// Regression: the truncation offset must be absolute. Messages appended
	// while the summarize LLM call is in flight must stay in live history,
	// and the cut must hold the boundary chosen before the call.
	h := newHarness(t)
	key := "cli:busy"
	for i := range 10 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		_ = h.store.Append(key, provider.Message{Role: role, Content: fmt.Sprintf("old-%d", i)})
	}
	h.chat.script = []func(*provider.Request) (*provider.Response, error){
		func(*provider.Request) (*provider.Response, error) {
			// mid-summarize: another turn appends to the same session
			_ = h.store.Append(key, provider.Message{Role: "user", Content: "raced-user"})
			_ = h.store.Append(key, provider.Message{Role: "assistant", Content: "raced-reply"})
			return &provider.Response{Content: "the summary"}, nil
		},
	}
	if err := h.loop.compact(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	history, err := h.store.History(key)
	if err != nil {
		t.Fatal(err)
	}
	var contents []string
	for _, m := range history {
		contents = append(contents, m.Content)
	}
	joined := strings.Join(contents, ",")
	if !strings.Contains(joined, "raced-user") || !strings.Contains(joined, "raced-reply") {
		t.Fatalf("messages appended during summarize were truncated: %v", contents)
	}
	if history[0].Role != "user" {
		t.Errorf("cut drifted off the turn-safe boundary; history starts with %q", history[0].Role)
	}
	if h.store.Summary(key) != "the summary" {
		t.Errorf("summary = %q", h.store.Summary(key))
	}
}

func TestProcessDirectSerializesPerSession(t *testing.T) {
	// Regression: direct turns honor the one-live-turn claim, so overlapping
	// cron firings (same session key) cannot interleave histories.
	h := newHarness(t)
	release := make(chan struct{})
	h.tool.block = release
	h.chat.script = []func(*provider.Request) (*provider.Response, error){
		toolCall("probe", map[string]any{"value": "slow"}),
		final("first done"),
		final("second done"),
	}
	key := "cron:job1"
	firstDone := make(chan string, 1)
	go func() {
		reply, _ := h.loop.ProcessDirect(context.Background(), "first", key)
		firstDone <- reply
	}()
	// wait until the first turn is live (blocked in the tool)
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.loop.mu.Lock()
		_, live := h.loop.active[key]
		h.loop.mu.Unlock()
		if live || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	secondDone := make(chan string, 1)
	go func() {
		reply, _ := h.loop.ProcessDirect(context.Background(), "second", key)
		secondDone <- reply
	}()
	select {
	case <-secondDone:
		t.Fatal("second turn completed while first held the session")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if r := <-firstDone; r != "first done" {
		t.Errorf("first = %q", r)
	}
	if r := <-secondDone; r != "second done" {
		t.Errorf("second = %q", r)
	}
	history, _ := h.store.History(key)
	// serialized: first,asst(tool),tool,asst-final, then second,asst-final
	for i, m := range history {
		if m.Content == "second" && i != 4 {
			t.Errorf("interleaved history: %d %+v", i, history)
		}
	}
}
