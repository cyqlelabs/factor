package cost

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

func usageTool(t *testing.T) (*Tool, *Meter) {
	t.Helper()
	m, _ := meterFor(t, config.CostConfig{Track: true}, answered("a/model", 1000, 500))
	registered := NewTool(m)
	if len(registered) != 1 {
		t.Fatalf("an active meter registered %d tools", len(registered))
	}
	return registered[0].(*Tool), m
}

func TestUsageToolReportsTheConversationItWasAskedFrom(t *testing.T) {
	tool, m := usageTool(t)
	if tool.Name() != "usage" || tool.Description() == "" {
		t.Errorf("tool identity = %q / %q", tool.Name(), tool.Description())
	}
	if _, ok := tool.Parameters()["properties"]; !ok {
		t.Errorf("parameters = %+v", tool.Parameters())
	}

	ctx := inSession("cli:main")
	if _, err := m.Chat(ctx, &provider.Request{}); err != nil {
		t.Fatal(err)
	}
	res := tool.Execute(ctx, map[string]any{})
	if res.IsError || !strings.Contains(res.ForLLM, "This session (cli:main)") {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(res.ForLLM, "$2.00") {
		t.Errorf("the session's spend is missing:\n%s", res.ForLLM)
	}
}

func TestUsageToolCanBeAimedAtAnotherSession(t *testing.T) {
	tool, m := usageTool(t)
	if _, err := m.Chat(inSession("telegram:7"), &provider.Request{}); err != nil {
		t.Fatal(err)
	}
	res := tool.Execute(inSession("cli:main"), map[string]any{"session": "telegram:7"})
	if !strings.Contains(res.ForLLM, "This session (telegram:7)") {
		t.Errorf("result = %s", res.ForLLM)
	}
	// Its own session spent nothing, and reports that rather than borrowing
	// another conversation's numbers.
	own := tool.Execute(inSession("cli:main"), map[string]any{})
	if strings.Contains(own.ForLLM, "telegram:7") {
		t.Errorf("result leaked another session: %s", own.ForLLM)
	}
	if !strings.Contains(own.ForLLM, "This session (cli:main): $0.00") {
		t.Errorf("an unspent session = %s", own.ForLLM)
	}
}

func TestUsageToolListsSessionsOnRequest(t *testing.T) {
	tool, m := usageTool(t)
	for _, key := range []string{"cli:main", "telegram:7"} {
		if _, err := m.Chat(inSession(key), &provider.Request{}); err != nil {
			t.Fatal(err)
		}
	}
	plain := tool.Execute(inSession("cli:main"), map[string]any{})
	if strings.Contains(plain.ForLLM, "By session:") {
		t.Errorf("the listing appeared unasked:\n%s", plain.ForLLM)
	}
	listed := tool.Execute(inSession("cli:main"), map[string]any{"sessions": true})
	for _, want := range []string{"By session:", "cli:main", "telegram:7"} {
		if !strings.Contains(listed.ForLLM, want) {
			t.Errorf("listing is missing %q:\n%s", want, listed.ForLLM)
		}
	}
}

func TestSessionTableBoundsItsLongTail(t *testing.T) {
	if got := sessionTable(nil); !strings.Contains(got, "spent anything yet") {
		t.Errorf("empty table = %q", got)
	}
	sessions := map[string]Totals{}
	for i := 0; i < maxSessionRows+7; i++ {
		sessions[fmt.Sprintf("cli:s%02d", i)] = Totals{USD: float64(i), Calls: 1}
	}
	got := sessionTable(sessions)
	if !strings.Contains(got, "… and 7 more") {
		t.Errorf("table did not say what it left out:\n%s", got)
	}
	if strings.Count(got, "cli:s") != maxSessionRows {
		t.Errorf("rows = %d, want %d:\n%s", strings.Count(got, "cli:s"), maxSessionRows, got)
	}
}

func TestUsageToolWithoutASessionKeyStillAnswers(t *testing.T) {
	tool, _ := usageTool(t)
	res := tool.Execute(context.Background(), map[string]any{"sessions": true})
	if res.IsError || !strings.Contains(res.ForLLM, "All time:") {
		t.Errorf("result = %+v", res)
	}
}

var _ tools.Tool = (*Tool)(nil)
