package memory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// mergeEngine is a scopeEngine that can also bridge, and records the merge it
// was asked for.
type mergeEngine struct {
	scopeEngine
	mu       sync.Mutex
	merges   [][2]string
	jaccard  float64
	created  int
	mergeErr error
	busy     bool
}

func newMergeEngine() *mergeEngine {
	e := &mergeEngine{created: 3}
	e.spacesOK, e.engineSpace = true, "main"
	return e
}

func (e *mergeEngine) MergeSpaces(_ context.Context, space, other string, minJaccard float64) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.merges = append(e.merges, [2]string{space, other})
	e.jaccard = minJaccard
	return e.created, e.mergeErr
}

func (e *mergeEngine) Idle(time.Duration) bool { return !e.busy }

func (e *mergeEngine) mergeCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.merges)
}

func bridgeAmbient(eng Engine) *Ambient {
	return NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, audiencePolicy())
}

func TestBridgeMergesTheTwoConversationalSpaces(t *testing.T) {
	eng := newMergeEngine()
	bridgeAmbient(eng).bridgeOnce(context.Background())
	if eng.mergeCount() != 1 {
		t.Fatalf("merges = %d, want 1", eng.mergeCount())
	}
	if got := eng.merges[0]; got != [2]string{"main", "shared"} {
		t.Errorf("merged %v, want main × shared", got)
	}
	if eng.jaccard != bridgeMinJaccard {
		t.Errorf("min_jaccard = %v, want %v", eng.jaccard, bridgeMinJaccard)
	}
}

// A bridge is never worth making a turn wait, and every skip below is an
// ordinary state rather than a failure.
func TestBridgeSkipsWhenItShould(t *testing.T) {
	busy := newMergeEngine()
	busy.busy = true
	bridgeAmbient(busy).bridgeOnce(context.Background())
	if busy.mergeCount() != 0 {
		t.Error("merged while the engine was mid-conversation")
	}

	old := newMergeEngine()
	old.spacesOK = false
	bridgeAmbient(old).bridgeOnce(context.Background())
	if old.mergeCount() != 0 {
		t.Error("merged against an engine that does not route spaces")
	}

	skewed := newMergeEngine()
	skewed.engineSpace = "default"
	bridgeAmbient(skewed).bridgeOnce(context.Background())
	if skewed.mergeCount() != 0 {
		t.Error("merged against an engine writing to another space")
	}
}

// A merge that fails is logged and forgotten: the next tick tries again.
func TestBridgeSurvivesAFailedMerge(t *testing.T) {
	eng := newMergeEngine()
	eng.mergeErr = errors.New("engine restarting")
	bridgeAmbient(eng).bridgeOnce(context.Background())
	eng.mergeErr = nil
	bridgeAmbient(eng).bridgeOnce(context.Background())
	if eng.mergeCount() != 2 {
		t.Errorf("merges = %d, want the retry to have happened", eng.mergeCount())
	}
}

// canBridge is the guard that lets the caller start the watcher blind.
func TestCanBridgeNeedsBothSpacesAndAnEngineThatMerges(t *testing.T) {
	if !bridgeAmbient(newMergeEngine()).canBridge() {
		t.Error("a merging engine with both spaces cannot bridge")
	}
	// An engine with no merge route: the partition simply stays unbridged.
	if NewAmbient(newScopeEngine(), 5, 0.1, 5, 500, 500, nil, audiencePolicy()).canBridge() {
		t.Error("an engine without the merge route claimed it could bridge")
	}
	// No shared space: nothing was partitioned, so nothing needs joining.
	if NewAmbient(newMergeEngine(), 5, 0.1, 5, 500, 500, nil, testPolicy()).canBridge() {
		t.Error("bridged without a shared space")
	}
	var nilAmbient *Ambient
	if nilAmbient.canBridge() {
		t.Error("a nil Ambient claimed it could bridge")
	}
	if (&Ambient{Spaces: audiencePolicy()}).canBridge() {
		t.Error("an engine-less Ambient claimed it could bridge")
	}
}

func TestWatchBridgesTicksUntilTheContextEnds(t *testing.T) {
	eng := newMergeEngine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); bridgeAmbient(eng).WatchBridges(ctx, time.Millisecond) }()

	deadline := time.After(5 * time.Second)
	for eng.mergeCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("the watcher never merged")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher outlived its context")
	}
}

// Nothing to bridge must return rather than spin a ticker forever.
func TestWatchBridgesReturnsWhenThereIsNothingToDo(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewAmbient(newScopeEngine(), 5, 0.1, 5, 500, 500, nil, testPolicy()).
			WatchBridges(context.Background(), time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchBridges blocked with nothing to bridge")
	}
}

func TestMergeSpacesOverREST(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/status":
			_, _ = w.Write([]byte(`{"total_atoms":1,"spaces":["main"],"space":"main"}`))
		case "/space_merge":
			_ = json.NewDecoder(r.Body).Decode(&got)
			_, _ = w.Write([]byte(`{"status":"ok","bridges_created":7,"bridge_space":"main_x_shared"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "")
	// Before the first status probe the engine has not advertised routing,
	// so a merge must not be attempted at all.
	if _, err := c.MergeSpaces(context.Background(), "main", "shared", 0.1); err == nil {
		t.Error("merged against an engine that never advertised spaces")
	}
	if err := c.CheckHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	created, err := c.MergeSpaces(context.Background(), "main", "shared", 0.25)
	if err != nil {
		t.Fatal(err)
	}
	if created != 7 {
		t.Errorf("created = %d, want 7", created)
	}
	if got["space"] != "main" || got["other_space"] != "shared" || got["min_jaccard"] != 0.25 {
		t.Errorf("request body = %+v", got)
	}
}

func TestMergeSpacesReportsATransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			_, _ = w.Write([]byte(`{"spaces":["main"],"space":"main"}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "", "")
	_ = c.CheckHealth(context.Background())
	if _, err := c.MergeSpaces(context.Background(), "main", "shared", 0.1); err == nil {
		t.Error("a refused merge was reported as success")
	}
}
