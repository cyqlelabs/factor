package evals

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/session"
	"github.com/cyqlelabs/factor/internal/skills"
	"github.com/cyqlelabs/factor/internal/tools"
)

// model plays the part the model would. It is handed the request exactly as it
// crossed the seam, which is the point: an eval that read the prompt from the
// code that built it could not tell whether the prompt reached the model.
type model func(step int, req *provider.Request) *provider.Response

// scripted is the provider seam, capturing every request for the checks.
type scripted struct {
	mu       sync.Mutex
	fn       model
	requests []*provider.Request
	err      error
}

func (s *scripted) Chat(_ context.Context, req *provider.Request) (*provider.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The request is copied because the loop keeps appending to the slice it
	// passed, and a check that ran afterwards would otherwise see the end of
	// the turn in every step.
	snapshot := *req
	snapshot.Messages = append([]provider.Message(nil), req.Messages...)
	s.requests = append(s.requests, &snapshot)
	if s.err != nil {
		return nil, s.err
	}
	resp := s.fn(len(s.requests)-1, &snapshot)
	if resp.Model == "" {
		resp.Model = "eval-model"
	}
	return resp, nil
}

func (s *scripted) seen() []*provider.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*provider.Request(nil), s.requests...)
}

// env is one eval's world: a real loop, a real registry, real files.
type env struct {
	t         *testing.T
	cfg       *config.Config
	loop      *agent.Loop
	chat      *scripted
	registry  *tools.Registry
	sessions  *session.Store
	workspace string
}

func newEnv(t *testing.T, fn model) *env {
	t.Helper()
	t.Setenv("FACTOR_HOME", t.TempDir())

	cfg := config.Default()
	cfg.Agent.Workspace = t.TempDir()
	cfg.Agent.MaxToolIterations = 6
	cfg.Agent.MaxContextTokens = 1 << 20 // compaction off unless a case asks
	cfg.Memory.Mode = "off"
	cfg.Trace.Enabled = false
	if err := config.EnsureWorkspace(cfg.Agent.Workspace); err != nil {
		t.Fatal(err)
	}

	store, err := session.NewStore(filepath.Join(cfg.Agent.Workspace, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	ambient := memory.NewAmbient(memory.Noop{}, 5, 0.3, 5, 500, 500, nil,
		memory.SpacePolicy{Strategy: "origin", Main: "main", System: "system"})

	registry := tools.NewRegistry(cfg.Tools.IsToolEnabled, cfg.FilterSecrets)
	guard := tools.NewPathGuard(cfg.Agent.Workspace, true, false, nil)
	registry.Register(tools.NewFSTools(guard)...)

	skillsRoot := filepath.Join(cfg.Agent.Workspace, "skills")
	loader := skills.NewLoader(skillsRoot)
	builder := agent.NewContextBuilder(cfg, loader, ambient)

	chat := &scripted{fn: fn}
	loop := agent.NewLoop(cfg, bus.New(), chat, registry, store, builder, ambient)

	return &env{t: t, cfg: cfg, loop: loop, chat: chat, registry: registry,
		sessions: store, workspace: cfg.Agent.Workspace}
}

// say runs one turn and returns the reply.
func (e *env) say(sessionKey, content string) (string, error) {
	e.t.Helper()
	return e.loop.ProcessDirect(context.Background(), content, sessionKey)
}

// lastRequest is what the model was sent on the final step.
func (e *env) lastRequest() *provider.Request {
	e.t.Helper()
	reqs := e.chat.seen()
	if len(reqs) == 0 {
		e.t.Fatal("the model was never called")
	}
	return reqs[len(reqs)-1]
}

// systemText is everything the request said in system role, joined.
func systemText(req *provider.Request) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role == "system" {
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// userText is everything the request said in user role, joined. Factor's
// per-turn context is a user message on purpose, so a check for it has to
// look here rather than in the system prompt.
func userText(req *provider.Request) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role == "user" {
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// answer is the ordinary scripted reply: say something, call nothing.
func answer(text string) model {
	return func(int, *provider.Request) *provider.Response {
		return &provider.Response{Content: text, FinishReason: "stop"}
	}
}

// callThen scripts a tool call on the first step and an answer after.
func callThen(name string, args map[string]any, text string) model {
	return func(step int, _ *provider.Request) *provider.Response {
		if step == 0 {
			return &provider.Response{ToolCalls: []provider.ToolCall{
				{ID: "c1", Name: name, Args: args},
			}}
		}
		return &provider.Response{Content: text, FinishReason: "stop"}
	}
}

// writeSkill puts a skill in the workspace catalog.
func (e *env) writeSkill(name, description, body string) {
	e.t.Helper()
	dir := filepath.Join(e.workspace, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	doc := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(doc), 0o644); err != nil {
		e.t.Fatal(err)
	}
}
