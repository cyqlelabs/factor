package cron

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// brokenPath returns a jobs.json path inside a directory that does not exist,
// so every save() fails with ENOENT regardless of the uid running the tests.
func brokenPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "missing-dir", "jobs.json")
}

func TestNewServiceFailsWhenDirCannotBeCreated(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(filepath.Join(blocker, "cron"), nil, nil); err == nil {
		t.Fatal("NewService succeeded with a regular file as the parent directory")
	}
}

func TestNewServiceRejectsMalformedStore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "jobs.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewService(dir, nil, nil)
	if err == nil {
		t.Fatal("NewService accepted a jobs.json containing malformed JSON")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should name the failing file, got %v", err)
	}
}

func TestNewServiceReportsUnreadableStore(t *testing.T) {
	dir := t.TempDir()
	// A directory where jobs.json belongs makes ReadFile fail with something
	// other than "not exist", which must surface rather than be ignored.
	if err := os.Mkdir(filepath.Join(dir, "jobs.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(dir, nil, nil); err == nil {
		t.Fatal("NewService succeeded although jobs.json could not be read")
	}
}

func TestLoadRestoresSeqAndJobs(t *testing.T) {
	dir := t.TempDir()
	stored := struct {
		Seq  int    `json:"seq"`
		Jobs []*Job `json:"jobs"`
	}{
		Seq: 7,
		Jobs: []*Job{
			{ID: "cron-6", Schedule: "0 9 * * *", Message: "six", Channel: "telegram", ChatID: "1", Enabled: true},
			{ID: "cron-7", Schedule: "0 10 * * *", Message: "seven", Enabled: false},
		},
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jobs.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := NewService(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs := s.List()
	if len(jobs) != 2 {
		t.Fatalf("loaded jobs = %+v", jobs)
	}
	if jobs[0].ID != "cron-6" || jobs[0].Message != "six" || !jobs[0].Enabled {
		t.Errorf("first job not restored: %+v", jobs[0])
	}
	if jobs[1].ID != "cron-7" || jobs[1].Enabled {
		t.Errorf("disabled state not restored: %+v", jobs[1])
	}
	next, err := s.Add("* * * * *", "eight", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "cron-8" {
		t.Errorf("seq not restored: next ID = %q, want cron-8", next.ID)
	}
}

func TestAddReportsSaveFailure(t *testing.T) {
	s := newService(t, nil, nil)
	s.path = brokenPath(t)
	if _, err := s.Add("* * * * *", "msg", "telegram", "1"); err == nil {
		t.Fatal("Add reported success although the store could not be written")
	}
}

func TestRemoveAndSetEnabledRejectUnknownID(t *testing.T) {
	s := newService(t, nil, nil)
	err := s.Remove("cron-404")
	if err == nil {
		t.Fatal("Remove accepted an unknown job id")
	}
	if !strings.Contains(err.Error(), "cron-404") {
		t.Errorf("error should name the missing job, got %v", err)
	}
	if err := s.SetEnabled("cron-404", true); err == nil {
		t.Fatal("SetEnabled accepted an unknown job id")
	}
}

func TestRemoveAndSetEnabledReportSaveFailure(t *testing.T) {
	s := newService(t, nil, nil)
	job, err := s.Add("0 9 * * *", "morning", "telegram", "1")
	if err != nil {
		t.Fatal(err)
	}
	s.path = brokenPath(t)
	if err := s.SetEnabled(job.ID, false); err == nil {
		t.Error("SetEnabled reported success although the store could not be written")
	}
	if err := s.Remove(job.ID); err == nil {
		t.Error("Remove reported success although the store could not be written")
	}
}

func TestNextWakeWithoutEnabledJobs(t *testing.T) {
	s := newService(t, nil, nil)
	if _, found := s.nextWake(); found {
		t.Error("nextWake found a tick with no jobs at all")
	}
	job, err := s.Add("0 9 * * *", "morning", "telegram", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnabled(job.ID, false); err != nil {
		t.Fatal(err)
	}
	if next, found := s.nextWake(); found {
		t.Errorf("nextWake scheduled a disabled job for %v", next)
	}
}

func TestNextWakeSkipsUnparsableSchedule(t *testing.T) {
	s := newService(t, nil, nil)
	// Add validates, so a corrupt schedule can only arrive from a hand-edited
	// or older jobs.json; it must be skipped rather than crash the scheduler.
	s.jobs["cron-bad"] = &Job{ID: "cron-bad", Schedule: "not a schedule", Message: "x", Enabled: true}
	if _, found := s.nextWake(); found {
		t.Error("nextWake scheduled a job with an unparsable schedule")
	}

	if _, err := s.Add("0 9 * * *", "morning", "telegram", "1"); err != nil {
		t.Fatal(err)
	}
	next, found := s.nextWake()
	if !found || next.Hour() != 9 {
		t.Errorf("valid job ignored alongside a corrupt one: %v %v", next, found)
	}
}

func TestDueJobsSkipsUnparsableSchedule(t *testing.T) {
	s := newService(t, nil, nil)
	s.now = func() time.Time { return time.Date(2026, 1, 1, 9, 0, 5, 0, time.UTC) }
	s.jobs["cron-bad"] = &Job{ID: "cron-bad", Schedule: "not a schedule", Message: "x", Enabled: true}
	if due := s.dueJobs(); len(due) != 0 {
		t.Errorf("job with an unparsable schedule fired: %+v", due)
	}
}

func TestDueJobsStillFiresWhenSaveFails(t *testing.T) {
	s := newService(t, nil, nil)
	base := time.Date(2026, 1, 1, 8, 59, 30, 0, time.UTC)
	s.now = func() time.Time { return base }
	if _, err := s.Add("0 9 * * *", "morning", "telegram", "1"); err != nil {
		t.Fatal(err)
	}
	s.path = brokenPath(t)
	base = time.Date(2026, 1, 1, 9, 0, 5, 0, time.UTC)

	due := s.dueJobs()
	if len(due) != 1 || due[0].Message != "morning" {
		t.Fatalf("a failed LastRun save must not swallow the due job, got %+v", due)
	}
}

func TestRunReturnsOnContextCancellation(t *testing.T) {
	s := newService(t, nil, nil)
	// A far-future job keeps Run parked in its select instead of spinning.
	if _, err := s.Add("0 0 1 1 *", "new year", "telegram", "1"); err != nil {
		t.Fatal(err)
	}
	// Add left a wake token behind; drain it so the loop cannot take the notify
	// branch and leave through the ctx.Err() loop condition instead.
	select {
	case <-s.notify:
	default:
	}
	// nextWake consults the clock once per loop iteration, so this signal means
	// Run is committed to reaching the select, where ctx.Done is the only case
	// that can become ready.
	parked := make(chan struct{}, 1)
	s.now = func() time.Time {
		select {
		case parked <- struct{}{}:
		default:
		}
		return time.Now()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	select {
	case <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("Run never reached its scheduling loop")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestRunJobDeliversHandlerFailure(t *testing.T) {
	var mu sync.Mutex
	var delivered []string
	s := newService(t,
		func(context.Context, Job) (string, error) { return "", errors.New("model unavailable") },
		func(channel, chatID, content string) {
			mu.Lock()
			defer mu.Unlock()
			delivered = append(delivered, content)
		},
	)
	s.runJob(context.Background(), Job{ID: "cron-1", Message: "brief", Channel: "telegram", ChatID: "42"})

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 1 {
		t.Fatalf("handler failure not reported to the user: %v", delivered)
	}
	if !strings.Contains(delivered[0], "cron-1") || !strings.Contains(delivered[0], "model unavailable") {
		t.Errorf("failure notice = %q, want the job id and the cause", delivered[0])
	}
}

func TestRunJobStaysSilentOnEmptyResult(t *testing.T) {
	var mu sync.Mutex
	var delivered []string
	record := func(channel, chatID, content string) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, content)
	}

	s := newService(t, func(context.Context, Job) (string, error) { return "", nil }, record)
	s.runJob(context.Background(), Job{ID: "cron-1", Channel: "telegram", ChatID: "42"})

	// A job with no channel has nowhere to deliver, even with a real result.
	s2 := newService(t, func(context.Context, Job) (string, error) { return "something to say", nil }, record)
	s2.runJob(context.Background(), Job{ID: "cron-2"})

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 0 {
		t.Errorf("silent job delivered: %v", delivered)
	}
}

func TestRunJobWithoutDeliverDoesNotPanic(t *testing.T) {
	s := newService(t, func(context.Context, Job) (string, error) { return "result", nil }, nil)
	s.runJob(context.Background(), Job{ID: "cron-1", Channel: "telegram", ChatID: "42"})
}
