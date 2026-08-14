package jobs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

// NewTools returns the job-management tool set.
func NewTools(engine *Engine) []tools.Tool {
	return []tools.Tool{
		&startTool{engine},
		&listTool{engine},
		&statusTool{engine},
		&cancelTool{engine},
	}
}

type startTool struct{ engine *Engine }

func (t *startTool) Name() string { return "job_start" }
func (t *startTool) Description() string {
	return "Run long work in the background and reply to the user immediately. kind=exec runs a shell command; kind=task delegates a sub-task to yourself (a separate agent run). You are notified automatically when the job finishes — tell the user you started it and stop waiting."
}
func (t *startTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind":        map[string]any{"type": "string", "enum": []any{"exec", "task"}, "description": "exec runs payload as a shell command; task hands payload to a fresh agent run that can use tools and reason"},
			"description": map[string]any{"type": "string", "description": "Short human-readable label"},
			"payload":     map[string]any{"type": "string", "description": "Shell command (exec) or task prompt (task)"},
		},
		"required": []any{"kind", "payload"},
	}
}
func (t *startTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	tc := tools.ToolContextFrom(ctx)
	desc := tools.StringArg(args, "description")
	if desc == "" {
		desc = firstLine(tools.StringArg(args, "payload"), 60)
	}
	job, err := t.engine.Start(
		Kind(tools.StringArg(args, "kind")),
		desc,
		tools.StringArg(args, "payload"),
		Origin{Channel: tc.Channel, ChatID: tc.ChatID, SessionKey: tc.SessionKey},
	)
	if err != nil {
		return tools.Errorf("job start failed: %v", err)
	}
	return tools.Textf("Started background job %s (%s). You will be notified when it finishes; reply to the user now.", job.ID, desc)
}

type listTool struct{ engine *Engine }

func (t *listTool) Name() string        { return "job_list" }
func (t *listTool) Description() string { return "List background jobs and their states." }
func (t *listTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *listTool) Execute(_ context.Context, _ map[string]any) *tools.Result {
	list := t.engine.List()
	if len(list) == 0 {
		return tools.Text("No jobs.")
	}
	var b strings.Builder
	for _, j := range list {
		v := j.Snapshot()
		age := time.Since(v.Started).Round(time.Second)
		fmt.Fprintf(&b, "%s [%s] %s — %s (started %s ago)\n", v.ID, v.State, v.Kind, v.Description, age)
	}
	return tools.Text(b.String())
}

type statusTool struct{ engine *Engine }

func (t *statusTool) Name() string { return "job_status" }
func (t *statusTool) Description() string {
	return "Get one job's state and its recent output (tail)."
}
func (t *statusTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"id": map[string]any{"type": "string", "description": "Job id returned by job_start or job_list"}},
		"required":   []any{"id"},
	}
}
func (t *statusTool) Execute(_ context.Context, args map[string]any) *tools.Result {
	job, ok := t.engine.Get(tools.StringArg(args, "id"))
	if !ok {
		return tools.Errorf("no job %q", tools.StringArg(args, "id"))
	}
	v := job.Snapshot()
	tail := job.OutputTail()
	if tail == "" {
		tail = "(no output yet)"
	}
	return tools.Textf("%s [%s] %s\n--- output tail ---\n%s", v.ID, v.State, v.Description, tail)
}

type cancelTool struct{ engine *Engine }

func (t *cancelTool) Name() string        { return "job_cancel" }
func (t *cancelTool) Description() string { return "Cancel a running background job." }
func (t *cancelTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"id": map[string]any{"type": "string", "description": "Job id returned by job_start or job_list"}},
		"required":   []any{"id"},
	}
}
func (t *cancelTool) Execute(_ context.Context, args map[string]any) *tools.Result {
	if err := t.engine.Cancel(tools.StringArg(args, "id")); err != nil {
		return tools.Errorf("%v", err)
	}
	return tools.Text("Cancelled.")
}

func firstLine(s string, max int) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	// Cut on runes, not bytes: a byte-slice through a multi-byte character
	// would put invalid UTF-8 in front of the model and the user.
	if runes := []rune(s); len(runes) > max {
		s = string(runes[:max]) + "…"
	}
	return s
}
