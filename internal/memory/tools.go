package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cyqlelabs/factor/internal/tools"
)

// NewTools returns the deliberate-memory tool set on top of the ambient loop.
func NewTools(engine Engine) []tools.Tool {
	return []tools.Tool{
		&rememberTool{engine},
		&recallTool{engine},
		&forgetTool{engine},
		&reflectTool{engine},
		&statusTool{engine},
	}
}

type rememberTool struct{ engine Engine }

func (t *rememberTool) Name() string { return "remember" }
func (t *rememberTool) Description() string {
	return "Store an important memory, belief, or goal in long-term memory. Resolve pronouns to explicit names, keep it atomic, and use negative valence (-0.5 to -1.0) for errors and things to avoid. Use type=belief with evidence to assert a probabilistic fact."
}
func (t *rememberTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content":     map[string]any{"type": "string", "description": "The memory to store"},
			"type":        map[string]any{"type": "string", "enum": []any{"episode", "belief", "goal"}},
			"probability": map[string]any{"type": "number", "description": "How true this is (0-1, default 0.8)"},
			"valence":     map[string]any{"type": "number", "description": "Emotional tone -1..1; omit to auto-estimate"},
			"evidence":    map[string]any{"type": "string", "description": "Why you believe this (beliefs only)"},
		},
		"required": []any{"content"},
	}
}
func (t *rememberTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	req := RememberRequest{
		Content:     tools.StringArg(args, "content"),
		Type:        tools.StringArg(args, "type"),
		Probability: tools.FloatArg(args, "probability", 0.8),
		Evidence:    tools.StringArg(args, "evidence"),
	}
	if v, ok := args["valence"].(float64); ok {
		req.Valence = &v
	}
	id, err := t.engine.Remember(ctx, req)
	if err != nil {
		return tools.Errorf("remember failed: %v", err)
	}
	if id == "" {
		return tools.Text("Stored (filtered or deduplicated by the memory engine).")
	}
	return tools.Textf("Stored memory %s", id)
}

type recallTool struct{ engine Engine }

func (t *recallTool) Name() string { return "recall" }
func (t *recallTool) Description() string {
	return "Search long-term memory by meaning. Results carry severity: critical_warning = past mistake, do not repeat; known_antipattern = disproven belief; context = background."
}
func (t *recallTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
			"top_k": map[string]any{"type": "integer", "description": "Max results (default 10)"},
		},
		"required": []any{"query"},
	}
}
func (t *recallTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	mems, err := t.engine.Recall(ctx, tools.StringArg(args, "query"), tools.IntArg(args, "top_k", 10), 0.1)
	if err != nil {
		return tools.Errorf("recall failed: %v", err)
	}
	if len(mems) == 0 {
		return tools.Text("No memories found.")
	}
	var b strings.Builder
	for _, m := range mems {
		fmt.Fprintf(&b, "[%s | %s | salience %.2f | confidence %.2f | valence %+.2f] %s\n",
			m.Type, m.Severity, m.Salience, m.Confidence, m.Valence, m.Content)
	}
	return tools.Text(b.String())
}

type forgetTool struct{ engine Engine }

func (t *forgetTool) Name() string { return "forget" }
func (t *forgetTool) Description() string {
	return "Lower confidence on memories matching a query. Not a hard delete; consolidation prunes them over time."
}
func (t *forgetTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":  map[string]any{"type": "string"},
			"reason": map[string]any{"type": "string"},
		},
		"required": []any{"query"},
	}
}
func (t *forgetTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	if err := t.engine.Forget(ctx, tools.StringArg(args, "query"), tools.StringArg(args, "reason")); err != nil {
		return tools.Errorf("forget failed: %v", err)
	}
	return tools.Text("Softened matching memories.")
}

type reflectTool struct{ engine Engine }

func (t *reflectTool) Name() string { return "reflect" }
func (t *reflectTool) Description() string {
	return "Trigger a memory consolidation epoch: merge evidence, decay attention, resolve contradictions, prune."
}
func (t *reflectTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *reflectTool) Execute(ctx context.Context, _ map[string]any) *tools.Result {
	report, err := t.engine.Reflect(ctx)
	if err != nil {
		return tools.Errorf("reflect failed: %v", err)
	}
	return tools.Text(compactJSON(report))
}

type statusTool struct{ engine Engine }

func (t *statusTool) Name() string { return "memory_status" }
func (t *statusTool) Description() string {
	return "Memory engine statistics: atom counts, emotional state, spaces."
}
func (t *statusTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *statusTool) Execute(ctx context.Context, _ map[string]any) *tools.Result {
	if !t.engine.Healthy() {
		return tools.Errorf("memory engine is not reachable right now")
	}
	status, err := t.engine.Status(ctx)
	if err != nil {
		return tools.Errorf("status failed: %v", err)
	}
	return tools.Text(compactJSON(status))
}

func compactJSON(m map[string]any) string {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", m)
	}
	return string(data)
}
