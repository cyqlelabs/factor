package tools

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

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
			result.ForLLM = capResult(name, r.filter(result.ForLLM))
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

// maxResultChars bounds one tool result at roughly four thousand tokens.
// That is generous for an answer and stingy for a document: a page, a log or
// a directory listing that runs past it was never going to be read whole, and
// what it costs is charged on every turn for the rest of the session, not
// just the one that asked for it.
const maxResultChars = 16000

// capResult truncates an oversized tool result and says so in the result
// itself. A silent cut reads to the model as the whole answer — the page
// really did end there, the search really did return nine hits — so the note
// is the part that matters: it names what was withheld and how to ask for
// less, which turns a truncation into a next step instead of a wrong fact.
func capResult(name, text string) string {
	if len(text) <= maxResultChars {
		return text
	}
	kept := text[:maxResultChars]
	// Ending mid-line invites the model to complete the sentence itself.
	if i := strings.LastIndexByte(kept, '\n'); i > maxResultChars/2 {
		kept = kept[:i]
	}
	// A byte cut can land inside a multi-byte character, and a page in any
	// language but English is full of them. Back off to the last whole rune
	// rather than hand the model a broken one — bounded, because output that
	// is not text at all must not turn this into a byte-at-a-time scan.
	for range utf8.UTFMax {
		if r, size := utf8.DecodeLastRuneInString(kept); r != utf8.RuneError || size > 1 {
			break
		}
		kept = kept[:len(kept)-1]
	}
	slog.Info("tool result truncated", "tool", name, "chars", len(text), "kept", len(kept))
	return fmt.Sprintf("%s\n\n[Truncated: %d of %d characters shown, and the rest is not in your context. "+
		"Narrow the call — a filter, a query, a smaller range, a specific path — to see the part you need.]",
		kept, len(kept), len(text))
}

// ValidateArgs checks required fields, primitive types, and string enums
// against a JSON schema fragment. It is intentionally minimal: enough to catch
// the common LLM mistakes (missing field, wrong type, invented enum value) with
// messages the model can act on without another round trip.
func ValidateArgs(schema, args map[string]any) error {
	if schema == nil {
		return nil
	}
	// Accept both JSON-decoded ([]any) and hand-written Go ([]string) schemas
	// so a new tool cannot silently lose required-argument checking.
	for _, name := range SchemaStrings(schema["required"]) {
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
		// An unenforced enum is worse than no enum: the bad value reaches the
		// tool's switch, falls through to a default, and the model never learns
		// why. Listing the allowed values lets it retry correctly.
		if allowed := SchemaStrings(spec["enum"]); len(allowed) > 0 {
			if got, ok := raw.(string); ok && !slices.Contains(allowed, got) {
				return fmt.Errorf("argument %q must be one of [%s], got %q",
					key, strings.Join(allowed, ", "), got)
			}
		}
	}
	return nil
}

// SchemaStrings reads a schema's string list (required, enum) from either a
// JSON-decoded []any or a hand-written Go []string, so a tool cannot silently
// lose validation by writing its schema in the other shape.
func SchemaStrings(raw any) []string {
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
