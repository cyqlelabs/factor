package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

// Server variants the test binary can re-exec into, selected by
// FACTOR_TEST_MCP_MODE (see TestMain in mcp_test.go).
const (
	modeRich         = "rich"          // advertises the adapter edge-case tools below
	modeBadHandshake = "bad-handshake" // fails the initialize request
	modeBadList      = "bad-list"      // answers tools/list with the wrong shape
)

// bigResultBytes overshoots the registry's result cap so Execute must truncate.
const bigResultBytes = 40 * 1024

// richToolSpecs is what the rich server advertises from tools/list. Note that
// "noschema" deliberately omits inputSchema so the adapter's empty-object
// fallback is exercised.
var richToolSpecs = []map[string]any{
	{"name": "noschema", "description": "Advertises no input schema"},
	{
		"name":        "big",
		"description": "Returns more text than the result cap allows",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{"name": "quiet", "description": "Returns no text content at all"},
	{"name": "twoblocks", "description": "Returns several content blocks"},
	{"name": "nonstandard", "description": "Returns a result that is not the standard content shape"},
	{"name": "boom", "description": "Returns a JSON-RPC error"},
	{"name": "vanish", "description": "Exits the process instead of replying"},
	{"name": "hang", "description": "Never replies"},
}

func richServer(mode string) (string, []string, map[string]string) {
	return os.Args[0], []string{"-test.run=NONE"}, map[string]string{
		"FACTOR_TEST_MCP_SERVER": "1",
		"FACTOR_TEST_MCP_MODE":   mode,
	}
}

func richServerConfig(mode string) config.MCPServer {
	cmd, args, env := richServer(mode)
	return config.MCPServer{Command: cmd, Args: args, Env: env}
}

func echoServerConfig() config.MCPServer {
	cmd, args, env := selfServer()
	return config.MCPServer{Command: cmd, Args: args, Env: env}
}

// richServerMain speaks the same newline-delimited JSON-RPC as fakeServerMain
// but covers the adapter and transport edge cases.
func richServerMain(mode string) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	emit := func(msg map[string]any) {
		data, _ := json.Marshal(msg)
		fmt.Fprintf(out, "%s\n", data)
		_ = out.Flush()
	}
	result := func(id any, res any) {
		emit(map[string]any{"jsonrpc": "2.0", "id": id, "result": res})
	}
	rpcError := func(id any, code int, message string) {
		emit(map[string]any{"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": code, "message": message}})
	}
	textResult := func(id any, text string) {
		result(id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": false,
		})
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
			if mode == modeBadHandshake {
				rpcError(req.ID, -32603, "handshake refused")
				continue
			}
			// Real servers interleave log lines and notifications with their
			// responses; the client must skip them.
			fmt.Fprint(out, "\n")
			fmt.Fprint(out, "starting up, not JSON at all\n")
			emit(map[string]any{"jsonrpc": "2.0", "method": "notifications/message",
				"params": map[string]any{"level": "info", "data": "no id here"}})
			result(req.ID, map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "rich", "version": "1.0"},
			})
		case "notifications/initialized":
			// notification: no response
		case "tools/list":
			if mode == modeBadList {
				result(req.ID, "not the catalog you were expecting")
				continue
			}
			result(req.ID, map[string]any{"tools": richToolSpecs})
		case "tools/call":
			name, _ := req.Params["name"].(string)
			switch name {
			case "big":
				textResult(req.ID, strings.Repeat("A", bigResultBytes))
			case "quiet":
				result(req.ID, map[string]any{"content": []map[string]any{}, "isError": false})
			case "twoblocks":
				result(req.ID, map[string]any{"content": []map[string]any{
					{"type": "text", "text": "first"},
					{"type": "image", "data": "ignored"},
					{"type": "text", "text": "second"},
				}, "isError": false})
			case "nonstandard":
				result(req.ID, []any{"not", "an", "object"})
			case "boom":
				rpcError(req.ID, -32000, "tool exploded")
			case "vanish":
				_ = out.Flush()
				os.Exit(0)
			case "hang":
				// never replies
			default:
				textResult(req.ID, "ok:"+name)
			}
		}
	}
}

// connectRich wires a manager to the rich server and returns both the manager
// and its registry.
func connectRich(t *testing.T, name string) (*Manager, *tools.Registry) {
	t.Helper()
	m, registry, _ := newTestManager(t)
	t.Cleanup(m.CloseAll)
	if err := m.Connect(context.Background(), name, richServerConfig(modeRich)); err != nil {
		t.Fatal(err)
	}
	return m, registry
}

func TestStartAllConnectsEnabledServersAndSkipsTheRest(t *testing.T) {
	m, registry, cfg := newTestManager(t)
	t.Cleanup(m.CloseAll)
	disabled := false
	off := echoServerConfig()
	off.Enabled = &disabled
	cfg.MCP.Servers = map[string]config.MCPServer{
		"good":   echoServerConfig(),
		"off":    off,
		"broken": {Command: "/nonexistent-mcp-server"},
	}

	m.StartAll(context.Background())

	status := m.Status()
	if len(status) != 1 || status["good"] != 1 {
		t.Fatalf("Status() = %v, want only the enabled server with 1 tool", status)
	}
	if _, ok := registry.Get("good__echo"); !ok {
		t.Errorf("enabled server's tool not mounted; registry = %v", registry.Names())
	}
	if _, ok := registry.Get("off__echo"); ok {
		t.Error("a disabled server was connected")
	}

	m.CloseAll()

	if status := m.Status(); len(status) != 0 {
		t.Errorf("Status() after CloseAll = %v, want empty", status)
	}
	if _, ok := registry.Get("good__echo"); ok {
		t.Error("tool still mounted after CloseAll")
	}
}

func TestDisconnectUnknownServerReturnsAnError(t *testing.T) {
	m, _, _ := newTestManager(t)
	err := m.Disconnect("never-connected")
	if err == nil {
		t.Fatal("Disconnect accepted a server that was never connected")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("error = %q, want it to say the server is not connected", err)
	}
}

func TestMCPToolAdapterExposesNamespacedNameAndPrefixedDescription(t *testing.T) {
	_, registry := connectRich(t, "rich")

	tool, ok := registry.Get("rich__noschema")
	if !ok {
		t.Fatalf("rich__noschema not mounted; registry = %v", registry.Names())
	}
	if tool.Name() != "rich__noschema" {
		t.Errorf("Name() = %q, want rich__noschema", tool.Name())
	}
	if want := "[MCP rich] Advertises no input schema"; tool.Description() != want {
		t.Errorf("Description() = %q, want %q", tool.Description(), want)
	}
	if len(registry.Names()) < len(richToolSpecs) {
		t.Errorf("registry = %v, want all %d rich tools mounted", registry.Names(), len(richToolSpecs))
	}
}

func TestMCPToolParametersFallBackToAnEmptyObjectSchema(t *testing.T) {
	_, registry := connectRich(t, "rich")

	tool, _ := registry.Get("rich__noschema")
	params := tool.Parameters()
	if params["type"] != "object" {
		t.Errorf("Parameters()[type] = %v, want object", params["type"])
	}
	props, ok := params["properties"].(map[string]any)
	if !ok || len(props) != 0 {
		t.Errorf("Parameters()[properties] = %v, want an empty object", params["properties"])
	}

	// A server-supplied schema must pass through untouched.
	withSchema, _ := registry.Get("rich__big")
	if got := withSchema.Parameters(); got["type"] != "object" {
		t.Errorf("advertised schema not passed through: %v", got)
	}
}

func TestMCPToolTruncatesOversizedResults(t *testing.T) {
	_, registry := connectRich(t, "rich")

	res := registry.Execute(context.Background(), "rich__big", nil)
	if res.IsError {
		t.Fatalf("execute = %+v", res)
	}
	if len(res.ForLLM) >= bigResultBytes {
		t.Errorf("result is %d bytes, want it truncated below the %d byte source", len(res.ForLLM), bigResultBytes)
	}
	// One cap, applied to every tool alike: an MCP server cannot flood the
	// context by the back door, and what it says when it does is the same
	// sentence every other tool gets.
	if !strings.Contains(res.ForLLM, fmt.Sprintf("of %d characters shown", bigResultBytes)) {
		t.Errorf("result does not say what it withheld: %q", res.ForLLM[max(len(res.ForLLM)-200, 0):])
	}
	if !strings.HasPrefix(res.ForLLM, strings.Repeat("A", 64)) {
		t.Error("truncation dropped the head of the result")
	}
}

func TestMCPToolReportsAnEmptyResult(t *testing.T) {
	_, registry := connectRich(t, "rich")

	res := registry.Execute(context.Background(), "rich__quiet", nil)
	if res.IsError || res.ForLLM != "(empty result)" {
		t.Errorf("execute = %+v, want the empty-result placeholder", res)
	}
}

func TestMCPToolFailsWhenTheServerIsGone(t *testing.T) {
	m, registry := connectRich(t, "rich")

	tool, ok := registry.Get("rich__noschema")
	if !ok {
		t.Fatal("rich__noschema not mounted")
	}
	if err := m.Disconnect("rich"); err != nil {
		t.Fatal(err)
	}

	res := tool.Execute(context.Background(), map[string]any{})
	if !res.IsError || !strings.Contains(res.ForLLM, "not running") {
		t.Errorf("execute against a closed client = %+v, want a 'not running' error", res)
	}
}

func TestMCPToolSurfacesCallFailures(t *testing.T) {
	_, registry := connectRich(t, "rich")

	res := registry.Execute(context.Background(), "rich__boom", nil)
	if !res.IsError || !strings.Contains(res.ForLLM, "tool exploded") {
		t.Errorf("execute = %+v, want the server's JSON-RPC error surfaced", res)
	}
}

func TestNewToolsExposesTheSelfManagementSet(t *testing.T) {
	m, _, _ := newTestManager(t)

	set := NewTools(m)
	want := []string{"mcp_add", "mcp_remove", "mcp_list"}
	if len(set) != len(want) {
		t.Fatalf("NewTools() returned %d tools, want %d", len(set), len(want))
	}
	for i, tool := range set {
		if tool.Name() != want[i] {
			t.Errorf("tool %d = %q, want %q", i, tool.Name(), want[i])
		}
		if tool.Description() == "" {
			t.Errorf("tool %q has no description", tool.Name())
		}
		params := tool.Parameters()
		if params["type"] != "object" {
			t.Errorf("tool %q schema type = %v, want object", tool.Name(), params["type"])
		}
		if _, ok := params["properties"].(map[string]any); !ok {
			t.Errorf("tool %q schema has no properties object", tool.Name())
		}
	}
}

func TestMCPAddRejectsInvalidServerNames(t *testing.T) {
	m, registry, _ := newTestManager(t)
	t.Cleanup(m.CloseAll)

	for _, name := range []string{"", "   ", "has__separator"} {
		res := (&addTool{m}).Execute(context.Background(), map[string]any{
			"name": name, "command": "/nonexistent-mcp-server",
		})
		if !res.IsError || !strings.Contains(res.ForLLM, "invalid server name") {
			t.Errorf("mcp_add(%q) = %+v, want an invalid-name error", name, res)
		}
	}
	if len(registry.Names()) != 0 {
		t.Errorf("registry = %v, want nothing mounted by rejected adds", registry.Names())
	}
	if len(m.Status()) != 0 {
		t.Errorf("Status() = %v, want no server connected by rejected adds", m.Status())
	}
}

func TestMCPRemoveRejectsAServerThatIsNeitherConnectedNorConfigured(t *testing.T) {
	m, _, _ := newTestManager(t)

	res := (&removeTool{m}).Execute(context.Background(), map[string]any{"name": "ghost"})
	if !res.IsError || !strings.Contains(res.ForLLM, "not connected") {
		t.Errorf("mcp_remove(ghost) = %+v, want a not-connected error", res)
	}
}

func TestMCPRemoveDropsAConfiguredButDisconnectedServer(t *testing.T) {
	m, _, cfg := newTestManager(t)
	if err := config.Update(cfg.Path(), func(c *config.Config) error {
		c.MCP.Servers = map[string]config.MCPServer{"stale": {Command: "/nonexistent-mcp-server"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	res := (&removeTool{m}).Execute(context.Background(), map[string]any{"name": "stale"})
	if res.IsError {
		t.Fatalf("mcp_remove(stale) = %+v, want success for a configured server", res)
	}
	saved, err := config.Load(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.MCP.Servers["stale"]; ok {
		t.Error("server still in config after mcp_remove")
	}
}

func TestMCPAddInitialisesAConfigWithNoServersMap(t *testing.T) {
	m, _, cfg := newTestManager(t)
	t.Cleanup(m.CloseAll)
	if err := os.WriteFile(cfg.Path(), []byte(`{"mcp":{"servers":null}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, args, env := selfServer()

	res := (&addTool{m}).Execute(context.Background(), map[string]any{
		"name": "fake", "command": cmd, "args": toAnySlice(args),
		"env": map[string]any{"FACTOR_TEST_MCP_SERVER": env["FACTOR_TEST_MCP_SERVER"]},
	})
	if res.IsError {
		t.Fatalf("mcp_add = %+v", res)
	}
	saved, err := config.Load(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.MCP.Servers["fake"]; !ok {
		t.Error("server not persisted into a config that had a null servers map")
	}
}

// blockConfigWrites puts a directory where the config file belongs, so every
// config.Update fails.
func blockConfigWrites(t *testing.T, cfg *config.Config) {
	t.Helper()
	if err := os.MkdirAll(cfg.Path(), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestMCPAddReportsAConfigSaveFailureAfterConnecting(t *testing.T) {
	m, registry, cfg := newTestManager(t)
	t.Cleanup(m.CloseAll)
	blockConfigWrites(t, cfg)
	cmd, args, env := selfServer()

	res := (&addTool{m}).Execute(context.Background(), map[string]any{
		"name": "fake", "command": cmd, "args": toAnySlice(args),
		"env": map[string]any{"FACTOR_TEST_MCP_SERVER": env["FACTOR_TEST_MCP_SERVER"]},
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "config save failed") {
		t.Errorf("mcp_add = %+v, want a config-save failure", res)
	}
	if _, ok := registry.Get("fake__echo"); !ok {
		t.Error("the server connected, so its tools should still be mounted")
	}
}

func TestMCPRemoveReportsAConfigSaveFailureAfterDisconnecting(t *testing.T) {
	m, registry, cfg := newTestManager(t)
	t.Cleanup(m.CloseAll)
	if err := m.Connect(context.Background(), "fake", echoServerConfig()); err != nil {
		t.Fatal(err)
	}
	blockConfigWrites(t, cfg)

	res := (&removeTool{m}).Execute(context.Background(), map[string]any{"name": "fake"})
	if !res.IsError || !strings.Contains(res.ForLLM, "config save failed") {
		t.Errorf("mcp_remove = %+v, want a config-save failure", res)
	}
	if _, ok := registry.Get("fake__echo"); ok {
		t.Error("tools should be unmounted even when the config save fails")
	}
}

func TestListToolReportsConnectedServersSorted(t *testing.T) {
	m, _, _ := newTestManager(t)
	t.Cleanup(m.CloseAll)
	for _, name := range []string{"beta", "alpha"} {
		if err := m.Connect(context.Background(), name, echoServerConfig()); err != nil {
			t.Fatal(err)
		}
	}

	res := (&listTool{m}).Execute(context.Background(), nil)
	if res.IsError {
		t.Fatalf("mcp_list = %+v", res)
	}
	if !strings.Contains(res.ForLLM, "alpha: 1 tools") || !strings.Contains(res.ForLLM, "beta: 1 tools") {
		t.Errorf("mcp_list = %q, want both servers with their tool counts", res.ForLLM)
	}
	if strings.Index(res.ForLLM, "alpha") > strings.Index(res.ForLLM, "beta") {
		t.Errorf("mcp_list = %q, want servers listed in sorted order", res.ForLLM)
	}
}

func TestConnectFailsWhenTheHandshakeIsRejected(t *testing.T) {
	cmd, args, env := richServer(modeBadHandshake)
	client, err := Connect(context.Background(), "bad", cmd, args, env)
	if err == nil {
		_ = client.Close()
		t.Fatal("Connect accepted a server that rejected initialize")
	}
	if !strings.Contains(err.Error(), "initialize") || !strings.Contains(err.Error(), "handshake refused") {
		t.Errorf("error = %q, want it to name the failed initialize", err)
	}
}

func TestCallToolPassesThroughNonStandardResults(t *testing.T) {
	cmd, args, env := richServer(modeRich)
	client, err := Connect(context.Background(), "rich", cmd, args, env)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	text, isErr, err := client.CallTool(context.Background(), "nonstandard", map[string]any{})
	if err != nil {
		t.Fatalf("CallTool = %v", err)
	}
	if isErr {
		t.Error("a non-standard result should not be reported as a tool error")
	}
	if text != `["not","an","object"]` {
		t.Errorf("text = %q, want the raw JSON result handed through", text)
	}
}

func TestCallToolReturnsAnErrorForAJSONRPCErrorResponse(t *testing.T) {
	cmd, args, env := richServer(modeRich)
	client, err := Connect(context.Background(), "rich", cmd, args, env)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	text, isErr, err := client.CallTool(context.Background(), "boom", map[string]any{})
	if err == nil {
		t.Fatal("CallTool accepted a JSON-RPC error response")
	}
	if !strings.Contains(err.Error(), "tool exploded") || !strings.Contains(err.Error(), "-32000") {
		t.Errorf("error = %q, want the server's message and code", err)
	}
	if !isErr || text != "" {
		t.Errorf("CallTool = (%q, %v), want an empty error result", text, isErr)
	}
}

func TestRequestHonoursTheCallerContext(t *testing.T) {
	cmd, args, env := richServer(modeRich)
	client, err := Connect(context.Background(), "rich", cmd, args, env)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, _, err := client.CallTool(ctx, "hang", map[string]any{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("CallTool against a silent tool = %v, want a deadline error", err)
	}
}

func TestConnectFailsWhenTheToolCatalogIsMalformed(t *testing.T) {
	m, registry, _ := newTestManager(t)
	t.Cleanup(m.CloseAll)

	err := m.Connect(context.Background(), "badlist", richServerConfig(modeBadList))
	if err == nil {
		t.Fatal("Connect accepted a malformed tools/list response")
	}
	if !strings.Contains(err.Error(), "list tools") {
		t.Errorf("error = %q, want it to name the failed tools/list", err)
	}
	if len(m.Status()) != 0 || len(registry.Names()) != 0 {
		t.Error("a server that failed tools/list must not stay mounted")
	}
}

func TestClientIgnoresServerOutputThatIsNotAResponse(t *testing.T) {
	// The rich server emits a blank line, a plain-text log line and an
	// id-less notification before answering initialize.
	cmd, args, env := richServer(modeRich)
	client, err := Connect(context.Background(), "rich", cmd, args, env)
	if err != nil {
		t.Fatalf("noise on stdout broke the handshake: %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.ListTools(context.Background()); err != nil {
		t.Errorf("ListTools after server noise = %v", err)
	}
}

func TestCallToolJoinsMultipleTextBlocks(t *testing.T) {
	cmd, args, env := richServer(modeRich)
	client, err := Connect(context.Background(), "rich", cmd, args, env)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	text, isErr, err := client.CallTool(context.Background(), "twoblocks", map[string]any{})
	if err != nil || isErr {
		t.Fatalf("CallTool = %q %v %v", text, isErr, err)
	}
	if text != "first\nsecond" {
		t.Errorf("text = %q, want the text blocks joined by a newline and non-text blocks dropped", text)
	}
}

func TestCallToolFailsWhenArgumentsCannotBeEncoded(t *testing.T) {
	cmd, args, env := richServer(modeRich)
	client, err := Connect(context.Background(), "rich", cmd, args, env)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	if _, _, err := client.CallTool(context.Background(), "noschema", map[string]any{"ch": make(chan int)}); err == nil {
		t.Error("CallTool accepted an argument that cannot be JSON encoded")
	}
}

func TestClientIsNotAliveAfterClose(t *testing.T) {
	cmd, args, env := selfServer()
	client, err := Connect(context.Background(), "fake", cmd, args, env)
	if err != nil {
		t.Fatal(err)
	}
	if !client.Alive() {
		t.Error("client not alive right after a successful handshake")
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if client.Alive() {
		t.Error("client still reports alive after Close")
	}
}

func TestRequestsFailOnceTheServerProcessExits(t *testing.T) {
	cmd, args, env := richServer(modeRich)
	client, err := Connect(context.Background(), "rich", cmd, args, env)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	// The server exits without answering this call.
	if _, _, err := client.CallTool(context.Background(), "vanish", map[string]any{}); err == nil {
		t.Fatal("the in-flight call survived the server exiting")
	}

	deadline := time.Now().Add(3 * time.Second)
	for client.Alive() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if client.Alive() {
		t.Fatal("client still reports alive after the server exited")
	}

	// A request issued after the exit must fail rather than block.
	if _, err := client.ListTools(context.Background()); err == nil {
		t.Error("ListTools succeeded after the server exited")
	}
}
