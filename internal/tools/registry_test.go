package tools

import (
	"context"
	"strings"
	"testing"
)

// floodTool returns more than any turn should have to carry.
type floodTool struct{ size int }

func (f *floodTool) Name() string               { return "flood" }
func (f *floodTool) Description() string        { return "returns too much" }
func (f *floodTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (f *floodTool) Execute(context.Context, map[string]any) *Result {
	return Text(strings.Repeat("A", f.size))
}

// One tool result must not be able to take a large share of the context. What
// it costs is charged on every turn for the rest of the session, not only on
// the turn that asked for it.
func TestAnOversizedToolResultIsCappedAndSaysSo(t *testing.T) {
	r := NewRegistry(nil, nil)
	r.Register(&floodTool{size: 100000})

	got := r.Execute(context.Background(), "flood", nil).ForLLM
	if len(got) >= 100000 {
		t.Fatalf("result is %d chars, want it capped", len(got))
	}
	// A silent cut reads to the model as the whole answer — the page really
	// did end there. The note is what turns a truncation into a next step.
	for _, want := range []string{"of 100000 characters shown", "Narrow the call"} {
		if !strings.Contains(got, want) {
			t.Errorf("the cap did not say what it withheld (%q missing)", want)
		}
	}
	if !strings.HasPrefix(got, strings.Repeat("A", 64)) {
		t.Error("the cap dropped the head of the result instead of the tail")
	}
}

func TestAResultUnderTheCapIsUntouched(t *testing.T) {
	r := NewRegistry(nil, nil)
	r.Register(&floodTool{size: 100})
	if got := r.Execute(context.Background(), "flood", nil).ForLLM; got != strings.Repeat("A", 100) {
		t.Errorf("an ordinary result was rewritten: %q", got)
	}
}
