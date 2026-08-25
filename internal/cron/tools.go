package cron

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

// Tool lets the agent manage its own schedule. The channel/chat a job is
// created from becomes its delivery target.
type Tool struct{ Service *Service }

func (t *Tool) Name() string { return "cron" }
func (t *Tool) Description() string {
	return "Manage scheduled tasks and reminders (5-field cron expressions, minute resolution). Actions: add (schedule+message), list, remove (id), enable (id), disable (id). A cron expression recurs: a date-and-month schedule repeats every year, and a time-of-day schedule repeats every day, so remove a one-off reminder once it has been delivered. add and list report when each job next runs — check that against what the user asked for, because a time that has already gone by today is accepted and simply waits until the schedule next comes round. Results are delivered to the chat the job was created from."
}
func (t *Tool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":   map[string]any{"type": "string", "enum": []any{"add", "list", "remove", "enable", "disable"}, "description": "add needs schedule+message; remove/enable/disable need id; list needs neither"},
			"schedule": map[string]any{"type": "string", "description": "Cron expression, e.g. '0 9 * * *' for 09:00 daily"},
			"message":  map[string]any{"type": "string", "description": "Prompt to run when due"},
			"id":       map[string]any{"type": "string", "description": "Job id from a previous list or add; required by remove, enable, and disable"},
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
		return tools.Textf("Scheduled %s: %q (%s) — %s, and again every time the schedule comes round.",
			job.ID, job.Message, job.Schedule, t.nextRunText(job))
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
			fmt.Fprintf(&b, "%s [%s] %s (%s) — %q → %s:%s\n",
				j.ID, state, j.Schedule, t.nextRunText(j), j.Message, j.Channel, j.ChatID)
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

// nextRunText says when a job fires next, in the units a person would use. A
// reminder the model meant for this afternoon but wrote as a date that has
// already passed reads as wrong at a glance here, and nowhere else.
func (t *Tool) nextRunText(job Job) string {
	next, ok := t.Service.nextRun(job)
	if !ok {
		return "never runs again"
	}
	return fmt.Sprintf("next run %s, %s", next.Format("Mon 2006-01-02 15:04 MST"), untilText(time.Until(next)))
}

func untilText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "in under a minute"
	case d < time.Hour:
		return fmt.Sprintf("in %d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("in %dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("in %d days", int(d.Hours()/24))
	}
}
