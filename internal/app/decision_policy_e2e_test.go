package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/decision"
	"github.com/cyqlelabs/factor/internal/trace"
)

// wireMessage is one chat message as the OpenAI dialect carries it.
type wireMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// writeCompletion answers one chat-completions request.
func writeCompletion(w http.ResponseWriter, message map[string]any, finish string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": message, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	})
}

// scriptedLLM answers the OpenAI wire from a list of steps, and keeps every
// request so a test can read what the model was actually sent.
type scriptedLLM struct {
	mu       sync.Mutex
	steps    []func(messages []wireMessage) (map[string]any, string)
	requests [][]wireMessage
}

func (m *scriptedLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []wireMessage `json:"messages"`
		Tools    []any         `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Tools) == 0 { // the filler, or a wrap-up
		writeCompletion(w, map[string]any{"role": "assistant", "content": "one moment"}, "stop")
		return
	}
	m.mu.Lock()
	m.requests = append(m.requests, req.Messages)
	step := len(m.requests) - 1
	m.mu.Unlock()
	if step >= len(m.steps) {
		writeCompletion(w, map[string]any{"role": "assistant", "content": "default final"}, "stop")
		return
	}
	message, finish := m.steps[step](req.Messages)
	writeCompletion(w, message, finish)
}

func says(content string) func([]wireMessage) (map[string]any, string) {
	return func([]wireMessage) (map[string]any, string) {
		return map[string]any{"role": "assistant", "content": content}, "stop"
	}
}

func calls(name string, args map[string]any) func([]wireMessage) (map[string]any, string) {
	return func([]wireMessage) (map[string]any, string) {
		encoded, _ := json.Marshal(args)
		return map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": fmt.Sprintf("call_%d", time.Now().UnixNano()), "type": "function",
				"function": map[string]any{"name": name, "arguments": string(encoded)}},
		}}, "tool_calls"
	}
}

// fixedJudge answers every question with one choice.
type fixedJudge struct {
	choice string
	mu     sync.Mutex
	asked  int
}

func (j *fixedJudge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Questions map[string]decision.Question `json:"questions"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	j.mu.Lock()
	j.asked++
	j.mu.Unlock()
	answers := map[string]decision.Answer{}
	for name, q := range req.Questions {
		ids := decision.Candidates(q)
		probs := map[string]float64{}
		for _, id := range ids {
			probs[id] = 0.1 / float64(max(len(ids)-1, 1))
		}
		probs[j.choice] = 0.9
		answers[name] = decision.Answer{Choice: j.choice, Confidence: 0.95, Probabilities: probs}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"answers": answers, "model": "jev-test",
		"usage": map[string]any{"input_tokens": 200, "output_tokens": 3}})
}

func policyApp(t *testing.T, llm *scriptedLLM, judge http.Handler, mode string) *App {
	t.Helper()
	llmSrv := httptest.NewServer(llm)
	t.Cleanup(llmSrv.Close)
	judgeSrv := httptest.NewServer(judge)
	t.Cleanup(judgeSrv.Close)
	cfg := testConfig(t)
	cfg.Provider.Type = "openai"
	cfg.Provider.APIBase = llmSrv.URL + "/v1"
	cfg.Decision.Mode = mode
	cfg.Decision.APIKey = "ts-e2e-key-12345"
	cfg.Decision.APIBase = judgeSrv.URL
	return newTestApp(t, cfg)
}

func userTurn(t *testing.T, a *App, session string) trace.Record {
	t.Helper()
	recs, err := trace.Since(a.Traces(), time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Session == session && r.Trigger == "user" {
			return r
		}
	}
	t.Fatalf("no trace for %s among %d records", session, len(recs))
	return trace.Record{}
}

// Through the whole app: a reply that claims work its tools do not show is
// held by the completion check, the model's correction is what comes back,
// and the trace carries the overclaim, the decision and its price.
func TestOverclaimIsCorrectedThroughTheWholeApp(t *testing.T) {
	llm := &scriptedLLM{steps: []func([]wireMessage) (map[string]any, string){
		calls("read_file", map[string]any{"path": "missing.txt"}),
		says("I read the file and emailed it to you."),
		func(messages []wireMessage) (map[string]any, string) {
			last, _ := messages[len(messages)-1].Content.(string)
			if !strings.Contains(last, "[Verification from the system") {
				return map[string]any{"role": "assistant", "content": "no hold arrived: " + last}, "stop"
			}
			return map[string]any{"role": "assistant", "content": "Correction: the file is missing and nothing was emailed."}, "stop"
		},
	}}
	a := policyApp(t, llm, &fixedJudge{choice: "overclaimed"}, "active")
	reply, err := a.Loop.ProcessDirect(context.Background(), "email me missing.txt", "cli:hold")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply, "Correction:") {
		t.Fatalf("reply = %q", reply)
	}
	rec := userTurn(t, a, "cli:hold")
	if rec.Count(trace.EventOverclaim) != 1 {
		t.Errorf("overclaim events = %d", rec.Count(trace.EventOverclaim))
	}
	if len(rec.Decisions) != 1 || rec.Decisions[0].Kind != decision.KindCompletion || rec.Decisions[0].Result != "acted" {
		t.Errorf("decisions = %+v", rec.Decisions)
	}
	if rec.USD <= 0 {
		t.Error("the decision was not billed to the turn")
	}
}

// The same model, the same judge, in shadow: the reply stands, the decision
// is on the trace as shadow, and browser_run is not offered.
func TestShadowModeRecordsAndChangesNothingThroughTheWholeApp(t *testing.T) {
	llm := &scriptedLLM{steps: []func([]wireMessage) (map[string]any, string){
		calls("read_file", map[string]any{"path": "missing.txt"}),
		says("I read the file and emailed it to you."),
	}}
	judge := &fixedJudge{choice: "overclaimed"}
	a := policyApp(t, llm, judge, "shadow")
	reply, err := a.Loop.ProcessDirect(context.Background(), "email me missing.txt", "cli:shadow")
	if err != nil || reply != "I read the file and emailed it to you." {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if judge.asked != 1 {
		t.Errorf("the judge was asked %d times, want 1", judge.asked)
	}
	rec := userTurn(t, a, "cli:shadow")
	if len(rec.Decisions) != 1 || rec.Decisions[0].Result != "shadow" || rec.Count(trace.EventOverclaim) != 0 {
		t.Errorf("decisions = %+v events = %+v", rec.Decisions, rec.Events)
	}
	if _, ok := a.Registry.Get("browser_run"); ok {
		t.Error("shadow mode mounted a tool that acts")
	}
}

// A dead decision endpoint costs nothing but a fallback on the trace: the
// turn answers as it always did.
func TestDeadDecisionEndpointFallsBackThroughTheWholeApp(t *testing.T) {
	llm := &scriptedLLM{steps: []func([]wireMessage) (map[string]any, string){
		calls("read_file", map[string]any{"path": "missing.txt"}),
		says("The file is missing."),
	}}
	dead := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	a := policyApp(t, llm, dead, "active")
	reply, err := a.Loop.ProcessDirect(context.Background(), "read missing.txt", "cli:dead")
	if err != nil || reply != "The file is missing." {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	rec := userTurn(t, a, "cli:dead")
	if rec.DecisionFallbacks() != 1 {
		t.Errorf("decisions = %+v", rec.Decisions)
	}
}

// A stall through the whole app: three identical failing reads, the system
// note with the decided recovery, and the model changing course on it.
func TestStallRecoveryThroughTheWholeApp(t *testing.T) {
	var note string
	repeat := calls("read_file", map[string]any{"path": "nowhere.txt"})
	llm := &scriptedLLM{steps: []func([]wireMessage) (map[string]any, string){
		repeat, repeat, repeat,
		func(messages []wireMessage) (map[string]any, string) {
			for _, m := range messages {
				if s, _ := m.Content.(string); m.Role == "user" && strings.Contains(s, "[Note from the system") {
					note = s
				}
			}
			return map[string]any{"role": "assistant", "content": "I will list the directory instead."}, "stop"
		},
	}}
	a := policyApp(t, llm, &fixedJudge{choice: "alternative_approach"}, "active")
	if _, err := a.Loop.ProcessDirect(context.Background(), "read nowhere.txt", "cli:stall"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "read_file(path=nowhere.txt) has now returned the same result 3 times") ||
		!strings.Contains(note, "another way to the same end") {
		t.Errorf("note = %q", note)
	}
	rec := userTurn(t, a, "cli:stall")
	if rec.Count(trace.EventStall) != 1 {
		t.Errorf("stall events = %d", rec.Count(trace.EventStall))
	}
	kinds := map[string]int{}
	for _, d := range rec.Decisions {
		kinds[d.Kind]++
	}
	if kinds[decision.KindRecovery] != 1 || kinds[decision.KindCompletion] != 1 {
		t.Errorf("decision kinds = %v", kinds)
	}
}
