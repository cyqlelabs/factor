package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/tools"
)

// suiteFor builds the job tool suite over e, keyed by tool name.
func suiteFor(t *testing.T, e *Engine) map[string]tools.Tool {
	t.Helper()
	byName := map[string]tools.Tool{}
	for _, tool := range NewTools(e) {
		byName[tool.Name()] = tool
	}
	return byName
}

func TestNewToolsExposesTheFourJobTools(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	suite := NewTools(e)
	if len(suite) != 4 {
		t.Fatalf("suite size = %d", len(suite))
	}
	byName := suiteFor(t, e)
	for _, name := range []string{"job_start", "job_list", "job_status", "job_cancel"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("missing tool %q", name)
		}
	}
}

func TestJobToolsDeclareUsableSchemas(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	for _, tool := range NewTools(e) {
		if strings.TrimSpace(tool.Description()) == "" {
			t.Errorf("%s has no description", tool.Name())
		}
		params := tool.Parameters()
		if params["type"] != "object" {
			t.Errorf("%s schema type = %v, want object", tool.Name(), params["type"])
		}
		props, ok := params["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no properties object", tool.Name())
		}
		for name, prop := range props {
			p, ok := prop.(map[string]any)
			if !ok || p["type"] == "" || p["type"] == nil {
				t.Errorf("%s property %q has no type: %v", tool.Name(), name, prop)
			}
		}
		if req, present := params["required"]; present {
			list, ok := req.([]any)
			if !ok {
				t.Fatalf("%s required = %T, want []any", tool.Name(), req)
			}
			for _, entry := range list {
				name, ok := entry.(string)
				if !ok {
					t.Fatalf("%s required entry %v is not a string", tool.Name(), entry)
				}
				if _, declared := props[name]; !declared {
					t.Errorf("%s requires undeclared property %q", tool.Name(), name)
				}
			}
		}
		if _, err := json.Marshal(params); err != nil {
			t.Errorf("%s schema is not JSON-encodable: %v", tool.Name(), err)
		}
	}
}

func TestJobStartRecordsToolContextAsOrigin(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	defer e.Wait()
	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{
		Channel: "telegram", ChatID: "42", SessionKey: "telegram:42",
	})

	res := suiteFor(t, e)["job_start"].Execute(ctx, map[string]any{
		"kind": "exec", "description": "greet", "payload": "true",
	})
	if res.IsError {
		t.Fatalf("job_start: %s", res.ForLLM)
	}

	list := e.List()
	if len(list) != 1 {
		t.Fatalf("jobs = %d", len(list))
	}
	v := list[0].Snapshot()
	want := Origin{Channel: "telegram", ChatID: "42", SessionKey: "telegram:42"}
	if v.Origin != want {
		t.Errorf("origin = %+v, want %+v", v.Origin, want)
	}
	if v.Kind != KindExec || v.Description != "greet" {
		t.Errorf("job = %+v", v)
	}
	if !strings.Contains(res.ForLLM, v.ID) || !strings.Contains(res.ForLLM, "greet") {
		t.Errorf("result does not name the job: %q", res.ForLLM)
	}
}

func TestJobStartWithoutToolContextLeavesOriginEmpty(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	defer e.Wait()

	res := suiteFor(t, e)["job_start"].Execute(context.Background(), map[string]any{
		"kind": "exec", "description": "greet", "payload": "true",
	})
	if res.IsError {
		t.Fatalf("job_start: %s", res.ForLLM)
	}
	if got := e.List()[0].Snapshot().Origin; got != (Origin{}) {
		t.Errorf("origin = %+v, want zero", got)
	}
}

func TestJobStartDefaultsDescriptionToFirstLineOfPayload(t *testing.T) {
	long := "true # " + strings.Repeat("x", 80)
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"single line is used verbatim", "true", "true"},
		{"only the first line is used", "true\ntrue", "true"},
		{"over 60 characters is truncated", long, long[:60] + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine(context.Background(), t.TempDir(), nil, nil)
			defer e.Wait()
			res := suiteFor(t, e)["job_start"].Execute(context.Background(), map[string]any{
				"kind": "exec", "payload": tc.payload,
			})
			if res.IsError {
				t.Fatalf("job_start: %s", res.ForLLM)
			}
			if got := e.List()[0].Snapshot().Description; got != tc.want {
				t.Errorf("description = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"short line is unchanged", "run the thing", "run the thing"},
		{"text after a newline is dropped", "first\nsecond\nthird", "first"},
		{"a leading newline yields an empty label", "\nsecond", ""},
		{"exactly 60 characters is not truncated", strings.Repeat("a", 60), strings.Repeat("a", 60)},
		{"61 characters is truncated with an ellipsis", strings.Repeat("a", 61), strings.Repeat("a", 60) + "…"},
		{"a long first line is truncated before the newline", strings.Repeat("a", 70) + "\nsecond", strings.Repeat("a", 60) + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstLine(tc.in, 60); got != tc.want {
				t.Errorf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestJobStartRejectsEmptyPayload(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	res := suiteFor(t, e)["job_start"].Execute(context.Background(), map[string]any{
		"kind": "exec", "payload": "   ",
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "empty exec payload") {
		t.Fatalf("result = %+v", res)
	}
	if got := len(e.List()); got != 0 {
		t.Errorf("jobs = %d, want none", got)
	}
}

func TestJobStartRejectsTaskKindWithoutARunner(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	res := suiteFor(t, e)["job_start"].Execute(context.Background(), map[string]any{
		"kind": "task", "payload": "research something",
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "task jobs are not available") {
		t.Fatalf("result = %+v", res)
	}
	if got := len(e.List()); got != 0 {
		t.Errorf("jobs = %d, want none", got)
	}
}

// An unrecognised kind is rejected up front, so the model gets an actionable
// error instead of being told a doomed job started.
func TestJobStartWithUnknownKindIsRejected(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	res := suiteFor(t, e)["job_start"].Execute(context.Background(), map[string]any{
		"kind": "bogus", "payload": "true",
	})
	if !res.IsError {
		t.Fatalf("job_start accepted an unknown kind: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, `unknown job kind "bogus"`) {
		t.Errorf("error = %q", res.ForLLM)
	}
	if got := len(e.List()); got != 0 {
		t.Errorf("a rejected job was still recorded: %d", got)
	}
}

func TestJobListReportsNothingUntilAJobExists(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	byName := suiteFor(t, e)

	if res := byName["job_list"].Execute(context.Background(), nil); res.ForLLM != "No jobs." {
		t.Fatalf("empty list = %q", res.ForLLM)
	}

	res := byName["job_start"].Execute(context.Background(), map[string]any{
		"kind": "exec", "description": "greet", "payload": "true",
	})
	if res.IsError {
		t.Fatalf("job_start: %s", res.ForLLM)
	}
	id := e.List()[0].ID
	waitFinished(t, e, id)

	res = byName["job_list"].Execute(context.Background(), nil)
	for _, want := range []string{id, "[done]", "exec", "greet", "ago"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("listing missing %q:\n%s", want, res.ForLLM)
		}
	}
}

func TestJobStatusRejectsUnknownID(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	res := suiteFor(t, e)["job_status"].Execute(context.Background(), map[string]any{"id": "j99"})
	if !res.IsError || !strings.Contains(res.ForLLM, `no job "j99"`) {
		t.Fatalf("result = %+v", res)
	}
}

func TestJobStatusShowsStateAndOutputTail(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	byName := suiteFor(t, e)
	if res := byName["job_start"].Execute(context.Background(), map[string]any{
		"kind": "exec", "description": "greet", "payload": "echo hello from the job",
	}); res.IsError {
		t.Fatalf("job_start: %s", res.ForLLM)
	}
	id := e.List()[0].ID
	waitFinished(t, e, id)

	res := byName["job_status"].Execute(context.Background(), map[string]any{"id": id})
	if res.IsError {
		t.Fatalf("job_status: %s", res.ForLLM)
	}
	for _, want := range []string{id, "[done]", "greet", "--- output tail ---", "hello from the job"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("status missing %q:\n%s", want, res.ForLLM)
		}
	}
}

func TestJobStatusSaysSoWhenAJobPrintedNothing(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	byName := suiteFor(t, e)
	if res := byName["job_start"].Execute(context.Background(), map[string]any{
		"kind": "exec", "payload": "true",
	}); res.IsError {
		t.Fatalf("job_start: %s", res.ForLLM)
	}
	id := e.List()[0].ID
	waitFinished(t, e, id)

	res := byName["job_status"].Execute(context.Background(), map[string]any{"id": id})
	if !strings.Contains(res.ForLLM, "(no output yet)") {
		t.Errorf("status = %q", res.ForLLM)
	}
}

func TestJobCancelRejectsUnknownID(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	res := suiteFor(t, e)["job_cancel"].Execute(context.Background(), map[string]any{"id": "j99"})
	if !res.IsError || !strings.Contains(res.ForLLM, "no job j99") {
		t.Fatalf("result = %+v", res)
	}
}

func TestJobCancelRejectsAFinishedJob(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	byName := suiteFor(t, e)
	if res := byName["job_start"].Execute(context.Background(), map[string]any{
		"kind": "exec", "payload": "true",
	}); res.IsError {
		t.Fatalf("job_start: %s", res.ForLLM)
	}
	id := e.List()[0].ID
	waitFinished(t, e, id)

	res := byName["job_cancel"].Execute(context.Background(), map[string]any{"id": id})
	if !res.IsError || !strings.Contains(res.ForLLM, "already done") {
		t.Fatalf("result = %+v", res)
	}
}

func TestJobCancelStopsARunningJob(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	byName := suiteFor(t, e)
	if res := byName["job_start"].Execute(context.Background(), map[string]any{
		"kind": "exec", "payload": "sleep 30",
	}); res.IsError {
		t.Fatalf("job_start: %s", res.ForLLM)
	}
	job := e.List()[0]
	id := job.ID
	if got := job.Snapshot().State; got != StateRunning {
		t.Fatalf("state = %s, want %s", got, StateRunning)
	}

	res := byName["job_cancel"].Execute(context.Background(), map[string]any{"id": id})
	if res.IsError || res.ForLLM != "Cancelled." {
		t.Fatalf("result = %+v", res)
	}
	e.Wait()
	if got := waitFinished(t, e, id).State; got != StateCancelled {
		t.Errorf("state = %s, want %s", got, StateCancelled)
	}
}
