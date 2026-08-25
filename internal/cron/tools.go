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
	return "Schedule work for later: one-off reminders and recurring tasks. Actions: add (message, plus either at or schedule), list, remove (id), enable (id), disable (id). Use `at` for anything that happens once — \"remind me at four\", \"tell me on the 5th\" — as a local wall-clock time like \"2026-09-05 10:00\"; it runs then and deletes itself. Use `schedule` only for something genuinely repeating, as a 5-field cron expression: it recurs forever, daily for a time-of-day expression and yearly for a date-and-month one. add and list report when each job actually runs next, so check that against what the user asked for. Results are delivered to the chat the job was created from."
}
func (t *Tool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":   map[string]any{"type": "string", "enum": []any{"add", "list", "remove", "enable", "disable"}, "description": "add needs schedule+message; remove/enable/disable need id; list needs neither"},
			"at":       map[string]any{"type": "string", "description": "One-off: the local time to run at, e.g. '2026-09-05 10:00' or '2026-09-05T10:00:00-03:00'. Runs once, then deletes itself. Must be in the future."},
			"schedule": map[string]any{"type": "string", "description": "Recurring only: cron expression, e.g. '0 9 * * *' for 09:00 every day. Never use this for a one-off — prefer at."},
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
		return t.add(args, tc)
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
			fmt.Fprintf(&b, "%s [%s] %s — %s — %q → %s:%s\n",
				j.ID, state, cadence(j), t.nextRunText(j), j.Message, j.Channel, j.ChatID)
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

// add takes either a moment or a cron expression, and refuses both at once:
// a caller that gave both does not know which it meant, and guessing is how a
// one-off reminder quietly becomes a yearly one.
func (t *Tool) add(args map[string]any, tc tools.ToolContext) *tools.Result {
	at, schedule := tools.StringArg(args, "at"), tools.StringArg(args, "schedule")
	message := tools.StringArg(args, "message")
	switch {
	case at != "" && schedule != "":
		return tools.Errorf("give either at (runs once) or schedule (recurring), not both")
	case at != "":
		moment, err := parseAt(at, t.Service.now())
		if err != nil {
			return tools.Errorf("%v", err)
		}
		job, err := t.Service.AddOnce(moment, message, tc.Channel, tc.ChatID)
		if err != nil {
			return tools.Errorf("%v", err)
		}
		return tools.Textf("Scheduled %s: %q — runs once at %s, %s, then deletes itself.",
			job.ID, job.Message, job.At.Format(stamp), untilText(time.Until(job.At)))
	case schedule != "":
		job, err := t.Service.Add(schedule, message, tc.Channel, tc.ChatID)
		if err != nil {
			return tools.Errorf("%v", err)
		}
		return tools.Textf("Scheduled %s: %q (%s) — %s, and again every time the schedule comes round.",
			job.ID, job.Message, job.Schedule, t.nextRunText(job))
	}
	return tools.Errorf("add needs at (a one-off, e.g. '2026-09-05 10:00') or schedule (recurring, e.g. '0 9 * * *')")
}

// atLayouts are what a model writes when asked for a moment. Everything
// without a zone is read as local, which is the clock the turn context told it
// the time on.
var atLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
}

// parseAt reads a moment in the machine's own timezone. A bare time of day is
// taken as the next one to come round, so "16:00" said in the morning means
// this afternoon and said at midnight means tomorrow.
func parseAt(text string, now time.Time) (time.Time, error) {
	text = strings.TrimSpace(text)
	for _, layout := range atLayouts {
		if at, err := time.ParseInLocation(layout, text, now.Location()); err == nil {
			return at, nil
		}
	}
	for _, layout := range []string{"15:04:05", "15:04"} {
		clock, err := time.ParseInLocation(layout, text, now.Location())
		if err != nil {
			continue
		}
		at := time.Date(now.Year(), now.Month(), now.Day(),
			clock.Hour(), clock.Minute(), clock.Second(), 0, now.Location())
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		return at, nil
	}
	return time.Time{}, fmt.Errorf("cannot read %q as a time; use a local wall clock like \"2006-01-02 15:04\"", text)
}

// cadence names how often a job runs, for a listing where one-shots and cron
// expressions sit side by side.
func cadence(job Job) string {
	if job.Once() {
		return "once"
	}
	return job.Schedule
}

// nextRunText says when a job fires next, in the units a person would use. A
// reminder the model meant for this afternoon but wrote as a date that has
// already passed reads as wrong at a glance here, and nowhere else.
func (t *Tool) nextRunText(job Job) string {
	next, ok := t.Service.nextRun(job)
	if !ok {
		return "never runs again"
	}
	return fmt.Sprintf("next run %s, %s", next.Format(stamp), untilText(time.Until(next)))
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
