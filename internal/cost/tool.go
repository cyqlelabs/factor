package cost

import (
	"context"
	"fmt"
	"strings"

	"github.com/cyqlelabs/factor/internal/tools"
)

// Tool lets the agent answer "what has this cost?" — for the conversation it
// is in, for today, for all time — and say how much room is left under a cap
// before it runs into one.
type Tool struct{ Meter *Meter }

// NewTool builds the usage tool, or nothing at all when the meter is
// inactive: a tool that can only ever answer "not counting" is prompt weight.
func NewTool(m *Meter) []tools.Tool {
	if !m.Active() {
		return nil
	}
	return []tools.Tool{&Tool{Meter: m}}
}

func (t *Tool) Name() string { return "usage" }

func (t *Tool) Description() string {
	return "Report token usage and what it cost: this session, today, this month, all time, broken down by model, plus how much room is left under any budget cap. Omit session to report the conversation you are in; pass sessions=true to also list the biggest spenders across every session."
}

func (t *Tool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session": map[string]any{
				"type":        "string",
				"description": "Session key to report on, e.g. telegram:12345 (default: the current conversation)",
			},
			"sessions": map[string]any{
				"type":        "boolean",
				"description": "Also list per-session totals, biggest first",
			},
		},
	}
}

func (t *Tool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	key := tools.StringArg(args, "session")
	if key == "" {
		key = tools.ToolContextFrom(ctx).SessionKey
	}
	out := t.Meter.Report(key)
	if tools.BoolArg(args, "sessions", false) && t.Meter.Active() {
		out += "\n" + sessionTable(t.Meter.Snapshot(key).Sessions)
	}
	return tools.Text(out)
}

// maxSessionRows bounds the per-session listing: the long tail of one-message
// conversations answers nothing.
const maxSessionRows = 15

func sessionTable(sessions map[string]Totals) string {
	if len(sessions) == 0 {
		return "No session has spent anything yet."
	}
	keys := bySpend(sessions)
	var b strings.Builder
	b.WriteString("By session:\n")
	for i, key := range keys {
		if i == maxSessionRows {
			fmt.Fprintf(&b, "  … and %d more\n", len(keys)-maxSessionRows)
			break
		}
		fmt.Fprintf(&b, "  %s: %s\n", key, bucketWords(sessions[key]))
	}
	return strings.TrimRight(b.String(), "\n")
}
