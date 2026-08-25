package cron

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

func toolCtx() context.Context {
	return tools.WithToolContext(context.Background(), tools.ToolContext{Channel: "telegram", ChatID: "77"})
}

func TestCronToolDescriptor(t *testing.T) {
	tool := &Tool{}
	if tool.Name() != "cron" {
		t.Errorf("Name() = %q, want cron", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description() is empty; the model has nothing to select on")
	}

	params := tool.Parameters()
	if params["type"] != "object" {
		t.Errorf("Parameters()[type] = %v, want object", params["type"])
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Parameters()[properties] = %T, want map[string]any", params["properties"])
	}
	for _, key := range []string{"action", "schedule", "message", "id"} {
		if _, ok := props[key]; !ok {
			t.Errorf("Parameters() omits property %q", key)
		}
	}
	required, ok := params["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "action" {
		t.Errorf("Parameters()[required] = %v, want [action]", params["required"])
	}
}

func TestCronToolReportsEmptyList(t *testing.T) {
	tool := &Tool{Service: newService(t, nil, nil)}
	res := tool.Execute(toolCtx(), map[string]any{"action": "list"})
	if res.IsError || !strings.Contains(res.ForLLM, "No scheduled tasks.") {
		t.Errorf("list with no jobs = %+v", res)
	}
}

func TestCronToolRejectsInvalidAdd(t *testing.T) {
	tool := &Tool{Service: newService(t, nil, nil)}
	res := tool.Execute(toolCtx(), map[string]any{"action": "add", "schedule": "every so often", "message": "x"})
	if !res.IsError {
		t.Errorf("invalid schedule accepted: %+v", res)
	}
	res = tool.Execute(toolCtx(), map[string]any{"action": "add", "schedule": "0 9 * * *"})
	if !res.IsError {
		t.Errorf("add without a message accepted: %+v", res)
	}
}

func TestCronToolRejectsMissingAndUnknownIDs(t *testing.T) {
	tool := &Tool{Service: newService(t, nil, nil)}
	for _, action := range []string{"remove", "enable", "disable"} {
		if res := tool.Execute(toolCtx(), map[string]any{"action": action}); !res.IsError {
			t.Errorf("%s without an id succeeded: %+v", action, res)
		}
		res := tool.Execute(toolCtx(), map[string]any{"action": action, "id": "cron-404"})
		if !res.IsError {
			t.Errorf("%s of an unknown id succeeded: %+v", action, res)
		}
	}
}

func TestCronToolRejectsUnknownAction(t *testing.T) {
	tool := &Tool{Service: newService(t, nil, nil)}
	res := tool.Execute(toolCtx(), map[string]any{"action": "frobnicate"})
	if !res.IsError || !strings.Contains(res.ForLLM, "unknown action") {
		t.Errorf("unknown action = %+v", res)
	}
	if res := tool.Execute(toolCtx(), map[string]any{}); !res.IsError {
		t.Errorf("missing action = %+v", res)
	}
}

func TestCronToolEnableRoundTrip(t *testing.T) {
	s := newService(t, nil, nil)
	tool := &Tool{Service: s}
	ctx := toolCtx()

	if res := tool.Execute(ctx, map[string]any{"action": "add", "schedule": "0 8 * * 1", "message": "weekly report"}); res.IsError {
		t.Fatalf("add: %+v", res)
	}
	id := s.List()[0].ID

	if res := tool.Execute(ctx, map[string]any{"action": "disable", "id": id}); res.IsError {
		t.Fatalf("disable: %+v", res)
	}
	if res := tool.Execute(ctx, map[string]any{"action": "list"}); !strings.Contains(res.ForLLM, "[disabled]") {
		t.Errorf("listing does not mark the job disabled: %+v", res)
	}

	res := tool.Execute(ctx, map[string]any{"action": "enable", "id": id})
	if res.IsError || !strings.Contains(res.ForLLM, "Enabled "+id) {
		t.Fatalf("enable = %+v", res)
	}
	if !s.List()[0].Enabled {
		t.Error("enable did not re-enable the job")
	}
	if res := tool.Execute(ctx, map[string]any{"action": "list"}); !strings.Contains(res.ForLLM, "[enabled]") {
		t.Errorf("listing does not mark the job enabled: %+v", res)
	}
}

func TestUntilTextSpeaksInUnitsAPersonUses(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{20 * time.Second, "in under a minute"},
		{7 * time.Minute, "in 7 minutes"},
		{90 * time.Minute, "in 1h30m"},
		{47 * time.Hour, "in 47h00m"},
		{50 * time.Hour, "in 2 days"},
		{364 * 24 * time.Hour, "in 364 days"},
	} {
		if got := untilText(tc.in); got != tc.want {
			t.Errorf("untilText(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A schedule nothing can parse can only come from a hand-edited or older
// jobs.json. It will never fire, and the listing has to say so rather than
// show a job that looks live.
func TestListSaysSoWhenAJobWillNeverRunAgain(t *testing.T) {
	s := newService(t, nil, nil)
	s.jobs["cron-bad"] = &Job{ID: "cron-bad", Schedule: "not a schedule", Message: "orphan", Enabled: true}
	res := (&Tool{Service: s}).Execute(toolCtx(), map[string]any{"action": "list"})
	if !strings.Contains(res.ForLLM, "never runs again") {
		t.Errorf("list = %s", res.ForLLM)
	}
}
