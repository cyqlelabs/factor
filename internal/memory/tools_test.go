package memory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cyqlelabs/factor/internal/tools"
)

// stubEngine drives every branch of the memory tools without a server.
// It is mutex-guarded because ambient storage runs on its own goroutine.
type stubEngine struct {
	mu         sync.Mutex
	remembered []RememberRequest
	recalls    []Memory
	forgot     [2]string
	reflected  bool
	healthy    bool
	enabled    bool
	err        error
}

func (s *stubEngine) setHealthy(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthy = v
}

func (s *stubEngine) stored() []RememberRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RememberRequest(nil), s.remembered...)
}

func (s *stubEngine) Remember(_ context.Context, req RememberRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	s.remembered = append(s.remembered, req)
	if req.Content == "dedup-me" {
		return "", nil // engine filtered/deduplicated it
	}
	return "atom-7", nil
}
func (s *stubEngine) Recall(context.Context, string, int, float64) ([]Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recalls, s.err
}
func (s *stubEngine) Forget(_ context.Context, query, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgot = [2]string{query, reason}
	return s.err
}
func (s *stubEngine) Reflect(context.Context) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	s.reflected = true
	return map[string]any{"beliefs_updated": 3, "atoms_pruned": 1}, nil
}
func (s *stubEngine) Status(context.Context) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return map[string]any{"total_atoms": 42}, nil
}
func (s *stubEngine) Enabled() bool { return s.enabled }
func (s *stubEngine) Healthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthy
}
func (s *stubEngine) Close() error { return nil }

func toolByName(t *testing.T, set []tools.Tool, name string) tools.Tool {
	t.Helper()
	for _, tool := range set {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %q not registered; have %d tools", name, len(set))
	return nil
}

func TestMemoryToolContracts(t *testing.T) {
	set := NewTools(&stubEngine{})
	want := []string{"remember", "recall", "forget", "reflect", "memory_status"}
	if len(set) != len(want) {
		t.Fatalf("NewTools returned %d tools, want %d", len(set), len(want))
	}
	for _, name := range want {
		tool := toolByName(t, set, name)
		if tool.Description() == "" {
			t.Errorf("%s has no description", name)
		}
		params := tool.Parameters()
		if params["type"] != "object" {
			t.Errorf("%s schema type = %v", name, params["type"])
		}
		props, ok := params["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s schema has no properties object", name)
		}
		for _, req := range asStrings(params["required"]) {
			if _, present := props[req]; !present {
				t.Errorf("%s requires %q which is not in properties", name, req)
			}
		}
	}
}

func asStrings(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestRememberTool(t *testing.T) {
	engine := &stubEngine{}
	tool := toolByName(t, NewTools(engine), "remember")
	ctx := context.Background()

	res := tool.Execute(ctx, map[string]any{
		"content":  "Nico ships Go on low-resource boxes",
		"type":     "belief",
		"valence":  -0.6,
		"evidence": "observed twice",
	})
	if res.IsError || !strings.Contains(res.ForLLM, "atom-7") {
		t.Fatalf("res = %+v", res)
	}
	got := engine.remembered[0]
	if got.Type != "belief" || got.Evidence != "observed twice" {
		t.Errorf("request = %+v", got)
	}
	if got.Valence == nil || *got.Valence != -0.6 {
		t.Errorf("valence not forwarded: %+v", got.Valence)
	}
	if got.Probability != 0.8 {
		t.Errorf("default probability = %v", got.Probability)
	}

	// omitted valence stays nil so smrti auto-estimates it
	tool.Execute(ctx, map[string]any{"content": "no valence", "probability": 0.5})
	if last := engine.remembered[1]; last.Valence != nil || last.Probability != 0.5 {
		t.Errorf("second request = %+v", last)
	}

	// engine filtered the memory: empty atom id is not an error
	res = tool.Execute(ctx, map[string]any{"content": "dedup-me"})
	if res.IsError || !strings.Contains(res.ForLLM, "filtered") {
		t.Errorf("dedup result = %+v", res)
	}

	engine.err = errors.New("boom")
	if res := tool.Execute(ctx, map[string]any{"content": "x"}); !res.IsError {
		t.Error("engine failure not reported")
	}
}

func TestRecallTool(t *testing.T) {
	engine := &stubEngine{recalls: []Memory{{
		Content: "force-push broke CI", Type: "episode", Severity: SeverityCriticalWarning,
		Salience: 0.81, Confidence: 0.92, Valence: -0.7,
	}}}
	tool := toolByName(t, NewTools(engine), "recall")

	res := tool.Execute(context.Background(), map[string]any{"query": "deploys", "top_k": 3})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	for _, want := range []string{"episode", "critical_warning", "force-push broke CI", "0.81", "-0.70"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("missing %q in %q", want, res.ForLLM)
		}
	}

	engine.recalls = nil
	if res := tool.Execute(context.Background(), map[string]any{"query": "x"}); res.IsError || !strings.Contains(res.ForLLM, "No memories") {
		t.Errorf("empty recall = %+v", res)
	}

	engine.err = errors.New("down")
	if res := tool.Execute(context.Background(), map[string]any{"query": "x"}); !res.IsError {
		t.Error("recall failure not reported")
	}
}

func TestForgetReflectStatusTools(t *testing.T) {
	engine := &stubEngine{healthy: true}
	set := NewTools(engine)
	ctx := context.Background()

	res := toolByName(t, set, "forget").Execute(ctx, map[string]any{"query": "wrong fact", "reason": "user corrected it"})
	if res.IsError {
		t.Fatalf("forget = %+v", res)
	}
	if engine.forgot != [2]string{"wrong fact", "user corrected it"} {
		t.Errorf("forget args = %v", engine.forgot)
	}

	res = toolByName(t, set, "reflect").Execute(ctx, nil)
	if res.IsError || !engine.reflected || !strings.Contains(res.ForLLM, "beliefs_updated") {
		t.Fatalf("reflect = %+v", res)
	}

	res = toolByName(t, set, "memory_status").Execute(ctx, nil)
	if res.IsError || !strings.Contains(res.ForLLM, "total_atoms") {
		t.Fatalf("status = %+v", res)
	}

	// an unhealthy engine reports plainly instead of erroring out obscurely
	engine.healthy = false
	if res := toolByName(t, set, "memory_status").Execute(ctx, nil); !res.IsError ||
		!strings.Contains(res.ForLLM, "not reachable") {
		t.Errorf("unhealthy status = %+v", res)
	}

	engine.healthy = true
	engine.err = errors.New("nope")
	for _, name := range []string{"forget", "reflect", "memory_status"} {
		if res := toolByName(t, set, name).Execute(ctx, map[string]any{"query": "x"}); !res.IsError {
			t.Errorf("%s did not report the engine failure", name)
		}
	}
}

func TestCompactJSONFallsBackOnUnmarshalableValues(t *testing.T) {
	out := compactJSON(map[string]any{"fn": func() {}})
	if out == "" {
		t.Error("compactJSON returned nothing for an unmarshalable value")
	}
	if got := compactJSON(map[string]any{"a": 1}); !strings.Contains(got, `"a": 1`) {
		t.Errorf("compactJSON = %q", got)
	}
}

func TestNoopEngine(t *testing.T) {
	var eng Engine = Noop{}
	ctx := context.Background()
	if id, err := eng.Remember(ctx, RememberRequest{Content: "x"}); id != "" || err != nil {
		t.Errorf("Remember = %q, %v", id, err)
	}
	if mems, err := eng.Recall(ctx, "q", 5, 0.1); mems != nil || err != nil {
		t.Errorf("Recall = %v, %v", mems, err)
	}
	if err := eng.Forget(ctx, "q", ""); err != nil {
		t.Errorf("Forget = %v", err)
	}
	if r, err := eng.Reflect(ctx); err != nil || r == nil {
		t.Errorf("Reflect = %v, %v", r, err)
	}
	status, err := eng.Status(ctx)
	if err != nil || status["mode"] != "off" {
		t.Errorf("Status = %v, %v", status, err)
	}
	if eng.Enabled() || eng.Healthy() {
		t.Error("Noop must report itself disabled and unhealthy")
	}
	if err := eng.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}
