package memory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
)

func TestBuildRecallQueryDefaults(t *testing.T) {
	if got := BuildRecallQuery(nil, "just this", 0, 0); got != "just this" {
		t.Errorf("zero args should fall back to defaults: %q", got)
	}
	if got := BuildRecallQuery(nil, "   ", 5, 100); got != "" {
		t.Errorf("blank current message produced %q", got)
	}
}

func TestFormatMemoriesUsesLabelWhenContentEmpty(t *testing.T) {
	out := FormatMemories([]Memory{
		{Label: "Alice", Content: "  ", Severity: SeverityContext, Confidence: 0.5},
		{Label: "", Content: "", Severity: SeverityContext, Confidence: 0.5},
	}, 0)
	if !strings.Contains(out, "Alice") {
		t.Errorf("concept label not used as content: %q", out)
	}
	if strings.Count(out, "Note (") != 1 {
		t.Errorf("empty label+content should be dropped entirely: %q", out)
	}
}

func TestFormatMemoriesClipsLongContent(t *testing.T) {
	long := strings.Repeat("x", 300)
	out := FormatMemories([]Memory{{Content: long, Severity: SeverityContext, Confidence: 1}}, 50)
	if !strings.Contains(out, "…") {
		t.Error("long memory not clipped")
	}
	if strings.Contains(out, long) {
		t.Error("clip did not shorten the content")
	}
}

func TestNewAmbientIgnoresBadPatterns(t *testing.T) {
	a := NewAmbient(&stubEngine{enabled: true, healthy: true}, 5, 0.3, 5, 500, 500,
		[]string{"([unclosed", "^ok$"}, SpacePolicy{})
	if len(a.ignore) != 1 {
		t.Fatalf("compiled %d patterns, want only the valid one", len(a.ignore))
	}
	if !a.ignored("ok") || a.ignored("something else") {
		t.Error("valid pattern not applied after a bad one was skipped")
	}
}

func TestMemoryPromptNilSafety(t *testing.T) {
	var nilAmbient *Ambient
	if got := nilAmbient.MemoryPrompt(context.Background(), nil, "q"); got != "" {
		t.Errorf("nil ambient returned %q", got)
	}
	noEngine := &Ambient{}
	if got := noEngine.MemoryPrompt(context.Background(), nil, "q"); got != "" {
		t.Errorf("engine-less ambient returned %q", got)
	}
	// healthy engine but an empty query short-circuits before any call
	a := NewAmbient(&stubEngine{enabled: true, healthy: true}, 5, 0.3, 5, 500, 500, nil, SpacePolicy{})
	if got := a.MemoryPrompt(context.Background(), nil, "   "); got != "" {
		t.Errorf("blank query produced %q", got)
	}
}

func TestStoreExchangeNilAndDisabled(t *testing.T) {
	var nilAmbient *Ambient
	nilAmbient.StoreExchange("cli", "", "u", "a") // must not panic

	(&Ambient{}).StoreExchange("cli", "", "u", "a") // engine-less: must not panic

	disabled := NewAmbient(&stubEngine{enabled: false}, 5, 0.3, 5, 500, 500, nil, SpacePolicy{})
	disabled.StoreExchange("cli", "", "u", "a") // off-mode engines never store
	if stored := disabled.Engine.(*stubEngine).stored(); len(stored) != 0 {
		t.Error("disabled engine received a write")
	}
}

func TestStoreExchangeWaitsForHealthThenStores(t *testing.T) {
	engine := &stubEngine{enabled: true, healthy: false}
	a := NewAmbient(engine, 5, 0.3, 5, 500, 500, nil, SpacePolicy{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		engine.setHealthy(true)
	}()
	done := make(chan struct{})
	go func() { a.StoreExchange("cli", "", "user text", "assistant text"); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("StoreExchange never returned")
	}
	stored := engine.stored()
	if len(stored) != 2 {
		t.Fatalf("stored %d memories, want 2", len(stored))
	}
	// Provenance rides on Source, not on an English prefix baked into Content.
	if stored[0].Content != "user text" || stored[0].Source != SourceUser {
		t.Errorf("user side = %+v", stored[0])
	}
	if stored[1].Content != "assistant text" || stored[1].Source != SourceAgent {
		t.Errorf("assistant side = %+v", stored[1])
	}
}

// The assistant's own turns must never be stored as if the user had said them:
// smrti weights user-authored memory above agent-authored memory and only ever
// prunes the latter, so an unmarked reply becomes a permanent graph fixture.
func TestStoreExchangeMarksTheAssistantSide(t *testing.T) {
	engine := &stubEngine{enabled: true, healthy: true}
	a := NewAmbient(engine, 5, 0.3, 5, 500, 500, nil, SpacePolicy{})
	a.StoreExchange("cli", "", "what should I do this weekend?", "here are some ideas: ...")
	stored := engine.stored()
	if len(stored) != 2 {
		t.Fatalf("stored %d memories, want 2", len(stored))
	}
	if stored[1].Source != SourceAgent {
		t.Errorf("assistant turn stored with source %q, want %q", stored[1].Source, SourceAgent)
	}
}

func TestRememberOmitsSourceWhenUnset(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"status":"ok","atom_id":"a1"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "")
	if _, err := c.Remember(context.Background(), RememberRequest{Content: "hello"}); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	// Keeps the payload identical to what older Factor builds sent, so the
	// wire format only changes when there is provenance to actually report.
	if _, present := body["source"]; present {
		t.Errorf("source sent when unset: %+v", body)
	}
}

func TestRememberSendsSourceWhenSet(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"status":"ok","atom_id":"a1"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "")
	_, err := c.Remember(context.Background(), RememberRequest{
		Content: "a reply", Source: SourceAgent,
	})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if body["source"] != SourceAgent {
		t.Errorf("source = %v, want %q", body["source"], SourceAgent)
	}
}

func TestStoreExchangeSkipsBlankSides(t *testing.T) {
	engine := &stubEngine{enabled: true, healthy: true}
	a := NewAmbient(engine, 5, 0.3, 5, 500, 500, nil, SpacePolicy{})
	a.StoreExchange("cli", "", "   ", "only the reply")
	if stored := engine.stored(); len(stored) != 1 {
		t.Errorf("blank user text was stored: %+v", stored)
	}
}

func TestClientErrorPaths(t *testing.T) {
	var status int
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "", "")
	ctx := context.Background()

	// a healthy call first so we can watch health flip later
	status, body = 200, `{"total_atoms":1}`
	if _, err := c.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if !c.Healthy() {
		t.Fatal("client should be healthy")
	}

	// 4xx: an error, but the server is up — health is untouched
	status, body = 400, "bad request"
	if _, err := c.Recall(ctx, "q", 5, 0.1, Scope{}); err == nil {
		t.Error("4xx accepted")
	}
	if !c.Healthy() {
		t.Error("a 4xx must not mark the engine unhealthy")
	}

	// 5xx: the server is failing — health must flip so callers stop trusting it
	status, body = 500, strings.Repeat("e", 500)
	if err := c.Forget(ctx, "q", "", ""); err == nil {
		t.Error("5xx accepted")
	}
	if c.Healthy() {
		t.Error("a 5xx must mark the engine unhealthy")
	}

	// malformed success payload
	status, body = 200, "{not json"
	if _, err := c.Reflect(ctx); err == nil {
		t.Error("malformed JSON accepted")
	}
}

func TestClientRecallEmptyQueryShortCircuits(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"memories":[]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "", "")
	mems, err := c.Recall(context.Background(), "", 5, 0.1, Scope{})
	if err != nil || mems != nil {
		t.Errorf("Recall(empty) = %v, %v", mems, err)
	}
	if called {
		t.Error("empty query still hit the server")
	}
}

func TestClientRememberDefaultsAndBeliefWithoutEvidence(t *testing.T) {
	var paths []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		_, _ = w.Write([]byte(`{"status":"ok","atom_id":"a1"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "", "")

	// type and probability default when omitted
	if _, err := c.Remember(context.Background(), RememberRequest{Content: "x"}); err != nil {
		t.Fatal(err)
	}
	if bodies[0]["type"] != "episode" || bodies[0]["probability"] != 0.8 {
		t.Errorf("defaults not applied: %v", bodies[0])
	}

	// a belief WITHOUT evidence goes to /remember, not /believe
	if _, err := c.Remember(context.Background(), RememberRequest{Content: "y", Type: "belief"}); err != nil {
		t.Fatal(err)
	}
	if paths[1] != "/remember" {
		t.Errorf("belief without evidence used %s", paths[1])
	}
}

func TestClientCloseAndUnreachableRequest(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "", "")
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	if _, err := c.Status(context.Background()); err == nil {
		t.Error("unreachable server accepted")
	}
	if err := c.CheckHealth(context.Background()); err == nil {
		t.Error("CheckHealth on an unreachable server returned nil")
	}
}

// In a house with more than one voice, a memory recorded against nobody in
// particular is one the agent hands back to the wrong person: the speaker is
// named in the content, because Source carries standing (human or agent) and
// is a closed set, not a place to put a name.
func TestStoreExchangeAttributesANamedSpeaker(t *testing.T) {
	engine := &stubEngine{enabled: true, healthy: true}
	a := NewAmbient(engine, 5, 0.3, 5, 500, 500, nil, SpacePolicy{})
	a.StoreExchange("voice", "Roxana", "me gusta el café sin azúcar", "anotado")

	stored := engine.stored()
	if len(stored) != 2 {
		t.Fatalf("stored %d memories, want 2", len(stored))
	}
	if stored[0].Content != "Roxana: me gusta el café sin azúcar" {
		t.Errorf("the speaker's memory reads %q", stored[0].Content)
	}
	// Standing is unchanged: a named person is still a human asserting it.
	if stored[0].Source != SourceUser {
		t.Errorf("source = %q, want %q — a name is not a standing", stored[0].Source, SourceUser)
	}
	// The agent's own reply is never attributed to the person it answered.
	if stored[1].Content != "anotado" || stored[1].Source != SourceAgent {
		t.Errorf("assistant side = %+v", stored[1])
	}
}

// The single-voice case is every other channel and the machine's owner: no
// name, and memories that read exactly as they did before speakers existed.
func TestStoreExchangeLeavesAnUnnamedSpeakerAlone(t *testing.T) {
	engine := &stubEngine{enabled: true, healthy: true}
	a := NewAmbient(engine, 5, 0.3, 5, 500, 500, nil, SpacePolicy{})
	a.StoreExchange("voice", "", "me gusta el café sin azúcar", "anotado")
	if stored := engine.stored(); stored[0].Content != "me gusta el café sin azúcar" {
		t.Errorf("an unattributed memory reads %q", stored[0].Content)
	}
}

// recordingEngine answers each recall query from a script and remembers the
// order the queries arrived in.
type recordingEngine struct {
	Noop
	mu      sync.Mutex
	queries []string
	answers map[string][]Memory
	err     error
}

func (e *recordingEngine) Healthy() bool { return true }
func (e *recordingEngine) Recall(_ context.Context, query string, _ int, _ float64, _ Scope) ([]Memory, error) {
	e.mu.Lock()
	e.queries = append(e.queries, query)
	e.mu.Unlock()
	if e.err != nil {
		return nil, e.err
	}
	return e.answers[query], nil
}

func TestInterleaveLeadsWithTheFirstListAndDropsDuplicates(t *testing.T) {
	first := []Memory{{ID: "a", Content: "a"}, {ID: "b", Content: "b"}}
	second := []Memory{{ID: "b", Content: "b"}, {ID: "c", Content: "c"}}
	got := interleave(first, second, 5)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("interleave returned %d memories, want %d: %+v", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("interleave[%d].ID = %q, want %q", i, got[i].ID, id)
		}
	}
}

func TestInterleaveRespectsTheLimit(t *testing.T) {
	first := []Memory{{ID: "a"}, {ID: "b"}}
	second := []Memory{{ID: "c"}, {ID: "d"}}
	got := interleave(first, second, 3)
	if len(got) != 3 {
		t.Fatalf("interleave(limit 3) returned %d", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "c" {
		t.Errorf("interleave did not alternate: %q, %q", got[0].ID, got[1].ID)
	}
}

func TestMemoryPromptAsksWithTheTurnsOwnMessageAndTheTail(t *testing.T) {
	// The regression: a subject change is a few words inside a tail about
	// something else, so the tail alone never returns the fact.
	current := "qué sabes sobre Esmeralda?"
	tail := "una charla larga sobre otra cosa\n" + current
	eng := &recordingEngine{answers: map[string][]Memory{
		current: {{ID: "fact", Content: "Esmeralda is his daughter", Confidence: 0.95}},
		tail:    {{ID: "froth", Content: "otra cosa", Confidence: 0.6}},
	}}
	a := NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, SpacePolicy{})
	got := a.MemoryPrompt(context.Background(),
		[]provider.Message{{Role: "user", Content: "una charla larga sobre otra cosa"}}, current)

	if !strings.Contains(got, "Esmeralda is his daughter") {
		t.Errorf("the turn's own message was never asked; prompt = %q", got)
	}
	if !strings.Contains(got, "otra cosa") {
		t.Errorf("the conversation tail was never asked; prompt = %q", got)
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.queries) != 2 {
		t.Fatalf("issued %d recalls, want 2: %q", len(eng.queries), eng.queries)
	}
}

func TestMemoryPromptAsksOnceWhenThereIsNoHistory(t *testing.T) {
	eng := &recordingEngine{answers: map[string][]Memory{"hola": {{ID: "x", Content: "x"}}}}
	a := NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, SpacePolicy{})
	a.MemoryPrompt(context.Background(), nil, "hola")
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.queries) != 1 {
		t.Errorf("issued %d recalls with no history, want 1: %q", len(eng.queries), eng.queries)
	}
}

func TestMemoryPromptSurvivesAFailedRecall(t *testing.T) {
	eng := &recordingEngine{err: errors.New("engine down")}
	a := NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, SpacePolicy{})
	if got := a.MemoryPrompt(context.Background(),
		[]provider.Message{{Role: "user", Content: "earlier"}}, "now"); got != "" {
		t.Errorf("MemoryPrompt on a dead engine = %q, want \"\"", got)
	}
}
