package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
	"github.com/cyqlelabs/factor/internal/trace"
)

func TestVersionAtLeast(t *testing.T) {
	for v, want := range map[string]bool{
		"0.21.4": true, "v0.21.4": true, "0.21.5": true, "0.22.0": true, "1.0.0": true,
		"0.21.4-rc1": true, "0.21.3": false, "0.19.1": false, "0.9.99": false,
		"": false, "dev": false, "0.21": false, "0.x.4": false, "0.-1.4": false,
	} {
		if got := versionAtLeast(v, quietRecallSince); got != want {
			t.Errorf("versionAtLeast(%q) = %v, want %v", v, got, want)
		}
	}
}

// attendSmrti answers /status with a version and records every /recall and
// /attend body it is sent.
func attendSmrti(t *testing.T, version *string) (*httptest.Server, func(string) []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	got := map[string][]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got[r.URL.Path] = append(got[r.URL.Path], body)
		v := *version
		mu.Unlock()
		switch r.URL.Path {
		case "/status":
			fmt.Fprintf(w, `{"spaces":["main"],"space":"main","version":%q}`, v)
		case "/recall":
			_, _ = w.Write([]byte(`{"memories":[{"id":"m1","content":"a fact"}]}`))
		default:
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func(path string) []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got[path])
	}
}

// The engine's version is read on every status probe, so a newer engine
// turns the quiet recall on and an older one swapped in under it turns it off.
func TestAttendsFollowsTheEngineVersion(t *testing.T) {
	version := ""
	srv, _ := attendSmrti(t, &version)
	c := NewClient(srv.URL, "", "")
	if c.Attends() {
		t.Fatal("attends before any probe")
	}
	for _, step := range []struct {
		version string
		want    bool
	}{{"0.21.4", true}, {"0.21.3", false}, {"0.22.4", true}, {"", false}} {
		version = step.version
		if _, err := c.Status(context.Background()); err != nil {
			t.Fatal(err)
		}
		if c.Attends() != step.want {
			t.Errorf("engine %q: Attends() = %v, want %v", step.version, c.Attends(), step.want)
		}
	}
}

// A boosting recall is the payload it always was; only the quiet one says
// boost:false.
func TestRecallQuietTurnsTheBoostOff(t *testing.T) {
	version := "0.22.4"
	srv, sent := attendSmrti(t, &version)
	c := NewClient(srv.URL, "", "")
	if _, err := c.Recall(context.Background(), "q", 5, 0, Scope{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecallQuiet(context.Background(), "q", 5, 0, Scope{}); err != nil {
		t.Fatal(err)
	}
	recalls := sent("/recall")
	if len(recalls) != 2 {
		t.Fatalf("recalls = %v", recalls)
	}
	if _, ok := recalls[0]["boost"]; ok {
		t.Errorf("a boosting recall carried the flag: %v", recalls[0])
	}
	if recalls[1]["boost"] != false {
		t.Errorf("a quiet recall did not turn the boost off: %v", recalls[1])
	}
}

// More ids than the engine takes at once go out in batches, each naming the
// write space once the engine routes spaces.
func TestAttendBatchesAndNamesTheSpace(t *testing.T) {
	version := "0.22.4"
	srv, sent := attendSmrti(t, &version)
	c := NewClient(srv.URL, "", "")
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, attendBatch+3)
	for i := range ids {
		ids[i] = fmt.Sprintf("a%d", i)
	}
	if err := c.Attend(context.Background(), ids, "main"); err != nil {
		t.Fatal(err)
	}
	attends := sent("/attend")
	if len(attends) != 2 {
		t.Fatalf("attends = %d, want 2", len(attends))
	}
	if n := len(attends[0]["atom_ids"].([]any)); n != attendBatch {
		t.Errorf("first batch = %d ids", n)
	}
	if n := len(attends[1]["atom_ids"].([]any)); n != 3 {
		t.Errorf("second batch = %d ids", n)
	}
	if attends[0]["space"] != "main" || attends[1]["space"] != "main" {
		t.Errorf("space missing: %v", attends)
	}
}

func TestAttendStopsOnTheFirstFailure(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "", "")
	if err := c.Attend(context.Background(), []string{"a"}, ""); err == nil {
		t.Fatal("an unreachable engine attended")
	}
}

// attendEngine is an Attender that records how it was used.
type attendEngine struct {
	stubEngine
	attends  bool
	mems     []Memory
	mu       sync.Mutex
	loud     int
	quiet    int
	attended chan attendCall
}

type attendCall struct {
	ids   []string
	space string
}

func newAttendEngine(attends bool, mems ...Memory) *attendEngine {
	return &attendEngine{attends: attends, mems: mems, attended: make(chan attendCall, 4)}
}

func (e *attendEngine) Healthy() bool { return true }
func (e *attendEngine) Enabled() bool { return true }
func (e *attendEngine) Attends() bool { return e.attends }
func (e *attendEngine) Recall(context.Context, string, int, float64, Scope) ([]Memory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.loud++
	return e.mems, nil
}
func (e *attendEngine) RecallQuiet(context.Context, string, int, float64, Scope) ([]Memory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.quiet++
	return e.mems, nil
}
func (e *attendEngine) Attend(_ context.Context, ids []string, space string) error {
	e.attended <- attendCall{ids: ids, space: space}
	return nil
}

func (e *attendEngine) counts() (loud, quiet int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.loud, e.quiet
}

var shownMems = []Memory{
	{ID: "m1", Content: "the fact the turn needed", Confidence: 0.9},
	{ID: "label", Content: "  "},
	{ID: "m2", Content: "another fact", Confidence: 0.8},
}

// Both recalls leave no trace, and what the turn was shown is boosted once:
// a duplicate both queries found and a bare label the prompt drops are not.
func TestMemoryPromptBoostsOnlyWhatTheTurnShowed(t *testing.T) {
	eng := newAttendEngine(true, shownMems...)
	a := NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, SpacePolicy{})
	dir := t.TempDir()
	rec := trace.NewRecorder(dir, trace.Config{Enabled: true, KeepDays: 14}, nil)
	defer func() { _ = rec.Close() }()
	turn := rec.Begin("cli:x", "user", "")
	ctx := trace.WithTurn(tools.WithToolContext(context.Background(), tools.ToolContext{Channel: "cli"}), turn)

	out := a.MemoryPrompt(ctx, []provider.Message{{Role: "user", Content: "earlier"}}, "now")
	if !strings.Contains(out, "the fact the turn needed") {
		t.Fatalf("prompt = %q", out)
	}
	select {
	case call := <-eng.attended:
		if !slices.Equal(call.ids, []string{"m1", "m2"}) {
			t.Errorf("attended %v, want [m1 m2]", call.ids)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was attended")
	}
	if loud, quiet := eng.counts(); loud != 0 || quiet != 2 {
		t.Errorf("recalls: %d boosting, %d quiet; want 0 and 2", loud, quiet)
	}
	turn.End("ok", nil)
	recs, err := trace.Since(dir, time.Now().Add(-time.Minute))
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %v, %v", recs, err)
	}
	var detail string
	for _, ev := range recs[0].Events {
		if ev.Kind == trace.EventMemoryShown {
			detail = ev.Detail
		}
	}
	if detail != "m1,m2" {
		t.Errorf("shown event = %q, want m1,m2", detail)
	}
}

// A heartbeat reads quietly and boosts nothing.
func TestHeartbeatRecallBoostsNothing(t *testing.T) {
	eng := newAttendEngine(true, shownMems...)
	a := NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, SpacePolicy{})
	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{Channel: "system"})
	a.MemoryPrompt(ctx, nil, "check the bands")
	select {
	case call := <-eng.attended:
		t.Fatalf("a heartbeat attended %v", call.ids)
	case <-time.After(100 * time.Millisecond):
	}
	if loud, quiet := eng.counts(); loud != 0 || quiet != 1 {
		t.Errorf("recalls: %d boosting, %d quiet; want 0 and 1", loud, quiet)
	}
}

// An engine too old for the quiet recall keeps the recall it always had.
func TestAnOldEngineKeepsTheBoostingRecall(t *testing.T) {
	eng := newAttendEngine(false, shownMems...)
	a := NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, SpacePolicy{})
	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{Channel: "cli"})
	a.MemoryPrompt(ctx, nil, "now")
	select {
	case call := <-eng.attended:
		t.Fatalf("an old engine was attended %v", call.ids)
	case <-time.After(100 * time.Millisecond):
	}
	if loud, quiet := eng.counts(); loud != 1 || quiet != 0 {
		t.Errorf("recalls: %d boosting, %d quiet; want 1 and 0", loud, quiet)
	}
}

// failingAttender logs and moves on.
type failingAttender struct{ attendEngine }

func (*failingAttender) Attend(context.Context, []string, string) error {
	return fmt.Errorf("engine went away")
}

func TestAttendFailureIsOnlyALogLine(t *testing.T) {
	a := &Ambient{}
	a.attend(&failingAttender{}, []string{"m1"}, "main")
}

// The sidecar hands the quiet recall through to its client.
func TestSidecarForwardsTheQuietRecall(t *testing.T) {
	version := "0.22.4"
	srv, sent := attendSmrti(t, &version)
	s := &Sidecar{client: NewClient(srv.URL, "", "")}
	if _, err := s.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	var _ Attender = s
	if !s.Attends() {
		t.Fatal("sidecar does not attend")
	}
	if _, err := s.RecallQuiet(context.Background(), "q", 5, 0, Scope{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Attend(context.Background(), []string{"m1"}, ""); err != nil {
		t.Fatal(err)
	}
	if len(sent("/recall")) != 1 || len(sent("/attend")) != 1 {
		t.Errorf("recall %d, attend %d", len(sent("/recall")), len(sent("/attend")))
	}
}
