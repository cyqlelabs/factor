package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// --- tool interface contract ---

// builtinTools constructs one of every built-in tool, through the same
// constructors the composition root uses.
func builtinTools(t *testing.T) []Tool {
	t.Helper()
	g := testGuard(t)
	et, err := NewExecTool(g, time.Second, true, nil)
	if err != nil {
		t.Fatalf("NewExecTool: %v", err)
	}
	all := append([]Tool{}, NewFSTools(g)...)
	all = append(all, NewWebTools()...)
	all = append(all, NewConfigTools(testConfig(t))...)
	return append(all, NewPkgInstallTool(), et)
}

func TestBuiltinToolSetIsComplete(t *testing.T) {
	var got []string
	for _, tool := range builtinTools(t) {
		got = append(got, tool.Name())
	}
	sort.Strings(got)
	want := []string{
		"config_get", "config_set", "edit_file", "exec", "list_dir",
		"pkg_install", "read_file", "web_fetch", "web_search", "write_file",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("built-in tool names = %v, want %v", got, want)
	}
}

// TestToolContract asserts the invariants the registry and the LLM rely on:
// a usable name, a description the model can route on, and a parameter schema
// that is a serializable JSON-Schema object whose required keys are declared.
func TestToolContract(t *testing.T) {
	for _, tool := range builtinTools(t) {
		t.Run(tool.Name(), func(t *testing.T) {
			if strings.TrimSpace(tool.Name()) == "" {
				t.Fatal("Name() is empty")
			}
			if strings.TrimSpace(tool.Description()) == "" {
				t.Error("Description() is empty; the model has nothing to route on")
			}

			schema := tool.Parameters()
			if schema == nil {
				t.Fatal("Parameters() is nil")
			}
			if schema["type"] != "object" {
				t.Errorf(`Parameters()["type"] = %v, want "object"`, schema["type"])
			}
			props, ok := schema["properties"].(map[string]any)
			if !ok {
				t.Fatalf(`Parameters()["properties"] = %T, want map[string]any`, schema["properties"])
			}
			for key, spec := range props {
				if _, ok := spec.(map[string]any); !ok {
					t.Errorf("property %q spec = %T, want map[string]any", key, spec)
				}
			}
			if _, err := json.Marshal(schema); err != nil {
				t.Errorf("Parameters() is not JSON-serializable: %v", err)
			}

			required, _ := schema["required"].([]any)
			for _, r := range required {
				key, ok := r.(string)
				if !ok {
					t.Errorf("required entry %v is %T, want string", r, r)
					continue
				}
				if _, declared := props[key]; !declared {
					t.Errorf("required key %q is not declared in properties", key)
				}
			}

			// The schema must agree with the validator that actually gates calls.
			err := ValidateArgs(schema, map[string]any{})
			switch {
			case len(required) > 0 && err == nil:
				t.Error("schema declares required keys but ValidateArgs accepts empty args")
			case len(required) == 0 && err != nil:
				t.Errorf("schema declares no required keys but ValidateArgs rejects empty args: %v", err)
			}
		})
	}
}

func TestToolDefinitionsExposeEveryBuiltin(t *testing.T) {
	r := NewRegistry(nil, nil)
	r.Register(builtinTools(t)...)
	defs := r.Definitions()
	if len(defs) != 10 {
		t.Fatalf("definitions = %d, want 10", len(defs))
	}
	for _, d := range defs {
		if d.Name == "" || d.Description == "" || d.Parameters == nil {
			t.Errorf("definition %+v is incomplete", d)
		}
	}
}

func TestNewWebToolsAreBoundedAndConfigured(t *testing.T) {
	ws := NewWebTools()
	if len(ws) != 2 {
		t.Fatalf("NewWebTools returned %d tools, want 2", len(ws))
	}
	fetch, ok := ws[0].(*webFetchTool)
	if !ok {
		t.Fatalf("first web tool = %T, want *webFetchTool", ws[0])
	}
	if fetch.client == nil || fetch.client.Timeout <= 0 {
		t.Error("web_fetch has no client timeout; a hung server would stall the agent")
	}
	search, ok := ws[1].(*webSearchTool)
	if !ok {
		t.Fatalf("second web tool = %T, want *webSearchTool", ws[1])
	}
	if search.client == nil || search.client.Timeout <= 0 {
		t.Error("web_search has no client timeout")
	}
	if !strings.HasPrefix(search.searchURL, "https://") {
		t.Errorf("search endpoint %q is not https", search.searchURL)
	}
}

// --- arg helpers ---

func TestIntArg(t *testing.T) {
	args := map[string]any{"jsonNum": 7.9, "goInt": 3, "str": "5", "null": nil, "bool": true}
	cases := []struct {
		name string
		key  string
		want int
	}{
		{"json number truncates toward zero", "jsonNum", 7},
		{"native int passes through", "goInt", 3},
		{"string falls back to the default", "str", -1},
		{"nil falls back to the default", "null", -1},
		{"bool falls back to the default", "bool", -1},
		{"missing key falls back to the default", "absent", -1},
	}
	for _, c := range cases {
		if got := IntArg(args, c.key, -1); got != c.want {
			t.Errorf("%s: IntArg(%q) = %d, want %d", c.name, c.key, got, c.want)
		}
	}
}

func TestFloatArg(t *testing.T) {
	args := map[string]any{"jsonNum": 2.5, "goInt": 4, "str": "1.5", "null": nil}
	cases := []struct {
		name string
		key  string
		want float64
	}{
		{"json number passes through", "jsonNum", 2.5},
		{"native int widens to float", "goInt", 4},
		{"string falls back to the default", "str", -1},
		{"nil falls back to the default", "null", -1},
		{"missing key falls back to the default", "absent", -1},
	}
	for _, c := range cases {
		if got := FloatArg(args, c.key, -1); got != c.want {
			t.Errorf("%s: FloatArg(%q) = %v, want %v", c.name, c.key, got, c.want)
		}
	}
}

func TestBoolArg(t *testing.T) {
	args := map[string]any{"yes": true, "no": false, "str": "true", "num": 1.0}
	cases := []struct {
		name string
		key  string
		def  bool
		want bool
	}{
		{"true wins over a false default", "yes", false, true},
		{"false wins over a true default", "no", true, false},
		{"string is not coerced", "str", false, false},
		{"number is not coerced", "num", false, false},
		{"missing key falls back to the default", "absent", true, true},
	}
	for _, c := range cases {
		if got := BoolArg(args, c.key, c.def); got != c.want {
			t.Errorf("%s: BoolArg(%q, %v) = %v, want %v", c.name, c.key, c.def, got, c.want)
		}
	}
}

func TestStringArg(t *testing.T) {
	args := map[string]any{"s": "value", "num": 1.0, "null": nil}
	for key, want := range map[string]string{"s": "value", "num": "", "null": "", "absent": ""} {
		if got := StringArg(args, key); got != want {
			t.Errorf("StringArg(%q) = %q, want %q", key, got, want)
		}
	}
}

// --- tool context ---

func TestToolContextRoundTrip(t *testing.T) {
	want := ToolContext{Channel: "telegram", ChatID: "42", SessionKey: "sess-1"}
	if got := ToolContextFrom(WithToolContext(context.Background(), want)); got != want {
		t.Errorf("ToolContextFrom = %+v, want %+v", got, want)
	}
}

func TestToolContextFromBareContextIsZero(t *testing.T) {
	if got := ToolContextFrom(context.Background()); got != (ToolContext{}) {
		t.Errorf("bare context yielded %+v, want the zero ToolContext", got)
	}
}

func TestToolContextIgnoresForeignKeys(t *testing.T) {
	type foreignKey struct{}
	ctx := context.WithValue(context.Background(), foreignKey{}, ToolContext{Channel: "spoofed"})
	if got := ToolContextFrom(ctx); got != (ToolContext{}) {
		t.Errorf("value under a foreign key leaked into ToolContextFrom: %+v", got)
	}
}

func TestToolContextInnermostValueWins(t *testing.T) {
	outer := WithToolContext(context.Background(), ToolContext{Channel: "cli"})
	inner := WithToolContext(outer, ToolContext{Channel: "http"})
	if got := ToolContextFrom(inner).Channel; got != "http" {
		t.Errorf("channel = %q, want the innermost value %q", got, "http")
	}
}

// --- registry ---

type nilResultTool struct{}

func (nilResultTool) Name() string                                    { return "nilres" }
func (nilResultTool) Description() string                             { return "returns nil" }
func (nilResultTool) Parameters() map[string]any                      { return nil }
func (nilResultTool) Execute(context.Context, map[string]any) *Result { return nil }

func TestRegistryUnregister(t *testing.T) {
	r := NewRegistry(nil, nil)
	r.Register(echoTool{}, panicTool{})

	r.Unregister("boom", "was-never-registered")

	if names := r.Names(); len(names) != 1 || names[0] != "echo" {
		t.Errorf("names after Unregister = %v, want [echo]", names)
	}
	if _, ok := r.Get("boom"); ok {
		t.Error("unregistered tool is still retrievable")
	}
	res := r.Execute(context.Background(), "boom", nil)
	if !res.IsError || !strings.Contains(res.ForLLM, "unknown tool") {
		t.Errorf("calling an unregistered tool = %+v", res)
	}
}

func TestRegistryUnregisterWithNoNamesIsANoOp(t *testing.T) {
	r := NewRegistry(nil, nil)
	r.Register(echoTool{})
	r.Unregister()
	if _, ok := r.Get("echo"); !ok {
		t.Error("Unregister() with no names dropped a tool")
	}
}

func TestRegistryLaterRegistrationWins(t *testing.T) {
	r := NewRegistry(nil, nil)
	r.Register(echoTool{})
	r.Register(shoutTool{})
	res := r.Execute(context.Background(), "echo", map[string]any{"text": "hi"})
	if res.ForLLM != "SHOUT: hi" {
		t.Errorf("result = %q, want the later registration to win", res.ForLLM)
	}
}

// shoutTool deliberately shares echoTool's name to test replacement.
type shoutTool struct{}

func (shoutTool) Name() string               { return "echo" }
func (shoutTool) Description() string        { return "louder" }
func (shoutTool) Parameters() map[string]any { return echoTool{}.Parameters() }
func (shoutTool) Execute(_ context.Context, args map[string]any) *Result {
	return Text("SHOUT: " + StringArg(args, "text"))
}

func TestRegistryNilFilterPassesOutputThrough(t *testing.T) {
	r := NewRegistry(nil, nil)
	r.Register(echoTool{})
	res := r.Execute(context.Background(), "echo", map[string]any{"text": "verbatim"})
	if res.IsError || res.ForLLM != "echo: verbatim" {
		t.Errorf("result = %+v, want the output untouched", res)
	}
}

func TestRegistryFilterAppliesToUserFacingOutput(t *testing.T) {
	r := NewRegistry(nil, func(s string) string { return strings.ReplaceAll(s, "sekret", "[redacted]") })
	r.Register(dualOutputTool{})
	res := r.Execute(context.Background(), "dual", nil)
	if strings.Contains(res.ForUser, "sekret") {
		t.Errorf("ForUser was not filtered: %q", res.ForUser)
	}
	if strings.Contains(res.ForLLM, "sekret") {
		t.Errorf("ForLLM was not filtered: %q", res.ForLLM)
	}
}

type dualOutputTool struct{}

func (dualOutputTool) Name() string               { return "dual" }
func (dualOutputTool) Description() string        { return "writes to both surfaces" }
func (dualOutputTool) Parameters() map[string]any { return nil }
func (dualOutputTool) Execute(context.Context, map[string]any) *Result {
	return &Result{ForLLM: "llm sekret", ForUser: "user sekret"}
}

func TestRegistryNilToolResultBecomesPlaceholder(t *testing.T) {
	r := NewRegistry(nil, nil)
	r.Register(nilResultTool{})
	res := r.Execute(context.Background(), "nilres", nil)
	if res == nil {
		t.Fatal("Execute returned nil")
	}
	if res.IsError || res.ForLLM != "(no output)" {
		t.Errorf("result = %+v, want the (no output) placeholder", res)
	}
}

func TestValidateArgsTypes(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"s":       map[string]any{"type": "string"},
			"n":       map[string]any{"type": "number"},
			"i":       map[string]any{"type": "integer"},
			"b":       map[string]any{"type": "boolean"},
			"arr":     map[string]any{"type": "array"},
			"obj":     map[string]any{"type": "object"},
			"exotic":  map[string]any{"type": "null"},
			"untyped": map[string]any{"description": "no type declared"},
		},
	}
	cases := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{"string accepts a string", map[string]any{"s": "x"}, ""},
		{"string rejects a number", map[string]any{"s": 1.0}, `argument "s" must be a string`},
		{"number accepts a json float", map[string]any{"n": 1.5}, ""},
		{"number accepts a native int", map[string]any{"n": 3}, ""},
		{"number rejects a string", map[string]any{"n": "1"}, `argument "n" must be a number`},
		{"number rejects a bool", map[string]any{"n": true}, `argument "n" must be a number`},
		{"integer accepts a whole float", map[string]any{"i": 4.0}, ""},
		{"integer accepts a native int", map[string]any{"i": 4}, ""},
		{"integer rejects a fractional float", map[string]any{"i": 4.5}, `argument "i" must be a integer`},
		{"integer rejects a bool", map[string]any{"i": true}, `argument "i" must be a integer`},
		{"boolean accepts a bool", map[string]any{"b": true}, ""},
		{"boolean rejects the string true", map[string]any{"b": "true"}, `argument "b" must be a boolean`},
		{"array accepts a slice", map[string]any{"arr": []any{"a", 1.0}}, ""},
		{"array accepts an empty slice", map[string]any{"arr": []any{}}, ""},
		{"array rejects a string", map[string]any{"arr": "a"}, `argument "arr" must be a array`},
		{"object accepts a map", map[string]any{"obj": map[string]any{"k": "v"}}, ""},
		{"object rejects an array", map[string]any{"obj": []any{}}, `argument "obj" must be a object`},
		{"unrecognized schema type accepts anything", map[string]any{"exotic": 1.0}, ""},
		{"property without a type accepts anything", map[string]any{"untyped": []any{}}, ""},
		{"nil value skips type checking", map[string]any{"s": nil}, ""},
		{"undeclared argument is tolerated", map[string]any{"unknown": 1.0}, ""},
		{"no arguments at all", map[string]any{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateArgs(schema, c.args)
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("ValidateArgs(%v) = %v, want nil", c.args, err)
			case c.wantErr != "" && err == nil:
				t.Errorf("ValidateArgs(%v) = nil, want %q", c.args, c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Errorf("ValidateArgs(%v) = %q, want it to contain %q", c.args, err, c.wantErr)
			}
		})
	}
}

func TestValidateArgsEnum(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":   map[string]any{"type": "string", "enum": []any{"add", "remove"}},
			"goStyle":  map[string]any{"type": "string", "enum": []string{"on", "off"}},
			"freeform": map[string]any{"type": "string"},
			"count":    map[string]any{"type": "integer", "enum": []any{1, 2}},
		},
	}
	cases := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{"declared value passes", map[string]any{"action": "add"}, ""},
		{"go-style enum is enforced too", map[string]any{"goStyle": "off"}, ""},
		{"invented value is rejected with the options", map[string]any{"action": "delete"},
			`argument "action" must be one of [add, remove], got "delete"`},
		{"enum is case-sensitive", map[string]any{"action": "Add"}, `must be one of [add, remove]`},
		{"empty string is not a free pass", map[string]any{"action": ""}, `must be one of [add, remove]`},
		{"property without an enum is unconstrained", map[string]any{"freeform": "anything"}, ""},
		{"non-string enums are left alone", map[string]any{"count": 3}, ""},
		{"absent key is not checked", map[string]any{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateArgs(schema, c.args)
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("ValidateArgs(%v) = %v, want nil", c.args, err)
			case c.wantErr != "" && err == nil:
				t.Errorf("ValidateArgs(%v) = nil, want %q", c.args, c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Errorf("ValidateArgs(%v) = %q, want it to contain %q", c.args, err, c.wantErr)
			}
		})
	}
}

// A rejected enum reaches the model as a tool result, so the message has to
// carry the valid options or it cannot correct itself.
func TestRegistryEnumErrorReachesTheModelWithOptions(t *testing.T) {
	r := NewRegistry(nil, nil)
	r.Register(enumTool{})
	res := r.Execute(context.Background(), "enum", map[string]any{"mode": "sideways"})
	if !res.IsError {
		t.Fatalf("invalid enum value was accepted: %+v", res)
	}
	for _, want := range []string{"up", "down", "sideways"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("error %q does not mention %q", res.ForLLM, want)
		}
	}
}

type enumTool struct{}

func (enumTool) Name() string        { return "enum" }
func (enumTool) Description() string { return "enum fixture" }
func (enumTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{"type": "string", "enum": []any{"up", "down"}},
		},
	}
}
func (enumTool) Execute(context.Context, map[string]any) *Result {
	return Text("should never run")
}

func TestValidateArgsNilSchemaAcceptsAnything(t *testing.T) {
	if err := ValidateArgs(nil, map[string]any{"anything": []any{1, 2}}); err != nil {
		t.Errorf("nil schema rejected args: %v", err)
	}
}

func TestValidateArgsRequired(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "string"}, "b": map[string]any{"type": "string"}},
		"required":   []any{"a", "b"},
	}
	if err := ValidateArgs(schema, map[string]any{"a": "x", "b": "y"}); err != nil {
		t.Errorf("all required args present but rejected: %v", err)
	}
	err := ValidateArgs(schema, map[string]any{"a": "x"})
	if err == nil || !strings.Contains(err.Error(), `missing required argument "b"`) {
		t.Errorf("missing arg error = %v", err)
	}
	// present-but-nil still counts as supplied
	if err := ValidateArgs(schema, map[string]any{"a": "x", "b": nil}); err != nil {
		t.Errorf("explicit nil for a required arg was rejected: %v", err)
	}
}

// --- path guard ---

func TestGuardResolveRequiresAPath(t *testing.T) {
	g := testGuard(t)
	if _, err := g.Resolve(""); err == nil || !strings.Contains(err.Error(), "path is required") {
		t.Errorf("Resolve(\"\") error = %v", err)
	}
	if _, err := g.CheckRead(""); err == nil {
		t.Error("CheckRead(\"\") was allowed")
	}
	if _, err := g.CheckWrite(""); err == nil {
		t.Error("CheckWrite(\"\") was allowed")
	}
}

func TestGuardResolveExpandsHomeTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if h, err := os.UserHomeDir(); err != nil || h != home {
		t.Skip("HOME is not the source of truth for os.UserHomeDir on this platform")
	}
	g := testGuard(t)

	got, err := g.Resolve("~/notes/todo.md")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(realHome, "notes", "todo.md"); got != want {
		t.Errorf("Resolve(\"~/notes/todo.md\") = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, g.Workspace()+string(filepath.Separator)) {
		t.Error("tilde path was treated as workspace-relative")
	}
}

func TestGuardResolveOnlyExpandsTildeSlash(t *testing.T) {
	g := testGuard(t)
	got, err := g.Resolve("~")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := filepath.Join(g.Workspace(), "~"); got != want {
		t.Errorf("Resolve(\"~\") = %q, want the literal workspace-relative %q", got, want)
	}
}

func TestGuardResolveIsIdempotentForAbsolutePaths(t *testing.T) {
	g := testGuard(t)
	first, err := g.Resolve("nested/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	second, err := g.Resolve(first)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("Resolve is not idempotent: %q then %q", first, second)
	}
}

func TestGuardUnrestrictedAllowsAnyPath(t *testing.T) {
	g := NewPathGuard(t.TempDir(), false, false, nil)
	if _, err := g.CheckWrite("/tmp/anywhere.txt"); err != nil {
		t.Errorf("unrestricted guard denied a write: %v", err)
	}
	if _, err := g.CheckRead("/etc/hostname"); err != nil {
		t.Errorf("unrestricted guard denied a read: %v", err)
	}
}
