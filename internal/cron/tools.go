package cron

import (
	"context"
	"fmt"
	"strings"

	"github.com/cyqlelabs/factor/internal/tools"
)

// Tool lets the agent manage its own schedule. The channel/chat a job is
// created from becomes its delivery target.
type Tool struct{ Service *Service }

func (t *Tool) Name() string { return "cron" }
func (t *Tool) Description() string {
	return "Manage scheduled tasks (5-field cron expressions, minute resolution). Actions: add (schedule+message), list, remove (id), enable (id), disable (id). Results are delivered to the chat the job was created from."
}
func (t *Tool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":   map[string]any{"type": "string", "enum": []any{"add", "list", "remove", "enable", "disable"}},
			"schedule": map[string]any{"type": "string", "description": "Cron expression, e.g. '0 9 * * *' for 09:00 daily"},
			"message":  map[string]any{"type": "string", "description": "Prompt to run when due"},
			"id":       map[string]any{"type": "string"},
		},
		"required": []any{"action"},
	}
}

func (t *Tool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	tc := tools.ToolContextFrom(ctx)
	id := tools.StringArg(args, "id")
	switch tools.StringArg(args, "action") {
	case "add":
		job, err := t.Service.Add(
			tools.StringArg(args, "schedule"),
			tools.StringArg(args, "message"),
			tc.Channel, tc.ChatID,
		)
		if err != nil {
			return tools.Errorf("%v", err)
		}
		return tools.Textf("Scheduled %s: %q (%s)", job.ID, job.Message, job.Schedule)
	case "list":
		jobs := t.Service.List()
		if len(jobs) == 0 {
			return tools.Text("No scheduled tasks.")
		}
		var b strings.Builder
		for _, j := range jobs {
			state := "enabled"
			if !j.Enabled {
				state = "disabled"
			}
			fmt.Fprintf(&b, "%s [%s] %s — %q → %s:%s\n", j.ID, state, j.Schedule, j.Message, j.Channel, j.ChatID)
		}
		return tools.Text(b.String())
	case "remove":
		if err := t.Service.Remove(id); err != nil {
			return tools.Errorf("%v", err)
		}
		return tools.Textf("Removed %s", id)
	case "enable", "disable":
		enable := tools.StringArg(args, "action") == "enable"
		if err := t.Service.SetEnabled(id, enable); err != nil {
			return tools.Errorf("%v", err)
		}
		if enable {
			return tools.Textf("Enabled %s", id)
		}
		return tools.Textf("Disabled %s", id)
	}
	return tools.Errorf("unknown action %q", tools.StringArg(args, "action"))
}
