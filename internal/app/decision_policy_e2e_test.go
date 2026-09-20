package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
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

// fixedJudge answers every question with one choice, over the same
// /v1/systemone route the managed model serves.
type fixedJudge struct {
	choice string
	mu     sync.Mutex
	asked  int
}

func (j *fixedJudge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "max_len": 1024, "head_max_len": 192})
		return
	}
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
	_ = json.NewEncoder(w).Encode(map[string]any{"answers": answers, "model": "laya-multilingual",
		"usage": map[string]any{"input_tokens": 200, "output_tokens": 0}})
}

// serveDecisions puts a decision model on a port of its own choosing and
// returns it. The supervisor probes before it spawns, so a model already
// answering is adopted rather than started a second time — which is the same
// path a Laya somebody runs themselves takes, and what lets a test stand in
// for one without a Python process.
func serveDecisions(t *testing.T, h http.Handler) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func policyApp(t *testing.T, llm *scriptedLLM, judge http.Handler, mode string) *App {
	t.Helper()
	llmSrv := httptest.NewServer(llm)
	t.Cleanup(llmSrv.Close)
	cfg := testConfig(t)
	cfg.Provider.Type = "openai"
	cfg.Provider.APIBase = llmSrv.URL + "/v1"
	cfg.Decision.Mode = mode
	cfg.Decision.Port = serveDecisions(t, judge)
	a := newTestApp(t, cfg)
	// The supervisor adopts the model already answering on that port; wait
	// for it to notice before the first turn asks anything of it.
	waitHealthy(t, a)
	return a
}

// waitHealthy blocks until the decision model is answering, or fails.
func waitHealthy(t *testing.T, a *App) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if a.Decisions.Healthy() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the decision model never became healthy: %s", a.Decisions.Down())
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
	// A decision answered on this machine is counted and costs nothing: the
	// same rule the cost catalog applies to a locally served LLM.
	if rec.USD != 0 {
		t.Errorf("a local decision was billed %v", rec.USD)
	}
	for _, m := range rec.Models {
		if m.Model == "jev-test" && m.Input == 0 {
			t.Error("the decision's tokens were not counted")
		}
	}
}

// Nothing here costs money: the model runs on this machine, so the turn
// records the decision and bills nothing for it.
func TestLocalDecisionsAreRecordedAndCostNothing(t *testing.T) {
	llm := &scriptedLLM{steps: []func([]wireMessage) (map[string]any, string){
		calls("read_file", map[string]any{"path": "missing.txt"}),
		says("The file is missing."),
	}}
	a := policyApp(t, llm, &fixedJudge{choice: "verified"}, "active")
	if _, err := a.Loop.ProcessDirect(context.Background(), "read missing.txt", "cli:free"); err != nil {
		t.Fatal(err)
	}
	rec := userTurn(t, a, "cli:free")
	if len(rec.Decisions) != 1 || rec.Decisions[0].Model != "laya-multilingual" {
		t.Errorf("decisions = %+v", rec.Decisions)
	}
	if rec.USD != 0 {
		t.Errorf("a local decision was billed %v", rec.USD)
	}
	snap := a.Cost.Snapshot("cli:free")
	if _, counted := snap.Models["laya-multilingual"]; counted {
		t.Error("the decision model reached the money ledger")
	}
}
