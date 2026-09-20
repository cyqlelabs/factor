package decision

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// countingBackend answers every question with the same choice and counts how
// often it was actually asked.
type countingBackend struct {
	mu    sync.Mutex
	calls int
	err   error
	name  string
	usage Usage
}

func (b *countingBackend) Decide(_ context.Context, req *Request) (*Response, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	answers := map[string]Answer{}
	for name, q := range req.Questions {
		ids := Candidates(q)
		probs := map[string]float64{}
		for _, id := range ids {
			probs[id] = 0.1 / float64(max(len(ids)-1, 1))
		}
		probs[ids[0]] = 0.9
		answers[name] = Answer{Choice: ids[0], Confidence: 0.9, Probabilities: probs}
	}
	return &Response{Answers: answers, Model: "counted", Usage: b.usage}, nil
}

func (b *countingBackend) Name() string {
	if b.name != "" {
		return b.name
	}
	return "counting"
}

func (b *countingBackend) seen() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func request(state string) *Request {
	return &Request{
		State:     map[string]any{"page": state},
		Questions: map[string]Question{"op": {Criteria: map[string]any{"CLICK": "c", "DONE": "d"}}},
	}
}

// The repeats the memo exists for: the same question about a page an action
// did not change is answered without a round trip.
func TestCacheAnswersARepeatedRequestWithoutAsking(t *testing.T) {
	inner := &countingBackend{usage: Usage{InputTokens: 400}}
	c := Cached(inner, 8).(*Cache)

	first, err := c.Decide(context.Background(), request("a"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Decide(context.Background(), request("a"))
	if err != nil {
		t.Fatal(err)
	}
	if inner.seen() != 1 {
		t.Errorf("the backend was asked %d times for one question", inner.seen())
	}
	if second.Answers["op"].Choice != first.Answers["op"].Choice {
		t.Errorf("the memo answered differently: %+v", second.Answers)
	}
	// The tokens were billed once. A hit that reported them again would be
	// spend that never happened.
	if first.Usage.InputTokens != 400 {
		t.Errorf("the first answer lost its usage: %+v", first.Usage)
	}
	if second.Usage != (Usage{}) {
		t.Errorf("a cache hit reported usage: %+v", second.Usage)
	}
	if hits, misses := c.Stats(); hits != 1 || misses != 1 {
		t.Errorf("hits=%d misses=%d", hits, misses)
	}

	// A different state is a different question.
	if _, err := c.Decide(context.Background(), request("b")); err != nil {
		t.Fatal(err)
	}
	if inner.seen() != 2 {
		t.Errorf("a changed state was served from the memo")
	}
}

// Anything that changes what would go on the wire changes the key.
func TestCacheKeysOnEverythingThatTravels(t *testing.T) {
	base := request("a")
	key, ok := fingerprint("b1", base)
	if !ok {
		t.Fatal("a plain request did not fingerprint")
	}
	other, _ := fingerprint("b2", base)
	if key == other {
		t.Error("two backends share a key")
	}

	differs := map[string]*Request{
		"state": request("b"),
		"criteria": {State: base.State,
			Questions: map[string]Question{"op": {Criteria: map[string]any{"CLICK": "c", "DONE": "different"}}}},
		"instructions": {State: base.State,
			Questions: map[string]Question{"op": {Criteria: map[string]any{"CLICK": "c", "DONE": "d"}, Instructions: "rules"}}},
		"question name": {State: base.State,
			Questions: map[string]Question{"other": {Criteria: map[string]any{"CLICK": "c", "DONE": "d"}}}},
		"extra question": {State: base.State, Questions: map[string]Question{
			"op":     {Criteria: map[string]any{"CLICK": "c", "DONE": "d"}},
			"target": {Criteria: map[string]any{"1": "x"}},
		}},
	}
	for what, req := range differs {
		got, _ := fingerprint("b1", req)
		if got == key {
			t.Errorf("a different %s shares a key", what)
		}
	}

	// Map order is not part of the request, so it is not part of the key.
	same := &Request{State: map[string]any{"page": "a"},
		Questions: map[string]Question{"op": {Criteria: map[string]any{"DONE": "d", "CLICK": "c"}}}}
	if got, _ := fingerprint("b1", same); got != key {
		t.Error("map iteration order changed the key")
	}

	// A state that cannot be encoded is not cached rather than given a key
	// that would collide with something else.
	if _, ok := fingerprint("b1", &Request{State: make(chan int)}); ok {
		t.Error("an unencodable request produced a key")
	}
}

func TestCacheEvictsTheOldestAndStaysBounded(t *testing.T) {
	inner := &countingBackend{}
	c := Cached(inner, 2).(*Cache)
	for _, state := range []string{"a", "b", "c"} {
		if _, err := c.Decide(context.Background(), request(state)); err != nil {
			t.Fatal(err)
		}
	}
	if inner.seen() != 3 {
		t.Fatalf("backend calls = %d", inner.seen())
	}
	// "a" was evicted; "c" is still there.
	if _, err := c.Decide(context.Background(), request("a")); err != nil {
		t.Fatal(err)
	}
	if inner.seen() != 4 {
		t.Error("the evicted entry was still served")
	}
	if _, err := c.Decide(context.Background(), request("c")); err != nil {
		t.Fatal(err)
	}
	if inner.seen() != 4 {
		t.Error("a live entry was re-asked")
	}
	if len(c.entries) > 2 {
		t.Errorf("the memo grew past its limit: %d", len(c.entries))
	}
}

// An error is a fact about the backend at a moment, not about the question,
// so it is never remembered.
func TestCacheNeverRemembersAFailure(t *testing.T) {
	inner := &countingBackend{err: errors.New("down")}
	c := Cached(inner, 4)
	for i := 0; i < 3; i++ {
		if _, err := c.Decide(context.Background(), request("a")); err == nil {
			t.Fatal("expected the failure through")
		}
	}
	if inner.seen() != 3 {
		t.Errorf("a failure was cached: %d calls", inner.seen())
	}
}

func TestCachedIsOffAtZeroAndPassesThroughWhatItWraps(t *testing.T) {
	inner := &countingBackend{name: "inner"}
	if got := Cached(inner, 0); got != Backend(inner) {
		t.Error("a zero limit should leave the backend unwrapped")
	}
	if got := Cached(nil, 8); got != nil {
		t.Error("nothing to wrap should stay nothing")
	}
	c := Cached(inner, 4)
	if c.Name() != "inner" {
		t.Errorf("the cache renamed the backend: %q", c.Name())
	}
	// A cache in front of a limited backend must not hide its window.
	limited := Cached(&limitedBackend{countingBackend: countingBackend{}, limits: Limits{MaxCandidates: 12}}, 4)
	if l := limited.(Limited).Limits(); l.MaxCandidates != 12 {
		t.Errorf("limits through the cache = %+v", l)
	}
	if l := c.(Limited).Limits(); l != (Limits{}) {
		t.Errorf("an unlimited backend reported limits through the cache: %+v", l)
	}
}

func TestCacheIsSafeUnderConcurrentUse(t *testing.T) {
	inner := &countingBackend{}
	c := Cached(inner, 16)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.Decide(context.Background(), request(fmt.Sprint(i%4))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if inner.seen() > 20 {
		t.Errorf("more calls than requests: %d", inner.seen())
	}
}

// limitedBackend is a backend that declares a window.
type limitedBackend struct {
	countingBackend
	limits Limits
}

func (b *limitedBackend) Limits() Limits { return b.limits }

// The decider reports what is behind it, so a caller can size a request
// instead of finding out from a refusal.
func TestDeciderReportsTheBackendsLimits(t *testing.T) {
	plain := New(&countingBackend{}, Options{Mode: ModeActive})
	if l := plain.Limits(); l != (Limits{}) {
		t.Errorf("an undeclared backend reported limits: %+v", l)
	}
	if plain.Backend() != "counting" {
		t.Errorf("Backend() = %q", plain.Backend())
	}
	bounded := New(&limitedBackend{limits: Limits{MaxCandidates: 12, MaxStateChars: 2000}}, Options{Mode: ModeActive})
	if l := bounded.Limits(); l.MaxCandidates != 12 || l.MaxStateChars != 2000 {
		t.Errorf("limits = %+v", l)
	}
	var none *Decider
	if none.Limits() != (Limits{}) || none.Backend() != "" {
		t.Error("a nil decider must report nothing")
	}
}

func TestLimitsBoundCandidatesAndState(t *testing.T) {
	none := Limits{}
	if none.Candidates(50) != 50 {
		t.Error("an unknown limit must not bound anything")
	}
	if got := none.ClipState("hello", 0); got != "hello" {
		t.Errorf("ClipState = %q", got)
	}
	l := Limits{MaxCandidates: 12, MaxStateChars: 10}
	if l.Candidates(50) != 12 || l.Candidates(4) != 4 {
		t.Errorf("Candidates: %d %d", l.Candidates(50), l.Candidates(4))
	}
	// The tighter of the two budgets wins, whichever it is.
	if got := l.ClipState("0123456789abcdef", 100); got != "0123456789…" {
		t.Errorf("model budget did not apply: %q", got)
	}
	if got := l.ClipState("0123456789abcdef", 4); got != "0123…" {
		t.Errorf("caller budget did not apply: %q", got)
	}
	if got := (Limits{}).ClipState("0123456789abcdef", 4); got != "0123…" {
		t.Errorf("caller budget alone did not apply: %q", got)
	}
}
