package trace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestRecorder(t *testing.T, cfg Config) (*Recorder, string) {
	t.Helper()
	dir := t.TempDir()
	if cfg.KeepDays == 0 {
		cfg.KeepDays = 14
	}
	cfg.Enabled = true
	r := NewRecorder(dir, cfg, nil)
	if r == nil {
		t.Fatal("recorder should be enabled")
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, dir
}

// Tracing off is a nil recorder, and every call on it has to be safe: the loop
// calls them unconditionally rather than guarding each one.
func TestDisabledRecorderIsNilAndSafe(t *testing.T) {
	r := NewRecorder(t.TempDir(), Config{Enabled: false}, nil)
	if r != nil {
		t.Fatal("tracing off should produce no recorder")
	}
	turn := r.Begin("cli:x", "user", "")
	turn.Tool("read_file", map[string]any{"path": "/tmp/x"}, time.Second, 10, false)
	turn.Event(EventSteering, "")
	turn.End("ok", nil)
	r.Charge("cli:x", ModelCall{Model: "m", Input: 1})
	r.Event("cli:x", EventFailover, "x")
	if got := r.Dir(); got != "" {
		t.Errorf("Dir() = %q on a nil recorder", got)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
}

func TestTurnRecordsTheTrajectory(t *testing.T) {
	r, dir := newTestRecorder(t, Config{})

	turn := r.Begin("telegram:42", "user", "roxana")
	turn.Tool("read_file", map[string]any{"path": "/etc/hosts"}, 20*time.Millisecond, 120, false)
	turn.Tool("web_fetch", map[string]any{"url": "https://example.test"}, 90*time.Millisecond, 0, true)
	turn.Event(EventSteering, "")
	r.Charge("telegram:42", ModelCall{Model: "big", Input: 1000, Cached: 800, Output: 50, USD: 0.02})
	turn.End("ok", nil)

	recs, err := Since(dir, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("read %d records, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Session != "telegram:42" || rec.Channel != "telegram" || rec.Speaker != "roxana" {
		t.Errorf("record identity = %+v", rec)
	}
	if len(rec.Tools) != 2 || rec.Tools[0].Name != "read_file" {
		t.Errorf("tools = %+v", rec.Tools)
	}
	if rec.ToolErrors() != 1 {
		t.Errorf("ToolErrors() = %d, want 1", rec.ToolErrors())
	}
	if rec.Count(EventSteering) != 1 {
		t.Errorf("steering events = %d", rec.Count(EventSteering))
	}
	if rec.InputTokens != 1000 || rec.CachedTokens != 800 || rec.OutputTokens != 50 {
		t.Errorf("token totals = %+v", rec)
	}
	if rec.CacheHitRate() != 0.8 {
		t.Errorf("CacheHitRate() = %v", rec.CacheHitRate())
	}
	if rec.Outcome != "ok" || rec.Duration < 0 {
		t.Errorf("outcome = %q duration = %v", rec.Outcome, rec.Duration)
	}
}

// Argument values are the user's paths and searches. The shape of a turn is
// what the trace exists for, so the values stay out unless asked for.
func TestArgumentValuesAreOmittedByDefault(t *testing.T) {
	r, dir := newTestRecorder(t, Config{})
	turn := r.Begin("cli:x", "user", "")
	turn.Tool("remember", map[string]any{"content": "the alarm code is 1234"}, time.Millisecond, 2, false)
	turn.End("ok", nil)

	recs, _ := Since(dir, time.Now().Add(-time.Minute))
	call := recs[0].Tools[0]
	if call.Args != "" {
		t.Errorf("argument values were recorded: %q", call.Args)
	}
	if len(call.ArgKeys) != 1 || call.ArgKeys[0] != "content" {
		t.Errorf("arg keys = %v, want the names without the values", call.ArgKeys)
	}
}

func TestRecordArgsPassesThroughTheSecretFilter(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir, Config{Enabled: true, RecordArgs: true, KeepDays: 5},
		func(s string) string { return strings.ReplaceAll(s, "hunter2", "***") })
	defer r.Close()

	turn := r.Begin("cli:x", "user", "")
	turn.Tool("exec", map[string]any{"command": "login --password hunter2"}, time.Millisecond, 2, false)
	turn.End("ok", nil)

	recs, _ := Since(dir, time.Now().Add(-time.Minute))
	if got := recs[0].Tools[0].Args; !strings.Contains(got, "***") || strings.Contains(got, "hunter2") {
		t.Errorf("args = %q, want the secret scrubbed", got)
	}
}

func TestLongArgumentsAreBounded(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir, Config{Enabled: true, RecordArgs: true}, nil)
	defer r.Close()
	turn := r.Begin("cli:x", "user", "")
	turn.Tool("write_file", map[string]any{"content": strings.Repeat("x", 5000)}, time.Millisecond, 2, false)
	turn.End("ok", nil)

	recs, _ := Since(dir, time.Now().Add(-time.Minute))
	if got := len(recs[0].Tools[0].Args); got > maxArgChars+4 {
		t.Errorf("recorded %d chars of arguments, want it bounded near %d", got, maxArgChars)
	}
}

// Spend with no turn open — an idle compaction, an induction verdict — is
// exactly the spend worth being able to find, so it gets a record of its own.
func TestHousekeepingSpendIsRecordedWithoutATurn(t *testing.T) {
	r, dir := newTestRecorder(t, Config{})
	r.Charge("cli:quiet", ModelCall{Model: "small", Input: 900, Output: 40, USD: 0.001})

	recs, _ := Since(dir, time.Now().Add(-time.Minute))
	if len(recs) != 1 {
		t.Fatalf("read %d records, want 1", len(recs))
	}
	if recs[0].Trigger != "housekeeping" || recs[0].USD == 0 {
		t.Errorf("record = %+v", recs[0])
	}
}

func TestEventBySessionKeyReachesTheOpenTurn(t *testing.T) {
	r, dir := newTestRecorder(t, Config{})
	turn := r.Begin("voice:local", "user", "")
	r.Event("voice:local", EventFailover, "primary: overloaded")
	r.Event("voice:nobody-here", EventFailover, "dropped")
	turn.End("ok", nil)

	recs, _ := Since(dir, time.Now().Add(-time.Minute))
	if recs[0].Count(EventFailover) != 1 {
		t.Errorf("events = %+v", recs[0].Events)
	}
}

func TestEndRecordsTheError(t *testing.T) {
	r, dir := newTestRecorder(t, Config{})
	turn := r.Begin("cli:x", "user", "")
	turn.End("error", errors.New("the browser would not start"))

	recs, _ := Since(dir, time.Now().Add(-time.Minute))
	if !recs[0].Failed() || !strings.Contains(recs[0].Error, "browser") {
		t.Errorf("record = %+v", recs[0])
	}
}

// An interruption is the system working — the user talked over the answer —
// so it must not read as something that needs looking at.
func TestInterruptedTurnIsNotAFailure(t *testing.T) {
	rec := Record{Outcome: "interrupted"}
	if rec.Failed() {
		t.Error("an interrupted turn should not count as a failure")
	}
}

// A crash mid-write leaves a torn final line. A watcher that refused to read
// anything because of it would go blind exactly when something went wrong.
func TestTornLineDoesNotStopTheReader(t *testing.T) {
	dir := t.TempDir()
	day := time.Now().Format("2006-01-02")
	body := `{"id":"a","started":"` + time.Now().Format(time.RFC3339Nano) + `","session":"cli:x","outcome":"ok"}` + "\n" +
		`{"id":"b","started":"` + time.Now().Format(time.RFC3339Nano) + `","sess`
	if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := Since(dir, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].ID != "a" {
		t.Errorf("records = %+v", recs)
	}
}

func TestSinceOnAMissingDirectoryIsEmptyNotAnError(t *testing.T) {
	recs, err := Since(filepath.Join(t.TempDir(), "nothing-here"), time.Now())
	if err != nil || len(recs) != 0 {
		t.Errorf("recs = %v err = %v", recs, err)
	}
}

func TestPruneDropsTheOldestDays(t *testing.T) {
	dir := t.TempDir()
	for _, day := range []string{"2020-01-01", "2020-01-02", "2020-01-03", "2020-01-04"} {
		if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := NewRecorder(dir, Config{Enabled: true, KeepDays: 2}, nil)
	defer r.Close()

	left, _ := os.ReadDir(dir)
	if len(left) != 2 {
		t.Fatalf("kept %d files, want 2", len(left))
	}
	if left[0].Name() != "2020-01-03.jsonl" {
		t.Errorf("pruned the wrong end: %v", left[0].Name())
	}
}

// Turns run concurrently, so the recorder is written to from several
// goroutines at once and must not interleave or lose a record.
func TestConcurrentTurnsEachGetARecord(t *testing.T) {
	r, dir := newTestRecorder(t, Config{})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "cli:" + string(rune('a'+i))
			turn := r.Begin(key, "user", "")
			turn.Tool("read_file", nil, time.Millisecond, 1, false)
			r.Charge(key, ModelCall{Model: "m", Input: 10, Output: 1})
			turn.End("ok", nil)
		}(i)
	}
	wg.Wait()

	recs, err := Since(dir, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 12 {
		t.Errorf("read %d records, want 12", len(recs))
	}
	for _, rec := range recs {
		if rec.InputTokens != 10 || len(rec.Tools) != 1 {
			t.Errorf("record lost detail: %+v", rec)
		}
	}
}

func TestSinceSkipsRecordsBeforeTheCutoff(t *testing.T) {
	r, dir := newTestRecorder(t, Config{})
	turn := r.Begin("cli:x", "user", "")
	turn.End("ok", nil)

	if recs, _ := Since(dir, time.Now().Add(time.Hour)); len(recs) != 0 {
		t.Errorf("read %d records from the future", len(recs))
	}
}
