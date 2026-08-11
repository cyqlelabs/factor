package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

// TestMain lets the test binary act as a fake MCP server when re-executed.
func TestMain(m *testing.M) {
	if os.Getenv("FACTOR_TEST_MCP_SERVER") == "1" {
		fakeServerMain()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeServerMain speaks newline-delimited JSON-RPC on stdio: initialize,
// tools/list (one echo tool), tools/call.
func fakeServerMain() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	reply := func(id any, result any) {
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		fmt.Fprintf(out, "%s\n", data)
		out.Flush()
	}
	for scanner.Scan() {
		var req struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			reply(req.ID, map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "1.0"},
			})
		case "notifications/initialized":
			// notification: no response
		case "tools/list":
			reply(req.ID, map[string]any{"tools": []map[string]any{{
				"name":        "echo",
				"description": "Echo back the input",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"text": map[string]any{"type": "string"}},
					"required":   []any{"text"},
				},
			}}})
		case "tools/call":
			text, _ := req.Params["arguments"].(map[string]any)["text"].(string)
			if text == "fail" {
				reply(req.ID, map[string]any{
					"content": []map[string]any{{"type": "text", "text": "simulated failure"}},
					"isError": true,
				})
				continue
			}
			reply(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": "echo: " + text}},
				"isError": false,
			})
		}
	}
}

func selfServer() (string, []string, map[string]string) {
	return os.Args[0], []string{"-test.run=NONE"}, map[string]string{"FACTOR_TEST_MCP_SERVER": "1"}
}

func TestClientHandshakeListCall(t *testing.T) {
	cmd, args, env := selfServer()
	client, err := Connect(context.Background(), "fake", cmd, args, env)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	specs, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Name != "echo" {
		t.Fatalf("specs = %+v", specs)
	}

	text, isErr, err := client.CallTool(context.Background(), "echo", map[string]any{"text": "hello"})
	if err != nil || isErr || text != "echo: hello" {
		t.Fatalf("call = %q %v %v", text, isErr, err)
	}
	text, isErr, err = client.CallTool(context.Background(), "echo", map[string]any{"text": "fail"})
	if err != nil || !isErr || text != "simulated failure" {
		t.Fatalf("error call = %q %v %v", text, isErr, err)
	}
}

func TestConnectFailsOnMissingBinary(t *testing.T) {
	if _, err := Connect(context.Background(), "x", "/nonexistent-mcp-server", nil, nil); err == nil {
		t.Error("missing binary accepted")
	}
}

func newTestManager(t *testing.T) (*Manager, *tools.Registry, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FACTOR_HOME", dir)
	cfg := config.Default()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry(nil, nil)
	return NewManager(registry, cfg), registry, cfg
}

func TestManagerMountsAndUnmountsTools(t *testing.T) {
	m, registry, _ := newTestManager(t)
	cmd, args, env := selfServer()
	if err := m.Connect(context.Background(), "fake", config.MCPServer{Command: cmd, Args: args, Env: env}); err != nil {
		t.Fatal(err)
	}
	defer m.CloseAll()

	if _, ok := registry.Get("fake__echo"); !ok {
		t.Fatalf("tool not mounted; registry has %v", registry.Names())
	}
	res := registry.Execute(context.Background(), "fake__echo", map[string]any{"text": "mounted"})
	if res.IsError || res.ForLLM != "echo: mounted" {
		t.Fatalf("execute = %+v", res)
	}

	if err := m.Disconnect("fake"); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Get("fake__echo"); ok {
		t.Error("tool still mounted after disconnect")
	}
}

func TestMCPAddToolPersistsConfig(t *testing.T) {
	m, registry, cfg := newTestManager(t)
	defer m.CloseAll()
	cmd, args, env := selfServer()

	addRes := (&addTool{m}).Execute(context.Background(), map[string]any{
		"name":    "fake",
		"command": cmd,
		"args":    toAnySlice(args),
		"env":     map[string]any{"FACTOR_TEST_MCP_SERVER": env["FACTOR_TEST_MCP_SERVER"]},
	})
	if addRes.IsError {
		t.Fatalf("add = %+v", addRes)
	}
	if _, ok := registry.Get("fake__echo"); !ok {
		t.Error("tool not mounted after mcp_add")
	}
	saved, err := config.Load(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.MCP.Servers["fake"]; !ok {
		t.Error("server not persisted to config")
	}

	// duplicate add fails cleanly
	if res := (&addTool{m}).Execute(context.Background(), map[string]any{"name": "fake", "command": cmd}); !res.IsError {
		t.Error("duplicate add accepted")
	}

	rmRes := (&removeTool{m}).Execute(context.Background(), map[string]any{"name": "fake"})
	if rmRes.IsError {
		t.Fatalf("remove = %+v", rmRes)
	}
	saved, _ = config.Load(cfg.Path())
	if _, ok := saved.MCP.Servers["fake"]; ok {
		t.Error("server still in config after mcp_remove")
	}
}

func TestListToolOutput(t *testing.T) {
	m, _, _ := newTestManager(t)
	res := (&listTool{m}).Execute(context.Background(), nil)
	if !strings.Contains(res.ForLLM, "No MCP servers") {
		t.Errorf("empty list = %+v", res)
	}
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// guard against the fake server writing into the real workspace
func TestNoStrayFiles(t *testing.T) {
	if _, err := os.Stat(filepath.Join(".", "memory.db")); err == nil {
		t.Error("test created stray memory.db")
	}
}
