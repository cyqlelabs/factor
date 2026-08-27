package cron

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

func newService(t *testing.T, handler Handler, deliver Deliver) *Service {
	t.Helper()
	s, err := NewService(t.TempDir(), handler, deliver)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAddValidatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	s, err := NewService(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("not a cron", "msg", "telegram", "1"); err == nil {
		t.Error("invalid schedule accepted")
	}
	if _, err := s.Add("*/5 * * * *", "", "telegram", "1"); err == nil {
		t.Error("empty message accepted")
	}
	job, err := s.Add("*/5 * * * *", "check the build", "telegram", "42")
	if err != nil {
		t.Fatal(err)
	}
	if job.ID == "" || !job.Enabled {
		t.Errorf("job = %+v", job)
	}

	// reload from disk
	s2, err := NewService(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs := s2.List()
	if len(jobs) != 1 || jobs[0].Message != "check the build" || jobs[0].ChatID != "42" {
		t.Fatalf("reloaded = %+v", jobs)
	}
	// seq continues after reload — no ID collisions
	j2, err := s2.Add("0 9 * * *", "morning brief", "telegram", "42")
	if err != nil {
		t.Fatal(err)
	}
	if j2.ID == jobs[0].ID {
		t.Error("duplicate job ID after reload")
	}
}

func TestDueJobsWithFakeClock(t *testing.T) {
	s := newService(t, nil, nil)
	base := time.Date(2026, 1, 1, 8, 59, 30, 0, time.UTC)
	s.now = func() time.Time { return base }
	if _, err := s.Add("0 9 * * *", "morning", "telegram", "1"); err != nil {
		t.Fatal(err)
	}

	if due := s.dueJobs(); len(due) != 0 {
		t.Errorf("job due before schedule: %+v", due)
	}
	next, found := s.nextWake()
	if !found || next.Hour() != 9 || next.Minute() != 0 {
		t.Errorf("nextWake = %v %v", next, found)
	}

	base = time.Date(2026, 1, 1, 9, 0, 5, 0, time.UTC)
	due := s.dueJobs()
	if len(due) != 1 || due[0].Message != "morning" {
		t.Fatalf("due = %+v", due)
	}
	// not due twice for the same tick
	if due := s.dueJobs(); len(due) != 0 {
		t.Errorf("job fired twice: %+v", due)
	}
	// disabled jobs never due
	if err := s.SetEnabled(s.List()[0].ID, false); err != nil {
		t.Fatal(err)
	}
	base = base.Add(24 * time.Hour)
	if due := s.dueJobs(); len(due) != 0 {
		t.Errorf("disabled job fired: %+v", due)
	}
}

func TestRunJobDelivers(t *testing.T) {
	var mu sync.Mutex
	var delivered []string
	handler := func(_ context.Context, job Job) (string, error) {
		return "result for " + job.Message, nil
	}
	deliver := func(channel, chatID, content string) {
		mu.Lock()
		delivered = append(delivered, channel+"|"+chatID+"|"+content)
		mu.Unlock()
	}
	s := newService(t, handler, deliver)
	s.runJob(context.Background(), Job{ID: "cron-1", Message: "brief", Channel: "telegram", ChatID: "42"})
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 1 || delivered[0] != "telegram|42|result for brief" {
		t.Errorf("delivered = %v", delivered)
	}
}

func TestCronTool(t *testing.T) {
	s := newService(t, nil, nil)
	tool := &Tool{Service: s}
	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{Channel: "telegram", ChatID: "77"})

	res := tool.Execute(ctx, map[string]any{"action": "add", "schedule": "0 8 * * 1", "message": "weekly report"})
	if res.IsError {
		t.Fatalf("add: %+v", res)
	}
	jobs := s.List()
	if len(jobs) != 1 || jobs[0].Channel != "telegram" || jobs[0].ChatID != "77" {
		t.Fatalf("tool did not capture origin: %+v", jobs)
	}

	res = tool.Execute(ctx, map[string]any{"action": "list"})
	if !strings.Contains(res.ForLLM, "weekly report") {
		t.Errorf("list = %+v", res)
	}
	res = tool.Execute(ctx, map[string]any{"action": "disable", "id": jobs[0].ID})
	if res.IsError || s.List()[0].Enabled {
		t.Errorf("disable failed: %+v", res)
	}
	res = tool.Execute(ctx, map[string]any{"action": "remove", "id": jobs[0].ID})
	if res.IsError || len(s.List()) != 0 {
		t.Errorf("remove failed: %+v", res)
	}
}

func TestRunLoopFiresAndWakes(t *testing.T) {
	fired := make(chan string, 4)
	handler := func(_ context.Context, job Job) (string, error) {
		fired <- job.ID
		return "", nil
	}
	s := newService(t, handler, nil)
	// every-minute job, clock pinned just before the tick
	base := time.Date(2026, 1, 1, 10, 0, 59, 900_000_000, time.UTC)
	var mu sync.Mutex
	s.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return base
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Waited for, not just cancelled: a scheduler goroutine that outlives its
	// test keeps reading the package's pacing while the next test writes it.
	stopped := make(chan struct{})
	go func() { defer close(stopped); s.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after its context was cancelled")
		}
	}()
	time.Sleep(20 * time.Millisecond)

	if _, err := s.Add("* * * * *", "tick", "", ""); err != nil { // wakes the loop
		t.Fatal(err)
	}
	mu.Lock()
	base = base.Add(200 * time.Millisecond) // now past the minute boundary
	mu.Unlock()

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("cron job never fired")
	}
}

// A missed run caught up seconds before the next boundary must count as the
// boundary's run, not fire again when the boundary arrives one second later
// (github.com/cyqlelabs/factor/issues/15).
func TestCatchUpBeforeBoundaryFiresOnce(t *testing.T) {
	s := newService(t, nil, nil)
	base := time.Date(2026, 8, 21, 7, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	if _, err := s.Add("0 8 * * *", "daily report", "telegram", "1"); err != nil {
		t.Fatal(err)
	}
	base = time.Date(2026, 8, 21, 8, 0, 1, 0, time.UTC)
	if due := s.dueJobs(); len(due) != 1 {
		t.Fatalf("baseline run: due = %+v", due)
	}
	// The gateway is down all of Aug 22 and restarts one second before the
	// Aug 23 boundary: the missed run catches up now, and the boundary owes
	// nothing more.
	base = time.Date(2026, 8, 23, 7, 59, 59, 0, time.UTC)
	if due := s.dueJobs(); len(due) != 1 {
		t.Fatalf("catch-up run: due = %+v", due)
	}
	base = time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC)
	if due := s.dueJobs(); len(due) != 0 {
		t.Errorf("job fired twice within 1s of its catch-up: %+v", due)
	}
	// The next day's run is still owed on time.
	base = time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	if due := s.dueJobs(); len(due) != 1 {
		t.Errorf("next scheduled run lost to the dedupe: due = %+v", due)
	}
}

// The catch-up dedupe must not eat an every-minute schedule, whose on-time
// ticks are always less than a minute from the next.
func TestMinutelyScheduleStillFiresEveryMinute(t *testing.T) {
	s := newService(t, nil, nil)
	base := time.Date(2026, 8, 23, 7, 59, 30, 0, time.UTC)
	s.now = func() time.Time { return base }
	if _, err := s.Add("* * * * *", "every minute", "telegram", "1"); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		base = time.Date(2026, 8, 23, 8, i, 0, 100_000_000, time.UTC)
		if due := s.dueJobs(); len(due) != 1 {
			t.Fatalf("minute %d: due = %+v", i, due)
		}
	}
}
