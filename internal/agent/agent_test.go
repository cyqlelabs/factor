package agent

import (
	"context"
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
	recalls    []memory.Memory
	healthy    bool
}

func (f *fakeEngine) Remember(_ context.Context, req memory.RememberRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remembered = append(f.remembered, req.Content)
	return "atom", nil
}
func (f *fakeEngine) Recall(context.Context, string, int, float64) ([]memory.Memory, error) {
	return f.recalls, nil
}
func (f *fakeEngine) Forget(context.Context, string, string) error    { return nil }
func (f *fakeEngine) Reflect(context.Context) (map[string]any, error) { return nil, nil }
func (f *fakeEngine) Status(context.Context) (map[string]any, error)  { return nil, nil }
func (f *fakeEngine) Healthy() bool                                   { return f.healthy }
func (f *fakeEngine) Close() error                                    { return nil }

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
	ambient := memory.NewAmbient(engine, 5, 0.3, 5, 500, 500, []string{"^HEARTBEAT_OK$"})
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
}

func TestIterationLimit(t *testing.T) {
	h := newHarness(t) // empty script
	h.chat.script = nil
	loops := func(*provider.Request) (*provider.Response, error) {
		return &provider.Response{ToolCalls: []provider.ToolCall{{ID: "x", Name: "probe", Args: map[string]any{"value": "again"}}}}, nil
	}
	for range 10 {
		h.chat.script = append(h.chat.script, loops)
	}
	reply, err := h.loop.ProcessDirect(context.Background(), "loop forever", "cli:test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "tool-iteration limit") {
		t.Errorf("reply = %q", reply)
	}
	if len(h.chat.requests) != 5 {
		t.Errorf("LLM called %d times, want MaxToolIterations=5", len(h.chat.requests))
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
