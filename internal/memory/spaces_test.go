package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/cyqlelabs/factor/internal/tools"
)

const (
	statusWithSpaces    = `{"total_atoms":1,"by_type":{},"personality":{},"spaces":["main"],"version":"0.9.0"}`
	statusWithoutSpaces = `{"total_atoms":1,"by_type":{},"personality":{}}`
)

// spaceCapture records the JSON body of every POST and serves a swappable /status.
type spaceCapture struct {
	mu     sync.Mutex
	bodies map[string]map[string]any
	status string
}

func (c *spaceCapture) setStatus(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = s
}

func (c *spaceCapture) body(path string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies[path]
}

func newSpaceCaptureServer(t *testing.T, statusJSON string) (*httptest.Server, *spaceCapture) {
	t.Helper()
	c := &spaceCapture{bodies: map[string]map[string]any{}, status: statusJSON}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if r.URL.Path == "/status" {
			_, _ = w.Write([]byte(c.status))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.bodies[r.URL.Path] = body
		if r.URL.Path == "/recall" {
			_, _ = w.Write([]byte(`{"memories":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","atom_id":"a1"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func TestSpaceFieldsOmittedUntilStatusProvesSupport(t *testing.T) {
	srv, cap := newSpaceCaptureServer(t, statusWithoutSpaces)
	c := NewClient(srv.URL, "", "")
	ctx := context.Background()

	// Before any status probe the engine's capabilities are unknown: an old
	// engine would silently misroute a space field into its default space, so
	// nothing may be sent.
	if _, err := c.Remember(ctx, RememberRequest{Content: "x", Space: "system"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cap.body("/remember")["space"]; ok {
		t.Error("space sent before any status probe")
	}

	// A status without a spaces key is an old engine: still nothing.
	if err := c.CheckHealth(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remember(ctx, RememberRequest{Content: "x", Space: "system"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cap.body("/remember")["space"]; ok {
		t.Error("space sent to an engine that never advertised spaces")
	}

	if _, err := c.Recall(ctx, "q", 5, 0.1, Scope{Space: "system", ReadSpaces: []string{"system", "main"}}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"space", "read_spaces"} {
		if _, ok := cap.body("/recall")[k]; ok {
			t.Errorf("%s sent to an engine that never advertised spaces", k)
		}
	}

	if err := c.Forget(ctx, "q", "", "system"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cap.body("/forget")["space"]; ok {
		t.Error("space sent to an engine that never advertised spaces")
	}
}

func TestSpaceFieldsSentOnceStatusAdvertisesSpaces(t *testing.T) {
	srv, cap := newSpaceCaptureServer(t, statusWithSpaces)
	c := NewClient(srv.URL, "", "")
	ctx := context.Background()
	if err := c.CheckHealth(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Remember(ctx, RememberRequest{Content: "x", Space: "system"}); err != nil {
		t.Fatal(err)
	}
	if got := cap.body("/remember")["space"]; got != "system" {
		t.Errorf("remember space = %v, want system", got)
	}

	if _, err := c.Remember(ctx, RememberRequest{Content: "b", Type: "belief", Evidence: "e", Probability: 0.9, Space: "system"}); err != nil {
		t.Fatal(err)
	}
	if got := cap.body("/believe")["space"]; got != "system" {
		t.Errorf("believe space = %v, want system", got)
	}

	if _, err := c.Recall(ctx, "q", 5, 0.1, Scope{Space: "system", ReadSpaces: []string{"system", "main"}}); err != nil {
		t.Fatal(err)
	}
	if got := cap.body("/recall")["space"]; got != "system" {
		t.Errorf("recall space = %v, want system", got)
	}
	rs, _ := cap.body("/recall")["read_spaces"].([]any)
	if len(rs) != 2 || rs[0] != "system" || rs[1] != "main" {
		t.Errorf("recall read_spaces = %v, want [system main]", rs)
	}

	if err := c.Forget(ctx, "q", "why", "system"); err != nil {
		t.Fatal(err)
	}
	if got := cap.body("/forget")["space"]; got != "system" {
		t.Errorf("forget space = %v, want system", got)
	}
}

func TestZeroScopeStaysByteIdenticalEvenWhenCapable(t *testing.T) {
	srv, cap := newSpaceCaptureServer(t, statusWithSpaces)
	c := NewClient(srv.URL, "", "")
	ctx := context.Background()
	if err := c.CheckHealth(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Remember(ctx, RememberRequest{Content: "x"}); err != nil {
		t.Fatal(err)
	}
	for k := range cap.body("/remember") {
		if k != "content" && k != "type" && k != "probability" {
			t.Errorf("unexpected remember field %q with a zero scope", k)
		}
	}

	if _, err := c.Recall(ctx, "q", 5, 0.1, Scope{}); err != nil {
		t.Fatal(err)
	}
	for k := range cap.body("/recall") {
		if k != "query" && k != "top_k" && k != "min_confidence" {
			t.Errorf("unexpected recall field %q with a zero scope", k)
		}
	}
}

func TestSpacePolicyScopes(t *testing.T) {
	p := SpacePolicy{Strategy: "origin", Main: "main", System: "system"}
	sysScope := Scope{Space: "system", ReadSpaces: []string{"system", "main"}}
	mainScope := Scope{Space: "main", ReadSpaces: []string{"main", "system"}}
	for channel, want := range map[string]Scope{
		"cron":     sysScope,
		"job":      sysScope,
		"system":   sysScope,
		"cli":      mainScope,
		"telegram": mainScope,
		"phone":    mainScope,
		"":         mainScope,
	} {
		if got := p.Scope(channel); !reflect.DeepEqual(got, want) {
			t.Errorf("Scope(%q) = %+v, want %+v", channel, got, want)
		}
	}

	single := SpacePolicy{Strategy: "single", Main: "main", System: "system"}
	if got := single.Scope("cron"); !reflect.DeepEqual(got, Scope{}) {
		t.Errorf("single-strategy Scope = %+v, want zero", got)
	}
	if got := (SpacePolicy{}).Scope("cron"); !reflect.DeepEqual(got, Scope{}) {
		t.Errorf("zero-policy Scope = %+v, want zero", got)
	}
}

// scopeEngine records the scope and requests the ambient layer hands the engine.
type scopeEngine struct {
	Noop
	mu          sync.Mutex
	recallScope Scope
	forgotSpace string
	remembered  []RememberRequest
}

func (e *scopeEngine) Recall(_ context.Context, _ string, _ int, _ float64, scope Scope) ([]Memory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recallScope = scope
	return []Memory{{Content: "a note", Confidence: 0.8, Space: "system"}}, nil
}

func (e *scopeEngine) Forget(_ context.Context, _, _, space string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forgotSpace = space
	return nil
}

func (e *scopeEngine) Remember(_ context.Context, req RememberRequest) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.remembered = append(e.remembered, req)
	return "id-1", nil
}

func (e *scopeEngine) Enabled() bool { return true }
func (e *scopeEngine) Healthy() bool { return true }

func testPolicy() SpacePolicy {
	return SpacePolicy{Strategy: "origin", Main: "main", System: "system"}
}

func TestMemoryPromptScopesRecallByTurnChannel(t *testing.T) {
	eng := &scopeEngine{}
	a := NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, testPolicy())

	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{Channel: "cron", ChatID: "job1"})
	if got := a.MemoryPrompt(ctx, nil, "what happened overnight"); got == "" {
		t.Fatal("MemoryPrompt returned nothing")
	}
	want := Scope{Space: "system", ReadSpaces: []string{"system", "main"}}
	if !reflect.DeepEqual(eng.recallScope, want) {
		t.Errorf("cron recall scope = %+v, want %+v", eng.recallScope, want)
	}

	ctx = tools.WithToolContext(context.Background(), tools.ToolContext{Channel: "telegram", ChatID: "5"})
	a.MemoryPrompt(ctx, nil, "hola")
	want = Scope{Space: "main", ReadSpaces: []string{"main", "system"}}
	if !reflect.DeepEqual(eng.recallScope, want) {
		t.Errorf("telegram recall scope = %+v, want %+v", eng.recallScope, want)
	}
}

func TestStoreExchangeWritesToTheChannelSpace(t *testing.T) {
	eng := &scopeEngine{}
	a := NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, testPolicy())

	a.StoreExchange("cron", "job output", "noted")
	a.StoreExchange("telegram", "hola", "hola, Nico")

	if len(eng.remembered) != 4 {
		t.Fatalf("remembered %d requests, want 4", len(eng.remembered))
	}
	for i, wantSpace := range []string{"system", "system", "main", "main"} {
		if eng.remembered[i].Space != wantSpace {
			t.Errorf("remembered[%d].Space = %q, want %q", i, eng.remembered[i].Space, wantSpace)
		}
	}
	if eng.remembered[0].Source != SourceUser || eng.remembered[1].Source != SourceAgent {
		t.Errorf("sources = %q, %q", eng.remembered[0].Source, eng.remembered[1].Source)
	}
}

func TestMemoryToolsDefaultToTheTurnScope(t *testing.T) {
	eng := &scopeEngine{}
	set := NewTools(eng, testPolicy())
	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{Channel: "cron", ChatID: "digest"})

	if res := toolByName(t, set, "remember").Execute(ctx, map[string]any{"content": "job ran"}); res.IsError {
		t.Fatalf("remember = %+v", res)
	}
	if got := eng.remembered[0].Space; got != "system" {
		t.Errorf("remember space = %q, want system", got)
	}

	if res := toolByName(t, set, "recall").Execute(ctx, map[string]any{"query": "jobs"}); res.IsError {
		t.Fatalf("recall = %+v", res)
	}
	want := Scope{Space: "system", ReadSpaces: []string{"system", "main"}}
	if !reflect.DeepEqual(eng.recallScope, want) {
		t.Errorf("recall scope = %+v, want %+v", eng.recallScope, want)
	}

	if res := toolByName(t, set, "forget").Execute(ctx, map[string]any{"query": "stale"}); res.IsError {
		t.Fatalf("forget = %+v", res)
	}
	if eng.forgotSpace != "system" {
		t.Errorf("forget space = %q, want system", eng.forgotSpace)
	}
}

func TestMemoryToolsHonorAnExplicitSpace(t *testing.T) {
	eng := &scopeEngine{}
	set := NewTools(eng, testPolicy())
	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{Channel: "telegram", ChatID: "5"})

	toolByName(t, set, "remember").Execute(ctx, map[string]any{"content": "x", "space": "projects"})
	if got := eng.remembered[0].Space; got != "projects" {
		t.Errorf("remember space = %q, want projects", got)
	}

	toolByName(t, set, "recall").Execute(ctx, map[string]any{"query": "q", "space": "projects"})
	want := Scope{Space: "projects", ReadSpaces: []string{"projects"}}
	if !reflect.DeepEqual(eng.recallScope, want) {
		t.Errorf("recall scope = %+v, want %+v", eng.recallScope, want)
	}

	toolByName(t, set, "forget").Execute(ctx, map[string]any{"query": "q", "space": "projects"})
	if eng.forgotSpace != "projects" {
		t.Errorf("forget space = %q, want projects", eng.forgotSpace)
	}
}

func TestRecallToolShowsTheMemorySpace(t *testing.T) {
	eng := &scopeEngine{}
	set := NewTools(eng, testPolicy())
	res := toolByName(t, set, "recall").Execute(context.Background(), map[string]any{"query": "jobs"})
	if res.IsError || !strings.Contains(res.ForLLM, "system") {
		t.Errorf("recall output missing the memory's space: %+v", res)
	}
}

func TestCapabilityFollowsTheLatestStatus(t *testing.T) {
	srv, cap := newSpaceCaptureServer(t, statusWithSpaces)
	c := NewClient(srv.URL, "", "")
	ctx := context.Background()
	if err := c.CheckHealth(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remember(ctx, RememberRequest{Content: "x", Space: "system"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cap.body("/remember")["space"]; !ok {
		t.Fatal("space not sent to a capable engine")
	}

	// The sidecar can be replaced by an older engine between probes; the next
	// status must withdraw the capability.
	cap.setStatus(statusWithoutSpaces)
	if err := c.CheckHealth(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remember(ctx, RememberRequest{Content: "y", Space: "system"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cap.body("/remember")["space"]; ok {
		t.Error("space still sent after the engine stopped advertising spaces")
	}
}
