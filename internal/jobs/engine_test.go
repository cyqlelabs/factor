package jobs

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// waitFinished polls a job until it leaves the running state. Reading the
// state through Snapshot (which takes the job lock) is what makes the polling
// safe while the worker goroutine is still writing.
func waitFinished(t *testing.T, e *Engine, id string) View {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := e.Get(id)
		if !ok {
			t.Fatalf("job %s is no longer known to the engine", id)
		}
		if v := job.Snapshot(); v.State != StateRunning {
			return v
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job %s never left the running state", id)
	return View{}
}

// waitCount polls an atomic counter until it reaches want.
func waitCount(t *testing.T, counter *atomic.Int64, want int64, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if counter.Load() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s = %d, want %d", what, counter.Load(), want)
}

func TestSnapshotCopiesJobStateWhileRunningAndAfterFinishing(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	origin := Origin{Channel: "cli", ChatID: "1", SessionKey: "cli:1"}
	job, err := e.Start(KindExec, "sleeper", "sleep 30", origin)
	if err != nil {
		t.Fatal(err)
	}

	running := job.Snapshot()
	if running.ID != job.ID || running.Kind != KindExec || running.Description != "sleeper" || running.Origin != origin {
		t.Errorf("snapshot = %+v", running)
	}
	if running.State != StateRunning {
		t.Errorf("state = %s, want %s", running.State, StateRunning)
	}
	if running.Started.IsZero() {
		t.Error("Started is zero on a running job")
	}
	if !running.Finished.IsZero() {
		t.Errorf("Finished = %v on a running job, want zero", running.Finished)
	}

	if err := e.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	e.Wait()

	done := job.Snapshot()
	if done.State != StateCancelled {
		t.Errorf("state = %s, want %s", done.State, StateCancelled)
	}
	if done.Finished.IsZero() {
		t.Error("Finished is zero on a cancelled job")
	}
	if !done.Started.Equal(running.Started) {
		t.Error("the earlier snapshot changed with the job")
	}
}

func TestEngineWithoutANotifierStillFinishesJobs(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	job, err := e.Start(KindExec, "greet", "echo all done", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	e.Wait() // a nil notifier must not panic when the job completes
	if got := job.Snapshot().State; got != StateDone {
		t.Errorf("state = %s, want %s", got, StateDone)
	}
	if !strings.Contains(job.OutputTail(), "all done") {
		t.Errorf("output = %q", job.OutputTail())
	}
}

func TestPruneDropsTheOldestFinishedJobsAndKeepsRunningOnes(t *testing.T) {
	e := NewEngine(context.Background(), t.TempDir(), nil, nil)
	sleeper, err := e.Start(KindExec, "sleeper", "sleep 30", Origin{})
	if err != nil {
		t.Fatal(err)
	}

	// One job at a time: each is read back through Snapshot before the next
	// Start, so the engine never prunes while a worker is still writing state.
	var ids []string
	for range keepFinished + 5 {
		job, err := e.Start(KindExec, "", "true", Origin{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, job.ID)
		waitFinished(t, e, job.ID)
	}

	// Pruning runs on Start, while the newest job is still running, so the
	// engine settles at keepFinished older jobs plus the newest plus the sleeper.
	kept := keepFinished + 1
	if got, want := len(e.List()), kept+1; got != want {
		t.Fatalf("tracked jobs = %d, want %d", got, want)
	}
	for _, id := range ids[:len(ids)-kept] {
		if _, ok := e.Get(id); ok {
			t.Errorf("job %s should have been pruned", id)
		}
	}
	for _, id := range ids[len(ids)-kept:] {
		if _, ok := e.Get(id); !ok {
			t.Errorf("job %s should have been kept", id)
		}
	}
	if j, ok := e.Get(sleeper.ID); !ok {
		t.Error("the running job was pruned")
	} else if got := j.Snapshot().State; got != StateRunning {
		t.Errorf("sleeper state = %s, want %s", got, StateRunning)
	}

	if err := e.Cancel(sleeper.ID); err != nil {
		t.Fatal(err)
	}
	e.Wait()
}

func TestConcurrencyCapRunsFourAtOnceAndCompletesTheRest(t *testing.T) {
	const jobCount = maxConcurrent * 3
	release := make(chan struct{})
	var inFlight, peak atomic.Int64
	runTask := func(context.Context, string, string) (string, error) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		return "ok", nil
	}
	e := NewEngine(context.Background(), t.TempDir(), runTask, nil)

	var ids []string
	for range jobCount {
		job, err := e.Start(KindTask, "", "work", Origin{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, job.ID)
	}

	// Nothing can finish while release is open, so the runner holds exactly
	// the cap and the rest queue behind it.
	waitCount(t, &inFlight, maxConcurrent, "jobs in flight")
	close(release)
	e.Wait()

	if got := peak.Load(); got != maxConcurrent {
		t.Errorf("peak concurrency = %d, want %d", got, maxConcurrent)
	}
	for _, id := range ids {
		job, ok := e.Get(id)
		if !ok {
			t.Fatalf("job %s is no longer tracked", id)
		}
		if got := job.Snapshot().State; got != StateDone {
			t.Errorf("job %s state = %s, want %s", id, got, StateDone)
		}
	}
}

func TestQueuedJobIsCancelledWhenTheEngineContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	var inFlight atomic.Int64
	runTask := func(context.Context, string, string) (string, error) {
		inFlight.Add(1)
		<-release
		return "ok", nil
	}
	rec := newNotifyRecorder()
	e := NewEngine(ctx, t.TempDir(), runTask, rec.notify)

	for range maxConcurrent {
		if _, err := e.Start(KindTask, "", "work", Origin{}); err != nil {
			t.Fatal(err)
		}
	}
	waitCount(t, &inFlight, maxConcurrent, "jobs in flight")

	queued, err := e.Start(KindTask, "queued", "work", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	cancel() // no slot can free while release is open, so the job dies queued
	if got := waitFinished(t, e, queued.ID).State; got != StateCancelled {
		t.Errorf("queued job state = %s, want %s", got, StateCancelled)
	}
	close(release)
	e.Wait()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, n := range rec.jobs {
		if n.ID == queued.ID {
			t.Error("a cancelled job sent a completion notification")
		}
	}
}

func TestTaskJobRecordsRunnerErrors(t *testing.T) {
	rec := newNotifyRecorder()
	runTask := func(context.Context, string, string) (string, error) {
		return "", errors.New("delegate exploded")
	}
	e := NewEngine(context.Background(), t.TempDir(), runTask, rec.notify)
	if _, err := e.Start(KindTask, "research", "dig", Origin{}); err != nil {
		t.Fatal(err)
	}

	done := rec.wait(t)
	if done.State != StateFailed {
		t.Errorf("state = %s, want %s", done.State, StateFailed)
	}
	tail := done.OutputTail()
	if !strings.Contains(tail, "error: delegate exploded") {
		t.Errorf("runner error missing from tail: %q", tail)
	}
	if !strings.Contains(tail, "[delegate exploded]") {
		t.Errorf("finish did not append the error: %q", tail)
	}
	e.Wait()
}
