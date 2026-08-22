package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

// Manager owns MCP server connections and mounts their tools into the
// registry as <server>__<tool>. It also persists server definitions to the
// config file so the agent can extend itself durably.
type Manager struct {
	registry *tools.Registry
	cfg      *config.Config

	mu         sync.Mutex
	clients    map[string]*Client
	toolNames  map[string][]string
	connecting map[string]bool
}

func NewManager(registry *tools.Registry, cfg *config.Config) *Manager {
	return &Manager{
		registry:   registry,
		cfg:        cfg,
		clients:    map[string]*Client{},
		toolNames:  map[string][]string{},
		connecting: map[string]bool{},
	}
}

// StartAll connects every enabled configured server; failures log and skip.
func (m *Manager) StartAll(ctx context.Context) {
	for name, srv := range m.cfg.MCP.Servers {
		if !srv.IsEnabled() {
			continue
		}
		if err := m.Connect(ctx, name, srv); err != nil {
			slog.Error("mcp server failed to connect", "server", name, "error", err)
		}
	}
}

// Connect spawns one server and mounts its tools. A reservation held for
// the whole connect prevents two concurrent adds of the same name from
// spawning a duplicate, orphaned server process.
func (m *Manager) Connect(ctx context.Context, name string, srv config.MCPServer) error {
	m.mu.Lock()
	if _, exists := m.clients[name]; exists || m.connecting[name] {
		m.mu.Unlock()
		return fmt.Errorf("mcp server %q already connected", name)
	}
	m.connecting[name] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.connecting, name)
		m.mu.Unlock()
	}()

	client, err := Connect(ctx, name, srv.Command, srv.Args, srv.Env)
	if err != nil {
		return err
	}
	listCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	specs, err := client.ListTools(listCtx)
	cancel()
	if err != nil {
		_ = client.Close()
		return fmt.Errorf("list tools: %w", err)
	}

	var names []string
	for _, spec := range specs {
		tool := &mcpTool{client: client, server: name, spec: spec}
		m.registry.Register(tool)
		names = append(names, tool.Name())
	}
	m.mu.Lock()
	m.clients[name] = client
	m.toolNames[name] = names
	m.mu.Unlock()
	slog.Info("mcp server connected", "server", name, "tools", len(names))
	return nil
}

// Disconnect closes a server and unmounts its tools.
func (m *Manager) Disconnect(name string) error {
	m.mu.Lock()
	client, ok := m.clients[name]
	names := m.toolNames[name]
	delete(m.clients, name)
	delete(m.toolNames, name)
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("mcp server %q is not connected", name)
	}
	m.registry.Unregister(names...)
	return client.Close()
}

// Status lists connected servers and their tool counts.
func (m *Manager) Status() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for name, tools := range m.toolNames {
		out[name] = len(tools)
	}
	return out
}

// CloseAll disconnects everything (shutdown).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	names := make([]string, 0, len(m.clients))
	for n := range m.clients {
		names = append(names, n)
	}
	m.mu.Unlock()
	for _, n := range names {
		_ = m.Disconnect(n)
	}
}

// mcpTool adapts one remote tool to the tools.Tool seam.
type mcpTool struct {
	client *Client
	server string
	spec   ToolSpec
}

func (t *mcpTool) Name() string { return t.server + "__" + t.spec.Name }
func (t *mcpTool) Description() string {
	return fmt.Sprintf("[MCP %s] %s", t.server, t.spec.Description)
}
func (t *mcpTool) Parameters() map[string]any {
	if t.spec.InputSchema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return t.spec.InputSchema
}
func (t *mcpTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	if !t.client.Alive() {
		return tools.Errorf("mcp server %s is not running", t.server)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	text, isErr, err := t.client.CallTool(ctx, t.spec.Name, args)
	if err != nil {
		return tools.Errorf("mcp call failed: %v", err)
	}
	if text == "" {
		text = "(empty result)"
	}
	// Bounding a runaway result is the registry's job, not this one: every
	// tool's output passes through the same cap on its way back to the loop,
	// and a second limit here would only cut what that one is about to cut
	// again, in a different place and with a different explanation.
	return &tools.Result{ForLLM: text, IsError: isErr}
}

// NewTools returns the MCP self-management tool set.
func NewTools(m *Manager) []tools.Tool {
	return []tools.Tool{&addTool{m}, &removeTool{m}, &listTool{m}}
}

type addTool struct{ m *Manager }

func (t *addTool) Name() string { return "mcp_add" }
func (t *addTool) Description() string {
	return "Connect a new MCP server (stdio command) and mount its tools. The server is saved to config so it reconnects on restart."
}
func (t *addTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":    map[string]any{"type": "string", "description": "Short identifier, e.g. 'github'"},
			"command": map[string]any{"type": "string", "description": "Executable to run"},
			"args":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Arguments passed to the command, one array element each (no shell quoting)"},
			"env":     map[string]any{"type": "object", "description": "Extra environment variables"},
		},
		"required": []any{"name", "command"},
	}
}
func (t *addTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	name := strings.TrimSpace(tools.StringArg(args, "name"))
	if name == "" || strings.Contains(name, "__") {
		return tools.Errorf("invalid server name %q", name)
	}
	srv := config.MCPServer{Command: tools.StringArg(args, "command")}
	if rawArgs, ok := args["args"].([]any); ok {
		for _, a := range rawArgs {
			if s, ok := a.(string); ok {
				srv.Args = append(srv.Args, s)
			}
		}
	}
	if rawEnv, ok := args["env"].(map[string]any); ok {
		srv.Env = map[string]string{}
		for k, v := range rawEnv {
			if s, ok := v.(string); ok {
				srv.Env[k] = s
			}
		}
	}
	if err := t.m.Connect(ctx, name, srv); err != nil {
		return tools.Errorf("connect failed (nothing saved): %v", err)
	}
	// Persist to the config FILE only; the live config stays immutable
	// (concurrent turns read it lock-free).
	err := config.Update(t.m.cfg.Path(), func(c *config.Config) error {
		if c.MCP.Servers == nil {
			c.MCP.Servers = map[string]config.MCPServer{}
		}
		c.MCP.Servers[name] = srv
		return nil
	})
	t.m.mu.Lock()
	count := len(t.m.toolNames[name])
	t.m.mu.Unlock()
	if err != nil {
		return tools.Errorf("connected but config save failed: %v", err)
	}
	return tools.Textf("Connected MCP server %q (%d tools mounted) and saved to config.", name, count)
}

type removeTool struct{ m *Manager }

func (t *removeTool) Name() string { return "mcp_remove" }
func (t *removeTool) Description() string {
	return "Disconnect an MCP server, unmount its tools, and remove it from config."
}
func (t *removeTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": map[string]any{"type": "string", "description": "Server name as shown by mcp_list"}},
		"required":   []any{"name"},
	}
}
func (t *removeTool) Execute(_ context.Context, args map[string]any) *tools.Result {
	name := tools.StringArg(args, "name")
	discErr := t.m.Disconnect(name)
	inConfig := false
	saveErr := config.Update(t.m.cfg.Path(), func(c *config.Config) error {
		_, inConfig = c.MCP.Servers[name]
		delete(c.MCP.Servers, name)
		return nil
	})
	if discErr != nil && !inConfig {
		return tools.Errorf("%v", discErr)
	}
	if saveErr != nil {
		return tools.Errorf("disconnected but config save failed: %v", saveErr)
	}
	return tools.Textf("Removed MCP server %q.", name)
}

type listTool struct{ m *Manager }

func (t *listTool) Name() string        { return "mcp_list" }
func (t *listTool) Description() string { return "List connected MCP servers and their tool counts." }
func (t *listTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *listTool) Execute(context.Context, map[string]any) *tools.Result {
	status := t.m.Status()
	if len(status) == 0 {
		return tools.Text("No MCP servers connected.")
	}
	names := make([]string, 0, len(status))
	for n := range status {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s: %d tools\n", n, status[n])
	}
	return tools.Text(b.String())
}
