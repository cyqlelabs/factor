package jobs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type notifyRecorder struct {
	mu   sync.Mutex
	jobs []*Job
	ch   chan *Job
}

func newNotifyRecorder() *notifyRecorder {
	return &notifyRecorder{ch: make(chan *Job, 16)}
}

func (n *notifyRecorder) notify(j *Job) {
	n.mu.Lock()
	n.jobs = append(n.jobs, j)
	n.mu.Unlock()
	n.ch <- j
}

func (n *notifyRecorder) wait(t *testing.T) *Job {
	t.Helper()
	select {
	case j := <-n.ch:
		return j
	case <-time.After(5 * time.Second):
		t.Fatal("no completion notification")
		return nil
	}
}

func TestExecJobCompletesAndNotifies(t *testing.T) {
	rec := newNotifyRecorder()
	e := NewEngine(context.Background(), t.TempDir(), nil, rec.notify)
	origin := Origin{Channel: "telegram", ChatID: "42", SessionKey: "telegram:42"}

	job, err := e.Start(KindExec, "greet", "echo working on it", origin)
	if err != nil {
		t.Fatal(err)
	}
	done := rec.wait(t)
	if done.ID != job.ID || done.State != StateDone {
		t.Fatalf("done = %+v", done)
	}
	if !strings.Contains(done.OutputTail(), "working on it") {
		t.Errorf("output = %q", done.OutputTail())
	}
	if done.Origin != origin {
		t.Errorf("origin = %+v", done.Origin)
	}
}

func TestExecJobFailureState(t *testing.T) {
	rec := newNotifyRecorder()
	e := NewEngine(context.Background(), t.TempDir(), nil, rec.notify)
	_, err := e.Start(KindExec, "", payloadFailErr, Origin{})
	if err != nil {
		t.Fatal(err)
	}
	done := rec.wait(t)
	if done.State != StateFailed {
		t.Errorf("state = %s", done.State)
	}
	if !strings.Contains(done.OutputTail(), "oops") {
		t.Errorf("stderr not captured: %q", done.OutputTail())
	}
}

func TestTaskJobRunsDelegate(t *testing.T) {
	rec := newNotifyRecorder()
	runTask := func(_ context.Context, prompt, sessionKey string) (string, error) {
		if prompt != "research something" || !strings.HasPrefix(sessionKey, "job:") {
			t.Errorf("prompt=%q session=%q", prompt, sessionKey)
		}
		return "research complete: 42", nil
	}
	e := NewEngine(context.Background(), t.TempDir(), runTask, rec.notify)
	if _, err := e.Start(KindTask, "research", "research something", Origin{}); err != nil {
		t.Fatal(err)
	}
	done := rec.wait(t)
	if done.State != StateDone || !strings.Contains(done.OutputTail(), "research complete: 42") {
		t.Errorf("done = %s output=%q", done.State, done.OutputTail())
	}
}

func TestCancelRunningJob(t *testing.T) {
	rec := newNotifyRecorder()
	e := NewEngine(context.Background(), t.TempDir(), nil, rec.notify)
	job, err := e.Start(KindExec, "long", payloadSleep, Origin{})
	if err != nil {
		t.Fatal(err)
	}
	// wait for it to actually start
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ := e.Get(job.ID); j.State == StateRunning && !j.Started.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	if err := e.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if time.Since(start) > 8*time.Second {
		t.Fatal("cancel did not kill the process group promptly")
	}
	j, _ := e.Get(job.ID)
	if j.State != StateCancelled {
		t.Errorf("state = %s", j.State)
	}
	// cancelled jobs must NOT notify (user asked for the cancel)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, n := range rec.jobs {
		if n.ID == job.ID {
			t.Error("cancelled job sent a notification")
		}
	}
}

func TestOutputTailBounded(t *testing.T) {
	rec := newNotifyRecorder()
	e := NewEngine(context.Background(), t.TempDir(), nil, rec.notify)
	if _, err := e.Start(KindExec, "", payloadFlood, Origin{}); err != nil {
		t.Fatal(err)
	}
	done := rec.wait(t)
	if len(done.OutputTail()) > tailBytes {
		t.Errorf("tail = %d bytes, cap %d", len(done.OutputTail()), tailBytes)
	}
}

func TestListOrdersAndPrunes(t *testing.T) {
	rec := newNotifyRecorder()
	e := NewEngine(context.Background(), t.TempDir(), nil, rec.notify)
	for range 3 {
		if _, err := e.Start(KindExec, "", payloadOK, Origin{}); err != nil {
			t.Fatal(err)
		}
	}
	e.Wait()
	if got := len(e.List()); got != 3 {
		t.Errorf("list = %d", got)
	}
}
