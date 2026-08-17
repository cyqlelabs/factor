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
	return "Store an important memory, belief, or goal in long-term memory. Memories fade from recall over time unless they come up again — set retention=permanent for facts that must not: the user's identity, family and relationships, where they live, standing preferences. Resolve pronouns to explicit names, keep it atomic, and use negative valence (-0.5 to -1.0) for errors and things to avoid. Use type=belief with evidence to assert a probabilistic fact."
}
func (t *rememberTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content": map[string]any{"type": "string", "description": "The memory to store"},
			"type":    map[string]any{"type": "string", "enum": []any{"episode", "belief", "goal"}, "description": "episode (default): something that happened. belief: a probabilistic claim — pair it with evidence. goal: an intention to carry forward."},
			"retention": map[string]any{
				"type":        "string",
				"enum":        []any{"normal", "permanent"},
				"description": "permanent: identity facts, relationships, and standing preferences — stored with the engine's strongest durability so they outlive ordinary fading. normal (default): fades from recall unless restated.",
			},
			"valence":  map[string]any{"type": "number", "description": "Emotional tone -1..1; omit to auto-estimate"},
			"evidence": map[string]any{"type": "string", "description": "Why you believe this (normal-retention beliefs only)"},
			"source": map[string]any{
				"type":        "string",
				"enum":        []any{"user", "agent"},
				"description": "Who authored this. Use \"agent\" for facts about yourself — mistakes you made, actions you took. Omit for anything the user told you.",
			},
		},
		"required": []any{"content"},
	}
}
func (t *rememberTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	req := RememberRequest{
		Content:  tools.StringArg(args, "content"),
		Type:     tools.StringArg(args, "type"),
		Evidence: tools.StringArg(args, "evidence"),
		Source:   tools.StringArg(args, "source"),
	}
	if req.Source != SourceUser && req.Source != SourceAgent {
		req.Source = "" // unrecognised value: store with default user standing
	}
	if v, ok := args["valence"].(float64); ok {
		req.Valence = &v
	}
	// Unlike source — where coercing an unrecognised value falls back to the
	// stronger user standing — silently defaulting a bad retention value
	// would quietly hand a permanent fact a fading one, so it errors and
	// lets the model retry.
	retention := tools.StringArg(args, "retention")
	if retention != "" && retention != "normal" && retention != "permanent" {
		return tools.Errorf("unknown retention %q: use \"normal\" or \"permanent\"", retention)
	}
	// "permanent" translates entirely client-side into the strongest write
	// every smrti version already accepts: a plain remember with type=belief
	// and source=user. That path is born at the engine's highest confidence
	// and user standing exempts it from pruning. /believe would be weaker,
	// not stronger — its atoms start lower and duplicate on restatement — so
	// evidence is dropped to keep permanent facts off that route.
	if retention == "permanent" {
		req.Type = "belief"
		req.Probability = 0.95
		req.Evidence = ""
		if req.Source == "" {
			req.Source = SourceUser
		}
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
			"query": map[string]any{"type": "string", "description": "What you are looking for, in plain language; matching is by meaning, not keywords"},
			"top_k": map[string]any{"type": "integer", "description": "Max results (default 10)"},
		},
		"required": []any{"query"},
	}
}
func (t *recallTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	// No confidence floor: an explicit search must reach everything still
	// stored. A floor here reports "no memories found" for decayed facts the
	// graph is still holding, which reads as data loss rather than fading.
	mems, err := t.engine.Recall(ctx, tools.StringArg(args, "query"), tools.IntArg(args, "top_k", 10), 0, Scope{})
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
			"query":  map[string]any{"type": "string", "description": "Describes the memories to soften; everything matching by meaning is affected, so be specific"},
			"reason": map[string]any{"type": "string", "description": "Why this is being forgotten (e.g. 'user corrected me: they moved to Berlin')"},
		},
		"required": []any{"query"},
	}
}
func (t *forgetTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	if err := t.engine.Forget(ctx, tools.StringArg(args, "query"), tools.StringArg(args, "reason"), ""); err != nil {
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
