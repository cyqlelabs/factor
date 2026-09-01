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
	"github.com/cyqlelabs/factor/internal/cost"
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
func (f *fakeEngine) SpaceSupport() (bool, string)                         { return true, "main" }
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
	cfg.Agent.MaxContextTokens = 1 << 20 // compaction off unless a test asks
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
	block := turnBlock(t, h.chat.requests[0].Messages)
	if !strings.Contains(block, "YOU MUST NOT repeat this past mistake") || !strings.Contains(block, "never force-push") {
		t.Errorf("memory not injected:\n%s", block)
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

func TestWrapUpStripsLeakedToolCall(t *testing.T) {
	h := newHarness(t)
	h.chat.script = nil
	for range 5 {
		h.chat.script = append(h.chat.script, spinner)
	}
	const leaked = "Tengo suficiente información. Preparo el email.<tool_call>\n" +
		"<function=exec>\n<parameter=command>cat cuerpo.txt</parameter>\n</function>\n</tool_call>"
	h.chat.script = append(h.chat.script, func(*provider.Request) (*provider.Response, error) {
		return &provider.Response{Content: leaked}, nil
	})

	reply, err := h.loop.ProcessDirect(context.Background(), "loop forever", "cli:test")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reply, "<tool_call>") || strings.Contains(reply, "<function=") {
		t.Errorf("leaked tool markup shipped to the channel: %q", reply)
	}
	if !strings.Contains(reply, "Preparo el email.") {
		t.Errorf("prose before the leak was lost: %q", reply)
	}
	if !strings.Contains(reply, "exec") {
		t.Errorf("reply does not name the tool that never ran: %q", reply)
	}
	history, _ := h.store.History("cli:test")
	last := history[len(history)-1]
	if last.Role != "assistant" || last.Content != reply {
		t.Errorf("persisted %q, want the cleaned reply", last.Content)
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

	p1 := cb.SystemPrompt()
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
	p2 := cb.SystemPrompt()
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
	p := cb.SystemPrompt()

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
	p := cb.SystemPrompt()
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
	p := cb.SystemPrompt()

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

// ctxChat records the session key each provider call carried, which is what
// the cost meter bills against.
type ctxChat struct {
	inner ChatProvider
	mu    sync.Mutex
	keys  []string
}

func (c *ctxChat) Chat(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	c.mu.Lock()
	c.keys = append(c.keys, tools.ToolContextFrom(ctx).SessionKey)
	c.mu.Unlock()
	return c.inner.Chat(ctx, req)
}

func TestBudgetCapIsAnsweredRatherThanReportedAsABreakage(t *testing.T) {
	h := newHarness(t, func(*provider.Request) (*provider.Response, error) {
		return nil, &cost.BudgetError{Scope: "session", Spent: 2, Limit: 1.5}
	})
	reply, err := h.loop.ProcessDirect(context.Background(), "hi", "cli:test")
	if err != nil {
		t.Fatalf("a cap the user set was raised as an error: %v", err)
	}
	if !strings.Contains(reply, "Budget cap reached") || !strings.Contains(reply, "session_usd") {
		t.Errorf("reply = %q", reply)
	}
	h.loop.WaitBackground(2 * time.Second)
	if len(h.engine.remembered) != 0 {
		t.Errorf("a budget stop was written to memory: %v", h.engine.remembered)
	}
}

func TestEveryCallOfATurnCarriesTheSessionItIsSpentFor(t *testing.T) {
	h := newHarness(t, final("one"), final("two"), final("summary"))
	h.loop.cfg.Agent.MaxContextTokens = 1 // compact after every turn
	watched := &ctxChat{inner: h.loop.chat}
	h.loop.chat = watched

	for _, msg := range []string{"first", "second"} {
		if _, err := h.loop.ProcessDirect(context.Background(), msg, "cli:test"); err != nil {
			t.Fatal(err)
		}
	}
	h.loop.WaitBackground(5 * time.Second)

	watched.mu.Lock()
	defer watched.mu.Unlock()
	if len(watched.keys) < 3 {
		t.Fatalf("calls = %v, want the compaction call among them", watched.keys)
	}
	for i, key := range watched.keys {
		if key != "cli:test" {
			t.Errorf("call %d ran under session %q, so its spend would be billed to nobody", i, key)
		}
	}
}

// turnBlock returns the per-turn block out of a request: everything a turn
// knows on its own rides one user message between the history and what the
// user just said.
func turnBlock(t *testing.T, msgs []provider.Message) string {
	t.Helper()
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, turnContextHeader) {
			if m.Role != "user" {
				t.Fatalf("the per-turn block rode role %q, which the Anthropic dialect would hoist to the head", m.Role)
			}
			return m.Content
		}
	}
	t.Fatal("no per-turn block in the request")
	return ""
}

// turnContextFor builds one turn's per-turn block as the given channel would
// see it.
func turnContextFor(t *testing.T, cb *ContextBuilder, channel string) string {
	t.Helper()
	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{Channel: channel, ChatID: "x", SessionKey: channel + ":x"})
	return cb.TurnContext(ctx, nil, "q", 0)
}

func TestSpokenChannelsAreToldTheyAreHeard(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Workspace = t.TempDir()
	if err := config.EnsureWorkspace(cfg.Agent.Workspace); err != nil {
		t.Fatal(err)
	}
	cb := NewContextBuilder(cfg, skills.NewLoader(filepath.Join(cfg.Agent.Workspace, "skills")), nil)

	for _, tc := range []struct{ channel, want string }{
		{"voice", "spoken aloud on the user's speakers"},
		{"phone", "live phone call"},
		{"cron", "nobody watching"},
	} {
		if got := turnContextFor(t, cb, tc.channel); !strings.Contains(got, tc.want) {
			t.Errorf("a %s turn was not told where it lands", tc.channel)
		}
	}
	// A written channel gets no sentence about being written: it would change
	// nothing and cost tokens on every turn.
	for _, quiet := range []string{"cli", "telegram", "system", ""} {
		got := turnContextFor(t, cb, quiet)
		for _, phrase := range []string{"spoken aloud", "nobody watching"} {
			if strings.Contains(got, phrase) {
				t.Errorf("a %q turn was briefed as %q", quiet, phrase)
			}
		}
	}
}

// The system prompt is one string shared by every session and every turn, and
// prompt caching pays off only while it stays byte-identical. Nothing that
// varies by channel, by clock or by turn may appear in it.
func TestTheSystemPromptIsTheSameForEveryTurn(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Workspace = t.TempDir()
	if err := config.EnsureWorkspace(cfg.Agent.Workspace); err != nil {
		t.Fatal(err)
	}
	cb := NewContextBuilder(cfg, skills.NewLoader(filepath.Join(cfg.Agent.Workspace, "skills")), nil)

	head := cb.SystemPrompt()
	for _, channel := range []string{"voice", "phone", "cron", "cli", "telegram"} {
		turnContextFor(t, cb, channel) // whatever a channel needs, it gets it here
		if again := cb.SystemPrompt(); again != head {
			t.Fatalf("a %s turn rewrote the shared system prompt", channel)
		}
	}
	for _, volatile := range []string{"Current time:", "spoken aloud", "nobody watching", "# Memory"} {
		if strings.Contains(head, volatile) {
			t.Errorf("the cached head carries %q, which changes every turn", volatile)
		}
	}
	if !strings.Contains(head, "Rules:") {
		t.Error("the operating rules left the system prompt")
	}
}

// And it has to survive the trip through the loop: the channel reaches the
// prompt through the tool context the turn runs under, not through an
// argument anyone passes by hand.
func TestATurnCarriesItsChannelIntoThePrompt(t *testing.T) {
	h := newHarness(t, final("said"))
	if _, err := h.loop.ProcessDirect(context.Background(), "hola", "voice:local"); err != nil {
		t.Fatal(err)
	}
	msgs := h.chat.requests[0].Messages
	if msgs[0].Role != "system" {
		t.Fatalf("first message is %q", msgs[0].Role)
	}
	if !strings.Contains(turnBlock(t, msgs), "spoken aloud on the user's speakers") {
		t.Error("a voice turn reached the provider without being told it is heard")
	}

	written := newHarness(t, final("written"))
	if _, err := written.loop.ProcessDirect(context.Background(), "hola", "cli:main"); err != nil {
		t.Fatal(err)
	}
	for _, msg := range written.chat.requests[0].Messages {
		if strings.Contains(msg.Content, "spoken aloud") {
			t.Error("a terminal turn was briefed as a spoken one")
		}
	}
}

// cancelTool stands in for a tool the user talks over: it cancels the turn
// the way the voice channel does, then returns what the browser suite returns
// when its run is cut short.
type cancelTool struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	calls  int
}

func (c *cancelTool) Name() string               { return "navigate" }
func (c *cancelTool) Description() string        { return "test navigate" }
func (c *cancelTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (c *cancelTool) Execute(ctx context.Context, _ map[string]any) *tools.Result {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	c.cancel()
	<-ctx.Done()
	return tools.Errorf("navigate failed: %v", ctx.Err())
}

// TestAnInterruptedTurnIsNotRecordedAsAToolFailure is the guard against the
// worst kind of wrong answer: a turn the user talked over used to persist
// "ERROR: navigate failed: context canceled" as its tool result, and the next
// turn read that back as proof the browser was broken and said so out loud.
func TestAnInterruptedTurnIsNotRecordedAsAToolFailure(t *testing.T) {
	h := newHarness(t, func(*provider.Request) (*provider.Response, error) {
		return &provider.Response{
			Content: "Voy a abrir la página.",
			ToolCalls: []provider.ToolCall{
				{ID: "tc1", Name: "navigate", Args: map[string]any{}},
				{ID: "tc2", Name: "navigate", Args: map[string]any{}},
			},
		}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tool := &cancelTool{cancel: cancel}
	h.registry.Register(tool)

	if _, err := h.loop.ProcessDirect(ctx, "abrí la página", "cli:test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	history, err := h.store.History("cli:test")
	if err != nil {
		t.Fatal(err)
	}
	// Every tool call still needs a result beside it, or the history stops
	// replaying on any provider that validates the pairing.
	answered := map[string]string{}
	for _, m := range history {
		if m.Role == "tool" {
			answered[m.ToolCallID] = m.Content
		}
		if strings.Contains(m.Content, "context canceled") {
			t.Errorf("the cancellation was written into history as content: %q", m.Content)
		}
	}
	for _, id := range []string{"tc1", "tc2"} {
		got, ok := answered[id]
		if !ok {
			t.Fatalf("tool call %s has no result: %+v", id, history)
		}
		if got != interruptedTool {
			t.Errorf("result for %s = %q, want the interrupted note", id, got)
		}
	}
	// The second call is not attempted once the turn is over.
	tool.mu.Lock()
	defer tool.mu.Unlock()
	if tool.calls != 1 {
		t.Errorf("tool ran %d times, want 1", tool.calls)
	}
}

// A named speaker is shown to the model as an annotation on the message and
// remembered as an attribution on the fact — two different jobs, so the tag
// meant for the prompt must not be what lands in the graph.
func TestSpeakerIsMarkedForTheModelAndAttributedInMemory(t *testing.T) {
	h := newHarness(t, final("noted"))

	if _, err := h.loop.ProcessDirectNotice(context.Background(),
		"me gusta el café sin azúcar", "voice:local:roxana", "Roxana", "", nil); err != nil {
		t.Fatal(err)
	}

	// The model is told who is talking, on the message rather than in the
	// shared prompt head.
	sent := h.chat.requests[0].Messages
	user := sent[len(sent)-1]
	if user.Role != "user" || user.Content != "[Roxana] me gusta el café sin azúcar" {
		t.Errorf("the model saw %q from role %q", user.Content, user.Role)
	}

	// Memory records the person, not the prompt's tag.
	waitFor(t, func() bool { return len(h.engine.snapshot()) == 2 })
	stored := h.engine.snapshot()
	if stored[0] != "Roxana: me gusta el café sin azúcar" {
		t.Errorf("remembered %q", stored[0])
	}
}

// The ordinary channel — one chat, one person — is untouched: no tag for the
// model, no name in memory.
func TestAnUnattributedTurnIsUnchanged(t *testing.T) {
	h := newHarness(t, final("noted"))
	if _, err := h.loop.ProcessDirect(context.Background(), "hello there", "cli:test"); err != nil {
		t.Fatal(err)
	}
	sent := h.chat.requests[0].Messages
	if user := sent[len(sent)-1]; user.Content != "hello there" {
		t.Errorf("the model saw %q", user.Content)
	}
	waitFor(t, func() bool { return len(h.engine.snapshot()) == 2 })
	if stored := h.engine.snapshot(); stored[0] != "hello there" {
		t.Errorf("remembered %q", stored[0])
	}
}

// snapshot copies what the engine has been asked to remember, under its lock.
func (f *fakeEngine) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.remembered...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Qwen on the live box kept asking for the same exec twice in one batch, which
// ran the command twice and spent two of the twenty iterations on one answer.
func TestDuplicateToolCallsInOneBatchRunOnce(t *testing.T) {
	h := newHarness(t,
		func(*provider.Request) (*provider.Response, error) {
			return &provider.Response{ToolCalls: []provider.ToolCall{
				{ID: "a", Name: "probe", Args: map[string]any{"value": "ls"}},
				{ID: "b", Name: "probe", Args: map[string]any{"value": "ls"}},
				{ID: "c", Name: "probe", Args: map[string]any{"value": "ls -la"}},
			}}, nil
		},
		final("done"))
	if _, err := h.loop.ProcessDirect(context.Background(), "look", "cli:dup"); err != nil {
		t.Fatal(err)
	}
	if got := len(h.tool.calls); got != 2 {
		t.Errorf("tool ran %d times, want 2: the repeat of the same arguments is one call", got)
	}
	// Every id still needs its own result, or replaying the history breaks.
	history, err := h.store.History("cli:dup")
	if err != nil {
		t.Fatal(err)
	}
	answered := map[string]string{}
	for _, m := range history {
		if m.Role == "tool" {
			answered[m.ToolCallID] = m.Content
		}
	}
	if len(answered) != 3 {
		t.Fatalf("tool results = %v, want one per call id", answered)
	}
	if answered["a"] != answered["b"] {
		t.Errorf("the repeated call got %q, want the first answer %q", answered["b"], answered["a"])
	}
	if answered["c"] == answered["a"] {
		t.Errorf("different arguments must still run: both got %q", answered["c"])
	}
}
