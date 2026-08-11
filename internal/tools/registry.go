package tools

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/cyqlelabs/factor/internal/provider"
)

// Registry holds the active tool arsenal. It applies the user's disabled
// list, validates arguments against each tool's JSON schema, recovers tool
// panics, and filters secrets out of tool output.
type Registry struct {
	mu      sync.RWMutex
	tools   map[string]Tool
	enabled func(name string) bool
	filter  func(s string) string
}

func NewRegistry(enabled func(string) bool, filter func(string) string) *Registry {
	if enabled == nil {
		enabled = func(string) bool { return true }
	}
	if filter == nil {
		filter = func(s string) string { return s }
	}
	return &Registry{tools: map[string]Tool{}, enabled: enabled, filter: filter}
}

// Register adds a tool unless the user disabled it. Registering the same
// name twice replaces the earlier tool (later registrations win).
func (r *Registry) Register(ts ...Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range ts {
		if !r.enabled(t.Name()) {
			slog.Info("tool disabled by config", "tool", t.Name())
			continue
		}
		r.tools[t.Name()] = t
	}
}

// Unregister removes tools by name (used when an MCP server goes away).
func (r *Registry) Unregister(names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range names {
		delete(r.tools, n)
	}
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Definitions returns the schema list sent to the LLM, sorted for stable
// prompts (stability helps provider-side prompt caching).
func (r *Registry) Definitions() []provider.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	defs := make([]provider.ToolDefinition, 0, len(names))
	for _, n := range names {
		t := r.tools[n]
		defs = append(defs, provider.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return defs
}

// Execute runs a tool call end to end. It never panics and never returns nil.
func (r *Registry) Execute(ctx context.Context, name string, args map[string]any) (result *Result) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("tool panicked", "tool", name, "panic", rec)
			result = Errorf("tool %s panicked: %v", name, rec)
		}
		if result != nil {
			result.ForLLM = r.filter(result.ForLLM)
			result.ForUser = r.filter(result.ForUser)
		}
	}()

	t, ok := r.Get(name)
	if !ok {
		return Errorf("unknown tool: %s (available: %v)", name, r.Names())
	}
	if args == nil {
		args = map[string]any{}
	}
	if err := ValidateArgs(t.Parameters(), args); err != nil {
		return Errorf("invalid arguments for %s: %v", name, err)
	}
	res := t.Execute(ctx, args)
	if res == nil {
		res = Text("(no output)")
	}
	return res
}

// ValidateArgs checks required fields and primitive types against a JSON
// schema fragment. It is intentionally minimal: enough to catch the common
// LLM mistakes (missing field, wrong type) with clear messages.
func ValidateArgs(schema, args map[string]any) error {
	if schema == nil {
		return nil
	}
	// Accept both JSON-decoded ([]any) and hand-written Go ([]string) schemas
	// so a new tool cannot silently lose required-argument checking.
	for _, name := range requiredNames(schema["required"]) {
		if _, present := args[name]; !present {
			return fmt.Errorf("missing required argument %q", name)
		}
	}
	props, _ := schema["properties"].(map[string]any)
	for key, raw := range args {
		spec, ok := props[key].(map[string]any)
		if !ok {
			continue // tolerate extra args
		}
		want, _ := spec["type"].(string)
		if want == "" || raw == nil {
			continue
		}
		if !typeMatches(want, raw) {
			return fmt.Errorf("argument %q must be a %s", key, want)
		}
	}
	return nil
}

func requiredNames(raw any) []string {
	switch req := raw.(type) {
	case []any:
		out := make([]string, 0, len(req))
		for _, r := range req {
			if name, ok := r.(string); ok {
				out = append(out, name)
			}
		}
		return out
	case []string:
		return req
	}
	return nil
}

func typeMatches(want string, v any) bool {
	switch want {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		return isNumber(v)
	case "integer":
		switch n := v.(type) {
		case float64:
			return n == float64(int64(n))
		case int:
			return true
		}
		return false
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	}
	return true
}

func isNumber(v any) bool {
	switch v.(type) {
	case float64, int:
		return true
	}
	return false
}
