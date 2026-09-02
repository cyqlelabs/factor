package bands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/trace"
)

// writeTraces lays down records at given ages, so a test can build a baseline
// and a recent window without waiting for either.
func writeTraces(t *testing.T, dir string, recs []trace.Record) {
	t.Helper()
	files := map[string][]byte{}
	for _, r := range recs {
		day := r.Started.Format("2006-01-02")
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		files[day] = append(files[day], append(raw, '\n')...)
	}
	for day, body := range files {
		if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func testWatcher(dir string, now time.Time) *Watcher {
	w := New(dir)
	w.now = func() time.Time { return now }
	return w
}

func breachFor(bs []Breach, metric string) (Breach, bool) {
	for _, b := range bs {
		if b.Metric == metric {
			return b, true
		}
	}
	return Breach{}, false
}

// The baseline is what a normal hour looks like; a run of failures against it
// is the whole point of the exercise.
func TestRisingToolErrorsBreach(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var recs []trace.Record
	for i := 0; i < 40; i++ {
		// Baseline: a little noise, mostly clean.
		errs := 0
		if i%8 == 0 {
			errs = 1
		}
		recs = append(recs, record(now.Add(-time.Duration(24+i)*time.Hour), 4, errs))
	}
	for i := 0; i < 6; i++ {
		recs = append(recs, record(now.Add(-time.Duration(i)*time.Minute), 4, 4))
	}
	writeTraces(t, dir, recs)

	got := testWatcher(dir, now).Check()
	b, ok := breachFor(got, "tool error rate")
	if !ok {
		t.Fatalf("no breach reported: %+v", got)
	}
	if b.Sigma < 1 {
		t.Errorf("sigma = %v", b.Sigma)
	}
	if b.Recent <= b.Baseline {
		t.Errorf("recent %v should be above baseline %v", b.Recent, b.Baseline)
	}
}

// A metric moving the harmless way is not news. Errors falling is the system
// getting better, and a watcher that reported it would be a watcher nobody
// reads.
func TestFallingErrorsAreNotABreach(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var recs []trace.Record
	for i := 0; i < 40; i++ {
		recs = append(recs, record(now.Add(-time.Duration(24+i)*time.Hour), 4, 2))
	}
	for i := 0; i < 6; i++ {
		recs = append(recs, record(now.Add(-time.Duration(i)*time.Minute), 4, 0))
	}
	writeTraces(t, dir, recs)

	if _, ok := breachFor(testWatcher(dir, now).Check(), "tool error rate"); ok {
		t.Error("fewer errors reported as a breach")
	}
}

// The cache hit rate is the one metric where a fall is the problem: it is the
// only signal that the request prefix stopped being byte-stable.
func TestFallingCacheHitRateBreaches(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var recs []trace.Record
	for i := 0; i < 40; i++ {
		cached := 800
		if i%5 == 0 {
			cached = 700
		}
		recs = append(recs, cacheRecord(now.Add(-time.Duration(24+i)*time.Hour), 1000, cached))
	}
	for i := 0; i < 6; i++ {
		recs = append(recs, cacheRecord(now.Add(-time.Duration(i)*time.Minute), 1000, 0))
	}
	writeTraces(t, dir, recs)

	b, ok := breachFor(testWatcher(dir, now).Check(), "cache hit rate")
	if !ok {
		t.Fatal("a collapsed cache hit rate went unnoticed")
	}
	if b.Recent >= b.Baseline {
		t.Errorf("recent %v should be below baseline %v", b.Recent, b.Baseline)
	}
}

// Two turns are not a baseline. A band that fires on them is one nobody
// trusts the third time.
func TestTooFewSamplesSaysNothing(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recs := []trace.Record{
		record(now.Add(-30*time.Hour), 4, 0),
		record(now.Add(-time.Minute), 4, 4),
	}
	writeTraces(t, dir, recs)
	if got := testWatcher(dir, now).Check(); len(got) != 0 {
		t.Errorf("reported %+v from two turns", got)
	}
}

// A metric that has never moved has no band to leave, and dividing by its
// zero deviation would report everything as infinitely wrong.
func TestNoVarianceInTheBaselineSaysNothing(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var recs []trace.Record
	for i := 0; i < 30; i++ {
		recs = append(recs, record(now.Add(-time.Duration(24+i)*time.Hour), 4, 0))
	}
	for i := 0; i < 5; i++ {
		recs = append(recs, record(now.Add(-time.Duration(i)*time.Minute), 4, 0))
	}
	writeTraces(t, dir, recs)

	for _, b := range testWatcher(dir, now).Check() {
		if b.Sigma > 1e6 {
			t.Errorf("a flat baseline produced %v", b)
		}
	}
}

func TestNoTracesIsNotAnError(t *testing.T) {
	if got := New(t.TempDir()).Check(); len(got) != 0 {
		t.Errorf("got %+v from an empty directory", got)
	}
	if got := New("").Check(); got != nil {
		t.Errorf("got %+v from no directory at all", got)
	}
	var nilWatcher *Watcher
	if got := nilWatcher.Check(); got != nil {
		t.Errorf("got %+v from a nil watcher", got)
	}
}

func TestTierCapsAtThree(t *testing.T) {
	for _, tc := range []struct {
		sigma float64
		want  int
	}{{1.2, 1}, {2.7, 2}, {3.4, 3}, {40, 3}} {
		if got := (Breach{Sigma: tc.sigma}).Tier(); got != tc.want {
			t.Errorf("Tier(%v) = %d, want %d", tc.sigma, got, tc.want)
		}
	}
}

func TestLineReadsAsASentence(t *testing.T) {
	b := Breach{Metric: "cost per turn", Unit: "USD", Recent: 0.5, Baseline: 0.05, Sigma: 4.2, Samples: 7}
	got := b.Line()
	for _, want := range []string{"cost per turn", "$0.5000", "$0.0500", "4.2σ", "7 turns"} {
		if !contains(got, want) {
			t.Errorf("Line() = %q, missing %q", got, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// A turn that called no tools says nothing about the tool error rate;
// averaging a zero in would drag the baseline toward whatever quiet turns did.
func TestSpecsSkipTurnsThatCarryNoReading(t *testing.T) {
	for _, spec := range Specs() {
		if _, ok := spec.Of(trace.Record{}); ok {
			switch spec.Name {
			case "turn failures", "provider failovers", "context overflows":
				// These are true of every turn: a turn that failed over zero
				// times is a real reading of zero.
			default:
				t.Errorf("%q read a value off an empty turn", spec.Name)
			}
		}
	}
}

func record(at time.Time, tools, errs int) trace.Record {
	r := trace.Record{Started: at, Session: "cli:x", Outcome: "ok", Duration: 1, USD: 0.01,
		InputTokens: 100, CachedTokens: 50}
	for i := 0; i < tools; i++ {
		r.Tools = append(r.Tools, trace.ToolCall{Name: "read_file", Error: i < errs})
	}
	return r
}

func cacheRecord(at time.Time, input, cached int) trace.Record {
	return trace.Record{Started: at, Session: "cli:x", Outcome: "ok", Duration: 1,
		InputTokens: input, CachedTokens: cached}
}

func TestFormatRendersEachUnit(t *testing.T) {
	for _, tc := range []struct {
		v          float64
		unit, want string
	}{
		{0.5, "USD", "$0.5000"},
		{0.42, "of calls", "42%"},
		{0.42, "of turns", "42%"},
		{0.42, "of input", "42%"},
		{3.25, "s", "3.2s"},
		{2, "per turn", "2.00 per turn"},
	} {
		if got := format(tc.v, tc.unit); got != tc.want {
			t.Errorf("format(%v, %q) = %q, want %q", tc.v, tc.unit, got, tc.want)
		}
	}
}

func TestStatsOnNothing(t *testing.T) {
	mean, sd := stats(nil)
	if mean != 0 || sd != 0 {
		t.Errorf("stats(nil) = %v, %v", mean, sd)
	}
}

// A recent window with no readings of a metric says nothing about it.
func TestNoRecentReadingsSaysNothing(t *testing.T) {
	spec := Specs()[0]
	base := make([]float64, 20)
	for i := range base {
		base[i] = float64(i % 3)
	}
	if _, ok := judge(spec, base, nil, 10); ok {
		t.Error("judged a metric with nothing recent to judge")
	}
}

// Cost is skipped on unpriced models: a zero there is "nobody knows", not
// "it was free", and averaging it in would drag the baseline down.
func TestUnpricedTurnsAreNotCountedAsFree(t *testing.T) {
	var costSpec Spec
	for _, s := range Specs() {
		if s.Name == "cost per turn" {
			costSpec = s
		}
	}
	if _, ok := costSpec.Of(trace.Record{USD: 0}); ok {
		t.Error("an unpriced turn was read as a zero-cost one")
	}
	if v, ok := costSpec.Of(trace.Record{USD: 0.5}); !ok || v != 0.5 {
		t.Errorf("a priced turn read as %v, %v", v, ok)
	}
}

// The seconds-per-turn spec skips a turn with no measured duration.
func TestDurationSpecSkipsUnmeasuredTurns(t *testing.T) {
	for _, s := range Specs() {
		if s.Name != "seconds per turn" {
			continue
		}
		if _, ok := s.Of(trace.Record{Duration: 0}); ok {
			t.Error("a turn with no duration was read as instantaneous")
		}
	}
}

// A breach in the tool error rate carries the calls that failed, with what
// they said, and each failing tool's record across the whole baseline. The
// second is what tells an hour's bad luck from a feature that has never
// worked on this machine.
func TestToolBreachCarriesItsEvidence(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var recs []trace.Record
	for i := 0; i < 40; i++ {
		recs = append(recs, record(now.Add(-time.Duration(24+i)*time.Hour), 4, 0))
	}
	// The fast path has failed every time it ran, three days running.
	for i := 1; i <= 3; i++ {
		recs = append(recs, trace.Record{Started: now.Add(-time.Duration(i*24) * time.Hour), Session: "voice:local", Outcome: "ok",
			Tools: []trace.ToolCall{{Name: "browser_fetch", Error: true, Fault: "lightpanda exited"}}})
	}
	// This hour: a search and the fast path both failed, and the loop ran
	// on with Chrome.
	recs = append(recs, trace.Record{Started: now.Add(-10 * time.Minute), Session: "voice:local", Outcome: "ok",
		Tools: []trace.ToolCall{
			{Name: "web_search", Error: true, Fault: "search failed: 403 from the engine"},
			{Name: "browser_fetch", Error: true, Fault: "lightpanda exited"},
			{Name: "browser_navigate"},
		}})
	writeTraces(t, dir, recs)

	b, ok := breachFor(testWatcher(dir, now).Check(), "tool error rate")
	if !ok {
		t.Fatal("no breach")
	}
	e := b.Evidence
	if len(e.Failures) != 2 || e.Failures[0].Tool != "web_search" || e.Failures[0].Fault != "search failed: 403 from the engine" {
		t.Errorf("failures = %+v, want this hour's two with what they said", e.Failures)
	}
	if len(e.History) != 2 || e.History[0].Tool != "browser_fetch" {
		t.Fatalf("history = %+v, want browser_fetch first as the worst", e.History)
	}
	if h := e.History[0]; h.Calls != 4 || h.Fails != 4 || !h.First.Before(now.Add(-48*time.Hour)) {
		t.Errorf("browser_fetch record = %+v, want every call a failure, the first days ago", h)
	}
	if h := e.History[1]; h.Tool != "web_search" || h.Calls != 1 || h.Fails != 1 {
		t.Errorf("web_search record = %+v", h)
	}
	if e.Since.IsZero() || !e.Since.Before(now.Add(-6*24*time.Hour)) {
		t.Errorf("evidence is not dated from the baseline: %v", e.Since)
	}

	lines := b.Details()
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"web_search failed: search failed: 403", "browser_fetch has failed 4 of 4 calls since"} {
		if !strings.Contains(joined, want) {
			t.Errorf("details lack %q:\n%s", want, joined)
		}
	}
}

// A turn that died is named with what it died of; a metric that is only a
// number carries nothing.
func TestTurnFailuresCarryTheirErrors(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var recs []trace.Record
	for i := 0; i < 40; i++ {
		r := record(now.Add(-time.Duration(24+i)*time.Hour), 4, 0)
		if i%10 == 0 {
			r.Outcome = "error"
		}
		recs = append(recs, r)
	}
	for i := 0; i < 3; i++ {
		r := record(now.Add(-time.Duration(i)*time.Minute), 1, 0)
		r.Outcome, r.Error = "error", "provider: 529 overloaded"
		r.USD = 1 // spend spikes with them, and spend is only a number
		recs = append(recs, r)
	}
	writeTraces(t, dir, recs)
	got := testWatcher(dir, now).Check()

	b, ok := breachFor(got, "turn failures")
	if !ok {
		t.Fatal("no breach")
	}
	if len(b.Evidence.Turns) != 3 || b.Evidence.Turns[0].Error != "provider: 529 overloaded" {
		t.Errorf("turns = %+v", b.Evidence.Turns)
	}
	if !strings.Contains(strings.Join(b.Details(), "\n"), "turn failed: provider: 529 overloaded") {
		t.Errorf("details = %v", b.Details())
	}
	if cost, ok := breachFor(got, "cost per turn"); ok && len(cost.Details()) != 0 {
		t.Errorf("a number-only metric carried evidence: %v", cost.Details())
	}
}

// More failures than a prompt should carry keep the newest, which are the
// ones still true.
func TestEvidenceIsBounded(t *testing.T) {
	now := time.Now()
	var recs []trace.Record
	for i := 20; i > 0; i-- {
		recs = append(recs, trace.Record{Started: now.Add(-time.Duration(i) * time.Minute), Session: "cli:x", Outcome: "error", Error: "e",
			Tools: []trace.ToolCall{{Name: "exec", Error: true, Fault: fmt.Sprintf("fault %d", i)}}})
	}
	e := toolEvidence(recs, now.Add(-time.Hour))
	if len(e.Failures) != maxFailures || e.Failures[len(e.Failures)-1].Fault != "fault 1" {
		t.Errorf("failures = %d ending %q, want the newest %d", len(e.Failures), e.Failures[len(e.Failures)-1].Fault, maxFailures)
	}
	if turns := turnEvidence(recs, now.Add(-time.Hour)).Turns; len(turns) != maxTurns {
		t.Errorf("turns = %d, want %d", len(turns), maxTurns)
	}
}
